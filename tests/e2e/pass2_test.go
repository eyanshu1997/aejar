package e2e

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eyanshu1997/aejar/internal/auth"
	"github.com/eyanshu1997/aejar/internal/runner"
)

func (e *env) calls(op, taskID string) []runner.Call {
	var out []runner.Call
	for _, c := range e.rec.Calls() {
		if c.Op == op && c.TaskID == taskID {
			out = append(out, c)
		}
	}
	return out
}

func TestSayQueuesUntilChannelReady(t *testing.T) {
	e := setup(t)
	// Not running yet: queued.
	e.res.MemAvailableMB = 100
	id := e.must("POST", "/v1/tasks", e.owner, map[string]any{"title": "t"})["id"].(string)
	r := e.must("POST", "/v1/tasks/"+id+":say", e.owner, map[string]any{"text": "also check the logs"})
	if r["delivered"] == true {
		t.Fatal("delivered to a task that is not running")
	}
	// Running, but the channel is not up yet: still queued.
	e.rec.SetDeliverErr(errors.New("no socket"))
	e.res.MemAvailableMB = 2000
	e.tick()
	var sess string
	for _, c := range e.calls("start", id) {
		sess = c.Spec.SessionToken
	}
	if len(e.calls("deliver", id)) != 0 {
		t.Fatal("delivered before the channel was ready")
	}
	// Channel comes up: the queue flushes in order.
	e.rec.SetDeliverErr(nil)
	e.must("POST", "/v1/tasks/"+id+":say", e.owner, map[string]any{"text": "second", "kind": "comment"})
	e.must("POST", "/hooks/claude/channel-ready", sess, map[string]any{})
	got := e.calls("deliver", id)
	if len(got) != 2 || got[0].Event.Content != "also check the logs" || got[1].Event.Kind != "comment" {
		t.Fatalf("deliveries: %+v", got)
	}
	// Live delivery once running.
	r = e.must("POST", "/v1/tasks/"+id+":say", e.owner, map[string]any{"text": "now"})
	if r["delivered"] != true {
		t.Fatal("not delivered to a running session")
	}
	// Sessions cannot message tasks.
	if code, _ := e.call("POST", "/v1/tasks/"+id+":say", sess, map[string]any{"text": "x"}); code != http.StatusForbidden {
		t.Fatalf("session using say: %d", code)
	}
}

func TestApprovalResultReachesSession(t *testing.T) {
	e := setup(t)
	id, sess := e.startTask("t", "plan")
	e.must("POST", "/hooks/claude/channel-ready", sess, map[string]any{})
	a := e.must("POST", "/v1/approvals", sess, map[string]any{"class": "APPROVAL_CLASS_QUESTION", "summary": "Use sqlite?"})
	e.must("POST", "/v1/approvals/"+a["id"].(string)+":decide", e.owner, map[string]any{"approve": false, "reason": "use postgres", "payload_hash": a["payloadHash"]})
	got := e.calls("deliver", id)
	if len(got) != 1 || got[0].Event.Kind != "action_result" || !strings.Contains(got[0].Event.Content, "use postgres") {
		t.Fatalf("deliveries: %+v", got)
	}
}

func TestTypeFallbackWhenChannelNeverComesUp(t *testing.T) {
	e := setup(t)
	id, _ := e.startTask("t", "plan")
	e.rec.SetDeliverErr(errors.New("channel not registered"))
	e.must("POST", "/v1/tasks/"+id+":say", e.owner, map[string]any{"text": "line one\nline two"})
	e.tick()
	if len(e.calls("type", id)) != 0 {
		t.Fatal("typed before the grace period")
	}
	e.clock.Advance(3 * time.Minute)
	e.tick()
	typed := e.calls("type", id)
	if len(typed) != 1 || !strings.HasPrefix(typed[0].Event.Content, "[aejar chat ") || strings.Contains(typed[0].Event.Content, "\n") {
		t.Fatalf("typed: %+v", typed)
	}
	e.tick()
	if len(e.calls("type", id)) != 1 {
		t.Fatal("event typed twice")
	}
}

// TestGoogleLoginThroughNginx sends what nginx forwards after oauth2-proxy:
// X-Id-Token: Bearer <Google ID token>.
func TestGoogleLoginThroughNginx(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "g1", "kty": "RSA", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer jwks.Close()
	const client = "test-client.apps.googleusercontent.com"
	e := setupWith(t, func(a *auth.Authenticator) {
		a.OIDC = auth.GoogleVerifier(client)
		a.OIDC.JWKSURL = jwks.URL
		a.OwnerEmails = []string{"owner@example.com", "owner.alt@example.com"}
	})
	sign := func(email string) string {
		enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
		head := enc(map[string]string{"alg": "RS256", "kid": "g1"})
		body := enc(map[string]any{"iss": "https://accounts.google.com", "aud": client, "email": email,
			"email_verified": true, "exp": time.Now().Add(time.Hour).Unix()})
		d := sha256.Sum256([]byte(head + "." + body))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
		return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	get := func(idToken string) int {
		req, _ := http.NewRequest("GET", e.url+"/v1/status", nil)
		if idToken != "" {
			req.Header.Set("X-Id-Token", "Bearer "+idToken)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(sign("Owner.Alt@Example.com")); code != http.StatusOK {
		t.Fatalf("allowed email: %d", code)
	}
	if code := get(sign("someone@example.org")); code != http.StatusUnauthorized {
		t.Fatalf("other email: %d", code)
	}
	forged := sign("owner@example.com")
	forged = forged[:len(forged)-4] + "AAAA"
	if code := get(forged); code != http.StatusUnauthorized {
		t.Fatalf("forged token: %d", code)
	}
}

// Regression: the MCP server announced channel-ready even when Claude Code
// refused to register the channel ("Channels are not currently available"),
// so events were marked delivered that Claude never saw. When the session
// reports the channel unavailable, events must be typed into the terminal.
func TestChannelUnavailableFallsBackToTyping(t *testing.T) {
	e := setup(t)
	id, sess := e.startTask("t", "plan")
	e.must("POST", "/hooks/claude/channel-unavailable", sess, map[string]any{"message": "Channels are not currently available"})
	r := e.must("POST", "/v1/tasks/"+id+":say", e.owner, map[string]any{"text": "hello"})
	if r["delivered"] != true {
		t.Fatal("not delivered")
	}
	if len(e.calls("deliver", id)) != 0 {
		t.Fatal("sent through a channel Claude does not listen to")
	}
	typed := e.calls("type", id)
	if len(typed) != 1 || !strings.Contains(typed[0].Event.Content, "hello") {
		t.Fatalf("typed: %+v", typed)
	}
}

// Before the session reports on its channel, events wait (no guessing).
func TestNoDeliveryBeforeChannelStatusKnown(t *testing.T) {
	e := setup(t)
	id, sess := e.startTask("t", "plan")
	e.must("POST", "/v1/tasks/"+id+":say", e.owner, map[string]any{"text": "early"})
	if len(e.calls("deliver", id))+len(e.calls("type", id)) != 0 {
		t.Fatal("delivered before the channel status was known")
	}
	e.must("POST", "/hooks/claude/channel-ready", sess, map[string]any{})
	if len(e.calls("deliver", id)) != 1 {
		t.Fatal("not delivered once ready")
	}
}

// Regression: when the runner failed to start a session, the task stayed
// "running" with no container and never ran again.
func TestFailedStartRequeues(t *testing.T) {
	e := setup(t)
	e.rec.SetStartErr(errors.New("docker: name conflict"))
	id := e.must("POST", "/v1/tasks", e.owner, map[string]any{"title": "t"})["id"].(string)
	e.tick()
	if s := e.state(id); s != "TASK_STATE_QUEUED" {
		t.Fatalf("state after failed start: %s", s)
	}
	var tokens int
	e.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE task_id = ? AND revoked_at IS NULL`, id).Scan(&tokens)
	if tokens != 0 {
		t.Fatal("session token from the failed start is still valid")
	}
	e.rec.SetStartErr(nil)
	e.tick()
	if s := e.state(id); s != "TASK_STATE_RUNNING" {
		t.Fatalf("not retried: %s", s)
	}
}

func TestUIPageServedAndDataNeedsAuth(t *testing.T) {
	e := setup(t)
	resp, err := http.Get(e.url + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("ui: %d %v", resp.StatusCode, resp.Header)
	}
	// The page carries no data; the API behind it still needs a login.
	if code, _ := e.call("GET", "/v1/tasks", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("api without auth: %d", code)
	}
}

func TestSessionScreen(t *testing.T) {
	e := setup(t)
	id, sess := e.startTask("t", "plan")
	e.rec.SetScreen("❯ working on step 2")
	got := e.must("GET", "/v1/tasks/"+id+"/screen", e.owner, nil)
	if got["live"] != true || got["text"] != "❯ working on step 2" {
		t.Fatalf("screen: %v", got)
	}
	// Sessions cannot read screens (their own or others').
	if code, _ := e.call("GET", "/v1/tasks/"+id+"/screen", sess, nil); code != http.StatusForbidden {
		t.Fatalf("session reading a screen: %d", code)
	}
	e.must("POST", "/v1/tasks/"+id+":pause", e.owner, map[string]any{})
	if got := e.must("GET", "/v1/tasks/"+id+"/screen", e.owner, nil); got["live"] == true {
		t.Fatal("paused task reported a live screen")
	}
}

func TestAuditCarriesReadableDetail(t *testing.T) {
	e := setup(t)
	id, sess := e.startTask("t", "mail")
	e.preTool(sess, "Bash", map[string]any{"command": "ls -la /work", "description": "List files"})
	e.preTool(sess, "mcp__claude_ai_Gmail__search_threads", map[string]any{"query": "salary slip from hr"})
	e.must("POST", "/hooks/claude/stop", sess, map[string]any{"last_assistant_message": "Found 3 files."})
	events := e.must("GET", "/v1/audit?subject="+id+"&limit=100", e.owner, nil)["events"].([]any)
	var bashSummary, gmailSummary, reply any
	for _, ev := range events {
		m := ev.(map[string]any)
		p := m["payload"].(map[string]any)
		switch {
		case m["action"] == "tool.pass" && p["tool"] == "Bash":
			bashSummary = p["summary"]
		case m["action"] == "tool.pass" && p["tool"] == "mcp__claude_ai_Gmail__search_threads":
			gmailSummary = p["summary"]
		case m["action"] == "session.idle":
			reply = p["reply"]
		}
	}
	if bashSummary != "command: ls -la /work | description: List files" {
		t.Errorf("bash summary: %v", bashSummary)
	}
	if gmailSummary != nil {
		t.Errorf("private connector input leaked into the audit summary: %v", gmailSummary)
	}
	if reply != "Found 3 files." {
		t.Errorf("reply: %v", reply)
	}
}

// Regression: a session that finished its work stayed "running" forever and
// kept its scheduler slot (Claude Code waits for input after its last turn).
func TestIdleSessionFinishes(t *testing.T) {
	e := setup(t)
	id, sess := e.startTask("t", "plan")
	e.rec.SetScreen("● Done. Summary: all good.")
	e.must("POST", "/hooks/claude/stop", sess, map[string]any{"last_assistant_message": "Done."})
	e.clock.Advance(5 * time.Minute)
	e.tick()
	if s := e.state(id); s != "TASK_STATE_RUNNING" {
		t.Fatalf("finished too early: %s", s)
	}
	// Work in between resets the idle clock.
	e.preTool(sess, "Read", map[string]any{"file_path": "/work/a"})
	e.clock.Advance(6 * time.Minute)
	e.tick()
	if s := e.state(id); s != "TASK_STATE_RUNNING" {
		t.Fatalf("activity did not reset the idle clock: %s", s)
	}
	e.must("POST", "/hooks/claude/stop", sess, map[string]any{"last_assistant_message": "Done again."})
	e.clock.Advance(11 * time.Minute)
	e.tick()
	if s := e.state(id); s != "TASK_STATE_DONE" {
		t.Fatalf("idle session not finished: %s", s)
	}
	if len(e.calls("stop", id)) != 1 {
		t.Fatal("session not stopped")
	}
	// History keeps the last screen.
	if sc := e.must("GET", "/v1/tasks/"+id+"/screen", e.owner, nil); sc["live"] == true || !strings.Contains(sc["text"].(string), "Summary: all good") {
		t.Fatalf("last screen: %v", sc)
	}
	// A message reopens the task with the same conversation.
	e.must("POST", "/v1/tasks/"+id+":say", e.owner, map[string]any{"text": "one more thing"})
	if s := e.state(id); s != "TASK_STATE_QUEUED" {
		t.Fatalf("message did not reopen the task: %s", s)
	}
}

func TestSessionCanCompleteItsTask(t *testing.T) {
	e := setup(t)
	id, sess := e.startTask("t", "plan")
	e.must("POST", "/hooks/claude/task-complete", sess, map[string]any{"message": "Plan written to plan.html"})
	if s := e.state(id); s != "TASK_STATE_RUNNING" {
		t.Fatalf("stopped mid-turn: %s", s)
	}
	e.must("POST", "/hooks/claude/stop", sess, map[string]any{"last_assistant_message": "All done."})
	if s := e.state(id); s != "TASK_STATE_DONE" {
		t.Fatalf("not done after its turn ended: %s", s)
	}
}

func TestOwnerFinishesTask(t *testing.T) {
	e := setup(t)
	id, _ := e.startTask("t", "plan")
	e.must("POST", "/v1/tasks/"+id+":finish", e.owner, map[string]any{})
	if s := e.state(id); s != "TASK_STATE_DONE" || len(e.calls("stop", id)) != 1 {
		t.Fatalf("finish: %s", s)
	}
}

func TestTranscriptHistory(t *testing.T) {
	e := setup(t)
	e.srv.SessionsDir = t.TempDir()
	id, _ := e.startTask("t", "plan")
	proj := filepath.Join(e.srv.SessionsDir, id, "home", ".claude", "projects", "-work")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(
		`{"type":"user","uuid":"u1","timestamp":"2026-10-22T10:00:00Z","message":{"role":"user","content":"hello"}}`+"\n"+
			`{"type":"assistant","uuid":"a1","timestamp":"2026-10-22T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hi there"}]}}`+"\n"), 0o644)
	got := e.must("GET", "/v1/tasks/"+id+"/transcript", e.owner, nil)["entries"].([]any)
	if len(got) != 2 || got[1].(map[string]any)["text"] != "hi there" {
		t.Fatalf("transcript: %v", got)
	}
	// History stays readable after the task is finished.
	e.must("POST", "/v1/tasks/"+id+":finish", e.owner, map[string]any{})
	if got := e.must("GET", "/v1/tasks/"+id+"/transcript", e.owner, nil)["entries"].([]any); len(got) != 2 {
		t.Fatalf("after finish: %v", got)
	}
}

func TestTaskModelChoice(t *testing.T) {
	e := setup(t)
	if code, _ := e.call("POST", "/v1/tasks", e.owner, map[string]any{"title": "x", "model": "gpt-9"}); code != http.StatusBadRequest {
		t.Fatalf("unknown model accepted: %d", code)
	}
	id := e.must("POST", "/v1/tasks", e.owner, map[string]any{"title": "deep plan", "model": "opus"})["id"].(string)
	e.tick()
	for _, c := range e.rec.Calls() {
		if c.Op == "start" && c.TaskID == id && c.Spec.Model != "opus" {
			t.Fatalf("model not passed to the runner: %q", c.Spec.Model)
		}
	}
}
