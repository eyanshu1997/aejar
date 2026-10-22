package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/discordbot"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type fakeChat struct {
	mu      sync.Mutex
	threads map[string]string // id -> name
	msgs    map[string][]discordbot.Message
	n       int
}

func (f *fakeChat) CreateThread(_ context.Context, name, first string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := "th" + string(rune('0'+f.n))
	f.threads[id] = name
	f.msgs[id] = append(f.msgs[id], discordbot.Message{Text: first})
	return id, nil
}

func (f *fakeChat) Send(_ context.Context, thread string, m discordbot.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs[thread] = append(f.msgs[thread], m)
	return nil
}

func (f *fakeChat) find(thread, substr string) *discordbot.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.msgs[thread] {
		if strings.Contains(m.Text, substr) {
			return &f.msgs[thread][i]
		}
	}
	return nil
}

type tokenCreds string

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}
func (tokenCreds) RequireTransportSecurity() bool { return false }

func TestDiscordThreadPerTask(t *testing.T) {
	e := setup(t)
	conn, err := grpc.NewClient(strings.TrimPrefix(e.url, "http://"),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(tokenCreds(e.owner)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	chat := &fakeChat{threads: map[string]string{}, msgs: map[string][]discordbot.Message{}}
	const owner = "111"
	bot := &discordbot.Bot{
		API: discordbot.API{
			Tasks: aejarv1.NewTaskServiceClient(conn), Approvals: aejarv1.NewApprovalServiceClient(conn),
			Audit: aejarv1.NewAuditServiceClient(conn), Ops: aejarv1.NewOpsServiceClient(conn),
		},
		Chat: chat, OwnerID: owner, PublicURL: "https://aejar.example.com",
		StatePath: filepath.Join(t.TempDir(), "discord.json"), ReplayHistory: true,
	}
	ctx := context.Background()
	if err := bot.Load(ctx); err != nil {
		t.Fatal(err)
	}

	// A stranger cannot use commands.
	if r := bot.Command(ctx, "999", "task", map[string]string{"title": "x", "prompt": "y"}); !strings.Contains(r, "Only the owner") {
		t.Fatalf("stranger command: %q", r)
	}
	// /task creates the task; the poll opens its thread with the prompt.
	r := bot.Command(ctx, owner, "task", map[string]string{"title": "Find a laptop", "prompt": "compare 3 laptops under 80k", "profile": "research"})
	if !strings.Contains(r, "Created") {
		t.Fatalf("task command: %q", r)
	}
	if err := bot.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(chat.threads) != 1 {
		t.Fatalf("threads: %v", chat.threads)
	}
	var thread string
	for id := range chat.threads {
		thread = id
	}
	if chat.find(thread, "compare 3 laptops under 80k") == nil {
		t.Fatal("thread does not show the prompt")
	}

	// The session runs, replies and asks for approval: all in its thread.
	e.tick()
	var sess, taskID string
	for _, c := range e.rec.Calls() {
		if c.Op == "start" {
			sess, taskID = c.Spec.SessionToken, c.TaskID
		}
	}
	e.must("POST", "/hooks/claude/stop", sess, map[string]any{"last_assistant_message": "Here are 3 options: A, B, C."})
	a := e.must("POST", "/v1/approvals", sess, map[string]any{"class": "APPROVAL_CLASS_QUESTION", "summary": "Should I also check refurbished?"})
	if err := bot.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if chat.find(thread, "Here are 3 options") == nil {
		t.Fatal("session reply not posted in its thread")
	}
	card := chat.find(thread, "Approval needed")
	if card == nil || len(card.Buttons) != 2 || card.Buttons[0].ID != "approve:"+a["id"].(string) {
		t.Fatalf("approval card: %+v", card)
	}

	// Buttons: strangers are refused; the owner's click decides.
	if r := bot.Click(ctx, "999", card.Buttons[0].ID); !strings.Contains(r, "Only the owner") {
		t.Fatalf("stranger click: %q", r)
	}
	if r := bot.Click(ctx, owner, card.Buttons[0].ID); !strings.Contains(r, "approved") {
		t.Fatalf("owner click: %q", r)
	}
	if err := bot.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if chat.find(thread, "✅ approved") == nil {
		t.Fatal("decision not posted")
	}

	// The owner's reply in the thread reaches the session; others are ignored.
	if _, ok := bot.ThreadMessage(ctx, "999", thread, "do something else"); ok {
		t.Fatal("stranger message forwarded")
	}
	if reply, ok := bot.ThreadMessage(ctx, owner, thread, "also check battery life"); !ok || !strings.Contains(reply, "📨") {
		t.Fatalf("owner message: %q %v", reply, ok)
	}
	var queued bool
	for _, ev := range e.must("GET", "/v1/audit?subject="+taskID+"&limit=200", e.owner, nil)["events"].([]any) {
		m := ev.(map[string]any)
		if m["action"] == "event.queued" && m["payload"].(map[string]any)["text"] == "also check battery life" {
			queued = true
		}
	}
	if !queued {
		t.Fatal("thread reply did not reach the task")
	}

	// A restarted bot continues where it left off: no duplicate threads.
	bot2 := &discordbot.Bot{API: bot.API, Chat: chat, OwnerID: owner, StatePath: bot.StatePath}
	if err := bot2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bot2.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(chat.threads) != 1 {
		t.Fatalf("restart duplicated threads: %v", chat.threads)
	}
}
