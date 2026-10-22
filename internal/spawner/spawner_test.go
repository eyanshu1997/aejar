package spawner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
)

const token = "fake-session-token-abcdefghijklmnopqrstuvwxyz"

func testCfg(t *testing.T) Config {
	dir := t.TempDir()
	return Config{
		Image: "aejar-session:test", LocalSessionsDir: dir, HostSessionsDir: "/home/aejar/aejar-data/sessions",
		Network: "aejar_sessions", AejardURL: "http://aejard:9111", CoherenceURL: "http://coherence:8080",
		MemoryMB: 900, CPUs: "1.5", PidsLimit: 512, ClaudeToken: "sk-ant-fake-claude-token", CoherenceKey: "fake-coherence-key",
		StopTimeoutSec: 30,
	}
}

func req() *aejarv1.StartSessionRequest {
	return &aejarv1.StartSessionRequest{TaskId: "t-abcdefgh", Profile: "plan", Prompt: "plan it", SessionToken: token}
}

func TestRunArgsTemplate(t *testing.T) {
	cfg := testCfg(t)
	args := RunArgs(cfg, req())
	joined := strings.Join(args, " ")
	for _, want := range []string{"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--user 1500:1500",
		"--network aejar_sessions", "--pids-limit 512", "--memory 900m"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Exactly four mounts, all under this task's directory, config read-only.
	var mounts []string
	for i, a := range args {
		if a == "-v" {
			mounts = append(mounts, args[i+1])
		}
	}
	if len(mounts) != 4 {
		t.Fatalf("mounts: %v", mounts)
	}
	for _, m := range mounts {
		if !strings.HasPrefix(m, "/home/aejar/aejar-data/sessions/t-abcdefgh/") {
			t.Errorf("mount outside the task dir: %s", m)
		}
		if strings.Contains(m, "docker.sock") {
			t.Errorf("docker socket mounted: %s", m)
		}
	}
	if !slices.Contains(mounts, "/home/aejar/aejar-data/sessions/t-abcdefgh/etc:/etc/aejar:ro") {
		t.Error("config not mounted read-only")
	}
	for _, bad := range []string{"--privileged", "--network host", "--pid host", "--cap-add"} {
		if strings.Contains(joined, bad) {
			t.Errorf("forbidden flag %q", bad)
		}
	}
	// Secrets are passed by name only.
	for _, secret := range []string{token, "sk-ant-fake-claude-token", "fake-coherence-key"} {
		if strings.Contains(joined, secret) {
			t.Errorf("secret %q in argv", secret)
		}
	}
	if args[len(args)-1] != "aejar-session:test" {
		t.Error("image must be the configured one")
	}
}

func TestValidateRejectsInjection(t *testing.T) {
	bad := []*aejarv1.StartSessionRequest{
		{TaskId: "t-abcdefgh/../../etc", Profile: "plan", SessionToken: token},
		{TaskId: "t-abcdefgh --privileged", Profile: "plan", SessionToken: token},
		{TaskId: "t-abcdefgh", Profile: "root", SessionToken: token},
		{TaskId: "t-abcdefgh", Profile: "plan", SessionToken: "short"},
		{TaskId: "t-abcdefgh", Profile: "plan", SessionToken: token, ClaudeSessionId: "x; rm -rf /"},
	}
	for _, r := range bad {
		if Validate(r) == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	if err := Validate(req()); err != nil {
		t.Fatal(err)
	}
}

func TestStartPreparesAndRuns(t *testing.T) {
	cfg := testCfg(t)
	var calls [][]string
	var envs [][]string
	s := &Spawner{Cfg: cfg, Chown: func(string, int, int) error { return nil },
		Exec: func(_ context.Context, env []string, args ...string) ([]byte, error) {
			calls, envs = append(calls, args), append(envs, env)
			if args[0] == "inspect" {
				return nil, errors.New("no such object")
			}
			return nil, nil
		}}
	if _, err := s.StartSession(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if calls[0][0] != "rm" || calls[1][0] != "inspect" || calls[2][0] != "run" {
		t.Fatalf("calls: %v", calls)
	}
	if !slices.Contains(envs[2], "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-fake-claude-token") {
		t.Fatal("claude token not passed via env")
	}
	root := filepath.Join(cfg.LocalSessionsDir, "t-abcdefgh")
	for _, f := range []string{"etc/settings.json", "etc/mcp.json", "etc/launch.json", "etc/role.md", "home/.claude.json"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	// The seeded .claude.json survives restarts (it holds Claude's state).
	os.WriteFile(filepath.Join(root, "home/.claude.json"), []byte(`{"kept":true}`), 0o600)
	if _, err := s.StartSession(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "home/.claude.json")); string(b) != `{"kept":true}` {
		t.Fatal(".claude.json overwritten on restart")
	}
}

func TestSendKeysIsLiteral(t *testing.T) {
	var calls [][]string
	s := &Spawner{Cfg: testCfg(t), Exec: func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	}}
	text := "C-c; $(reboot) Enter"
	if _, err := s.SendKeys(context.Background(), &aejarv1.SendKeysRequest{TaskId: "t-abcdefgh", Text: text}); err != nil {
		t.Fatal(err)
	}
	first := calls[0]
	if first[len(first)-2] != "-l" || first[len(first)-1] != text {
		t.Fatalf("text not sent literally as one argument: %v", first)
	}
}

// Regression: etc/ was created 0750 root-owned, so the session user (uid
// 1500) could not read launch.json and the container exited at once.
func TestSessionUserCanReadConfig(t *testing.T) {
	cfg := testCfg(t)
	if err := Prepare(cfg, req(), func(string, int, int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	etc := filepath.Join(cfg.LocalSessionsDir, "t-abcdefgh", "etc")
	info, err := os.Stat(etc)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o005 != 0o005 {
		t.Fatalf("etc dir mode %v: others cannot list and enter it", info.Mode().Perm())
	}
	for _, f := range []string{"launch.json", "settings.json", "mcp.json", "role.md", "prompt.md"} {
		fi, err := os.Stat(filepath.Join(etc, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o004 == 0 {
			t.Errorf("%s mode %v: not readable by the session user", f, fi.Mode().Perm())
		}
	}
	// Nothing secret may be written there.
	for _, f := range []string{"launch.json", "settings.json", "mcp.json"} {
		b, _ := os.ReadFile(filepath.Join(etc, f))
		if strings.Contains(string(b), token) {
			t.Errorf("%s contains the session token", f)
		}
	}
}

// Regression: the Claude token was pasted from a wrapped terminal line and
// saved with a line break inside it; Claude Code rejected the header
// ("contains a line break"). Token secrets never contain whitespace.
func TestCleanTokenRemovesWrapping(t *testing.T) {
	got := CleanToken("sk-ant-oat01-fakefirstpart\n  fakesecondpart \r\n")
	if got != "sk-ant-oat01-fakefirstpartfakesecondpart" {
		t.Fatalf("got %q", got)
	}
}

// Regression: a quick pause/resume started the new container while docker was
// still removing the old one (--rm), and `docker run` failed with a name
// conflict. Start must wait until the old container is gone.
func TestStartWaitsForOldContainerRemoval(t *testing.T) {
	inspects := 0
	var ops []string
	s := &Spawner{Cfg: testCfg(t), Chown: func(string, int, int) error { return nil },
		Exec: func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			ops = append(ops, args[0])
			if args[0] == "inspect" {
				inspects++
				if inspects < 3 {
					return []byte("running"), nil // still there
				}
				return []byte("Error: No such object"), errors.New("exit 1")
			}
			return nil, nil
		}}
	s.pollInterval = time.Millisecond
	if _, err := s.StartSession(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if ops[len(ops)-1] != "run" || inspects != 3 {
		t.Fatalf("ops %v, inspects %d", ops, inspects)
	}
}
