// Package discordbot is aejar's Discord front end. It is deterministic: it
// follows aejard's audit log and turns events into messages, one thread per
// task, and turns the owner's commands, buttons and thread replies into
// aejard API calls. No model is involved.
package discordbot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
)

// Chat is the part of Discord the bot uses. The discordgo adapter implements
// it; tests use a fake.
type Chat interface {
	// CreateThread opens a public thread in the bot's channel and returns its id.
	CreateThread(ctx context.Context, name, firstMessage string) (string, error)
	Send(ctx context.Context, threadID string, m Message) error
}

// Message is one Discord message.
type Message struct {
	Text string
	// Code is shown in a code block under Text (payloads).
	Code string
	// Buttons are rendered as a row of buttons.
	Buttons []Button
}

// Button is a message button; ID comes back in a click.
type Button struct {
	ID, Label string
	Danger    bool
}

// API is the slice of aejard the bot calls.
type API struct {
	Tasks     aejarv1.TaskServiceClient
	Approvals aejarv1.ApprovalServiceClient
	Audit     aejarv1.AuditServiceClient
	Ops       aejarv1.OpsServiceClient
}

// State survives restarts: where the bot is in the audit log, and which
// thread belongs to which task.
type State struct {
	LastSeq int64             `json:"last_seq"`
	Threads map[string]string `json:"threads"` // task id -> thread id
}

// Bot is the deterministic core.
type Bot struct {
	API       API
	Chat      Chat
	OwnerID   string
	PublicURL string // e.g. https://aejar.example.com, for links
	StatePath string
	// ReplayHistory makes a bot with no state start from the first audit
	// event instead of the current end (tests).
	ReplayHistory bool

	mu    sync.Mutex
	state State
}

// Load reads the state file; a missing file starts from the current end of
// the audit log so a new bot does not replay history.
func (b *Bot) Load(ctx context.Context) error {
	b.state = State{Threads: map[string]string{}}
	data, err := os.ReadFile(b.StatePath)
	if err == nil {
		if err := json.Unmarshal(data, &b.state); err != nil {
			return fmt.Errorf("state: %w", err)
		}
		if b.state.Threads == nil {
			b.state.Threads = map[string]string{}
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if !b.ReplayHistory {
		v, err := b.API.Audit.VerifyAudit(ctx, &aejarv1.VerifyAuditRequest{})
		if err != nil {
			return err
		}
		b.state.LastSeq = v.Events
	}
	return b.save()
}

func (b *Bot) save() error {
	data, _ := json.MarshalIndent(b.state, "", "  ")
	tmp := b.StatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.StatePath)
}

// Run follows the audit log until ctx ends.
func (b *Bot) Run(ctx context.Context, every time.Duration) {
	for {
		if err := b.Poll(ctx); err != nil {
			log.Printf("discord poll: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Poll handles new audit events once.
func (b *Bot) Poll(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	resp, err := b.API.Audit.ListAuditEvents(ctx, &aejarv1.ListAuditEventsRequest{AfterSeq: b.state.LastSeq, Limit: 200})
	if err != nil {
		return err
	}
	for _, ev := range resp.Events {
		if err := b.handle(ctx, ev); err != nil {
			// Stop here; the event is retried on the next poll.
			return fmt.Errorf("event %d %s: %w", ev.Seq, ev.Action, err)
		}
		b.state.LastSeq = ev.Seq
		if err := b.save(); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) handle(ctx context.Context, ev *aejarv1.AuditEvent) error {
	p := ev.Payload.AsMap()
	str := func(k string) string { s, _ := p[k].(string); return s }
	task := ev.Subject

	switch ev.Action {
	case "task.created":
		t, err := b.API.Tasks.GetTask(ctx, &aejarv1.GetTaskRequest{Id: task})
		if err != nil {
			return err
		}
		first := fmt.Sprintf("**%s** · `%s` · %s · %s", t.Title, t.Id, short(t.Class.String()), t.Profile)
		if t.Prompt != "" {
			first += "\n> " + strings.ReplaceAll(clip(t.Prompt, 1500), "\n", "\n> ")
		}
		first += "\nReply in this thread to talk to the session. " + b.link(t.Id)
		id, err := b.Chat.CreateThread(ctx, clip(t.Title, 80)+" · "+t.Id, first)
		if err != nil {
			return err
		}
		b.state.Threads[task] = id
		return nil
	case "task.started":
		if p["resume"] == true {
			return b.post(ctx, task, Message{Text: "▶️ session resumed"})
		}
		return b.post(ctx, task, Message{Text: "▶️ session started"})
	case "task.stopped":
		if short(str("to")) == "done" {
			return b.post(ctx, task, Message{Text: "🏁 done (" + str("why") + "). Reply here to reopen it."})
		}
		return b.post(ctx, task, Message{Text: fmt.Sprintf("⏸️ session stopped → %s: %s", short(str("to")), str("why"))})
	case "task.complete_requested":
		return b.post(ctx, task, Message{Text: "📝 " + clip(str("summary"), 1500)})
	case "task.reopened":
		return b.post(ctx, task, Message{Text: "🔁 reopened"})
	case "task.start_failed":
		return b.post(ctx, task, Message{Text: "⚠️ could not start the session: " + clip(str("error"), 500)})
	case "task.woken":
		return b.post(ctx, task, Message{Text: "↩️ back in the queue"})
	case "session.idle", "session.reply":
		if r := str("reply"); r != "" {
			return b.post(ctx, task, Message{Text: "💬 " + r})
		}
	case "session.error":
		return b.post(ctx, task, Message{Text: fmt.Sprintf("⚠️ session error: %s %s", str("error"), clip(str("message"), 300))})
	case "session.sensitive":
		return b.post(ctx, task, Message{Text: "🔒 read private data; web access is off for the rest of this session"})
	case "tool.denied":
		return b.post(ctx, task, Message{Text: fmt.Sprintf("⛔ blocked %s: %s", str("tool"), clip(str("reason"), 300))})
	case "approval.requested":
		a, err := b.API.Approvals.GetApproval(ctx, &aejarv1.GetApprovalRequest{Id: ev.Subject})
		if err != nil {
			return err
		}
		return b.post(ctx, a.TaskId, approvalMessage(a))
	case "approval.decided":
		verdict := "❌ rejected"
		if p["approved"] == true {
			verdict = "✅ approved"
		}
		if r := str("reason"); r != "" {
			verdict += " (" + r + ")"
		}
		return b.post(ctx, str("task"), Message{Text: fmt.Sprintf("%s `%s` by %s", verdict, ev.Subject, ev.Actor)})
	case "limit.hit":
		return b.post(ctx, task, Message{Text: "⏳ usage limit reached; new work waits until " + str("blocked_until")})
	}
	return nil
}

// post sends to a task's thread, creating one if the task predates the bot.
func (b *Bot) post(ctx context.Context, task string, m Message) error {
	if task == "" {
		return nil
	}
	thread, ok := b.state.Threads[task]
	if !ok {
		t, err := b.API.Tasks.GetTask(ctx, &aejarv1.GetTaskRequest{Id: task})
		if err != nil {
			return err
		}
		thread, err = b.Chat.CreateThread(ctx, clip(t.Title, 80)+" · "+t.Id, fmt.Sprintf("**%s** · `%s` %s", t.Title, t.Id, b.link(t.Id)))
		if err != nil {
			return err
		}
		b.state.Threads[task] = thread
	}
	for _, part := range split(m.Text, 1900) {
		mm := m
		mm.Text = part
		if err := b.Chat.Send(ctx, thread, mm); err != nil {
			return err
		}
		mm.Code, mm.Buttons = "", nil
		m = mm
	}
	return nil
}

func approvalMessage(a *aejarv1.Approval) Message {
	payload, _ := json.MarshalIndent(a.Payload.AsMap(), "", "  ")
	text := fmt.Sprintf("🟠 **Approval needed** `%s` (%s)\n%s", a.Id, short(a.Class.String()), a.Summary)
	if a.ToolName != "" {
		text += "\nExact call: `" + a.ToolName + "`"
	}
	return Message{
		Text: text,
		Code: clip(string(payload), 1500),
		Buttons: []Button{
			{ID: "approve:" + a.Id, Label: "Approve exactly this"},
			{ID: "reject:" + a.Id, Label: "Reject", Danger: true},
		},
	}
}

// Click handles a button. Only the owner may decide; the decision carries the
// hash of the approval as it is now, which is the payload shown on the card
// (approvals never change after creation).
func (b *Bot) Click(ctx context.Context, userID, buttonID string) string {
	if userID != b.OwnerID {
		return "Only the owner can do that."
	}
	verb, id, ok := strings.Cut(buttonID, ":")
	if !ok || (verb != "approve" && verb != "reject") {
		return "Unknown button."
	}
	a, err := b.API.Approvals.GetApproval(ctx, &aejarv1.GetApprovalRequest{Id: id})
	if err != nil {
		return "Error: " + err.Error()
	}
	reason := ""
	if verb == "reject" {
		reason = "rejected from Discord"
	}
	if _, err := b.API.Approvals.DecideApproval(ctx, &aejarv1.DecideApprovalRequest{
		Id: id, Approve: verb == "approve", Reason: reason, PayloadHash: a.PayloadHash,
	}); err != nil {
		return "Error: " + err.Error()
	}
	return "Done: " + verb + "d " + id
}

// ThreadMessage forwards the owner's reply in a task thread to the session.
func (b *Bot) ThreadMessage(ctx context.Context, userID, threadID, text string) (string, bool) {
	if userID != b.OwnerID || strings.TrimSpace(text) == "" {
		return "", false
	}
	b.mu.Lock()
	task := ""
	for t, th := range b.state.Threads {
		if th == threadID {
			task = t
		}
	}
	b.mu.Unlock()
	if task == "" {
		return "", false
	}
	r, err := b.API.Tasks.SendToTask(ctx, &aejarv1.SendToTaskRequest{Id: task, Text: text})
	if err != nil {
		return "Error: " + err.Error(), true
	}
	if r.Delivered {
		return "📨 sent to the session", true
	}
	return "📨 queued; delivered when the session runs", true
}

// Command handles a slash command. Only the owner may use them.
func (b *Bot) Command(ctx context.Context, userID, name string, opts map[string]string) string {
	if userID != b.OwnerID {
		return "Only the owner can use aejar."
	}
	switch name {
	case "task":
		class := map[string]aejarv1.TaskClass{"p0": aejarv1.TaskClass_TASK_CLASS_P0, "p1": aejarv1.TaskClass_TASK_CLASS_P1,
			"p2": aejarv1.TaskClass_TASK_CLASS_P2, "p3": aejarv1.TaskClass_TASK_CLASS_P3}[opts["class"]]
		t, err := b.API.Tasks.CreateTask(ctx, &aejarv1.CreateTaskRequest{
			Title: opts["title"], Prompt: opts["prompt"], Class: class, Profile: opts["profile"], Model: opts["model"],
		})
		if err != nil {
			return "Error: " + err.Error()
		}
		return fmt.Sprintf("Created `%s`; its thread opens in a moment. %s", t.Id, b.link(t.Id))
	case "status":
		st, err := b.API.Ops.GetStatus(ctx, &aejarv1.GetStatusRequest{})
		if err != nil {
			return "Error: " + err.Error()
		}
		s := fmt.Sprintf("cap %d · running %d · queued %d · waiting %d · approvals %d", st.Cap, st.Running, st.Queued, st.Waiting, st.PendingApprovals)
		if st.LimitBlockedUntil != nil {
			s += " · usage limit until " + st.LimitBlockedUntil.AsTime().Local().Format("15:04")
		}
		return s
	case "tasks":
		resp, err := b.API.Tasks.ListTasks(ctx, &aejarv1.ListTasksRequest{})
		if err != nil {
			return "Error: " + err.Error()
		}
		var lines []string
		for i, t := range resp.Tasks {
			if i == 15 {
				break
			}
			lines = append(lines, fmt.Sprintf("`%s` %s · %s", t.Id, short(t.State.String()), clip(t.Title, 60)))
		}
		if len(lines) == 0 {
			return "No tasks."
		}
		return strings.Join(lines, "\n")
	case "finish":
		t, err := b.API.Tasks.FinishTask(ctx, &aejarv1.FinishTaskRequest{Id: opts["task"]})
		if err != nil {
			return "Error: " + err.Error()
		}
		return fmt.Sprintf("`%s` is %s.", t.Id, short(t.State.String()))
	case "screen":
		sc, err := b.API.Tasks.GetSessionScreen(ctx, &aejarv1.GetSessionScreenRequest{Id: opts["task"], HistoryLines: 0})
		if err != nil {
			return "Error: " + err.Error()
		}
		return "```\n" + clipTail(strings.TrimRight(sc.Text, " \n"), 1800) + "\n```"
	}
	return "Unknown command."
}

func (b *Bot) link(taskID string) string {
	if b.PublicURL == "" {
		return ""
	}
	return "<" + b.PublicURL + "/#task=" + taskID + ">"
}

func short(s string) string {
	for _, p := range []string{"TASK_STATE_", "TASK_CLASS_", "APPROVAL_CLASS_", "APPROVAL_STATE_"} {
		s = strings.TrimPrefix(s, p)
	}
	return strings.ToLower(s)
}

func clip(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func clipTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// split breaks text into Discord-sized chunks on line boundaries.
func split(s string, n int) []string {
	if len([]rune(s)) <= n {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		for len([]rune(line)) > n {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			r := []rune(line)
			out = append(out, string(r[:n]))
			line = string(r[n:])
		}
		if len([]rune(cur.String()))+len([]rune(line)) > n {
			out = append(out, cur.String())
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
