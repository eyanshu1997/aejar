// Package spawner is the only part of aejar that talks to Docker. It builds
// every session container from one fixed template: callers pass a task id, a
// profile and a few strings, and cannot choose images, mounts, networks or
// privileges. See implementation.html §3.1.
package spawner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/policy"
	"github.com/eyanshu1997/aejar/internal/sessioncfg"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AgentUID is the unprivileged user sessions run as.
const AgentUID = 1500

// Config is fixed at spawner start; nothing in it comes from callers.
type Config struct {
	Image string
	// LocalSessionsDir is where the spawner writes session dirs;
	// HostSessionsDir is the same directory as the Docker daemon sees it.
	LocalSessionsDir string
	HostSessionsDir  string
	Network          string
	AejardURL        string
	CoherenceURL     string
	MemoryMB         int
	CPUs             string
	PidsLimit        int
	// Runtime is an optional OCI runtime, e.g. runsc (gVisor).
	Runtime string
	// Secrets are passed to sessions by name (-e NAME) with values in the
	// docker process environment, so they never appear in argv.
	ClaudeToken    string
	CoherenceKey   string
	DockerBinary   string
	StopTimeoutSec int
}

// Exec runs the docker CLI; tests replace it.
type Exec func(ctx context.Context, env []string, args ...string) ([]byte, error)

// RealExec runs the docker binary.
func RealExec(binary string) Exec {
	return func(ctx context.Context, env []string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}
}

var (
	taskIDRe    = regexp.MustCompile(`^t-[a-z2-7]{8}$`)
	sessionIDRe = regexp.MustCompile(`^[0-9a-f-]{8,64}$`)
	tokenRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)
)

// Validate rejects anything that is not exactly what the template expects.
func Validate(req *aejarv1.StartSessionRequest) error {
	if !taskIDRe.MatchString(req.TaskId) {
		return status.Errorf(codes.InvalidArgument, "bad task id %q", req.TaskId)
	}
	if _, ok := policy.Profiles[req.Profile]; !ok {
		return status.Errorf(codes.InvalidArgument, "unknown profile %q", req.Profile)
	}
	if req.ClaudeSessionId != "" && !sessionIDRe.MatchString(req.ClaudeSessionId) {
		return status.Errorf(codes.InvalidArgument, "bad claude session id")
	}
	if req.Model != "" && !policy.Models[req.Model] {
		return status.Errorf(codes.InvalidArgument, "unknown model %q", req.Model)
	}
	if !tokenRe.MatchString(req.SessionToken) {
		return status.Errorf(codes.InvalidArgument, "bad session token")
	}
	if len(req.Prompt) > 64<<10 {
		return status.Errorf(codes.InvalidArgument, "prompt too long")
	}
	return nil
}

// ContainerName is the session container for a task.
func ContainerName(taskID string) string { return "aejar-s-" + taskID }

// RunArgs is the complete docker run command line for a session. It is the
// one place that decides what a session can touch.
func RunArgs(cfg Config, req *aejarv1.StartSessionRequest) []string {
	host := filepath.Join(cfg.HostSessionsDir, req.TaskId)
	uid := fmt.Sprintf("%d:%d", AgentUID, AgentUID)
	args := []string{
		"run", "-d", "--rm",
		"--name", ContainerName(req.TaskId),
		"--label", "aejar.managed=true",
		"--label", "aejar.task=" + req.TaskId,
		"--hostname", "aejar-" + req.TaskId,
		"--user", uid,
		"--read-only",
		"--tmpfs", "/tmp:rw,size=512m,mode=1777",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", fmt.Sprint(cfg.PidsLimit),
		"--memory", fmt.Sprintf("%dm", cfg.MemoryMB),
		"--memory-swap", fmt.Sprintf("%dm", cfg.MemoryMB+cfg.MemoryMB/2),
		"--cpus", cfg.CPUs,
		"--network", cfg.Network,
		"-v", host + "/work:/work",
		"-v", host + "/home:/home/agent",
		"-v", host + "/etc:" + sessioncfg.EtcDir + ":ro",
		"-v", host + "/run:/run/aejar",
		"-e", "HOME=/home/agent",
		"-e", "AEJAR_TASK_ID=" + req.TaskId,
		"-e", "AEJAR_URL=" + cfg.AejardURL,
		"-e", "COHERENCE_REMOTE_URL=" + cfg.CoherenceURL,
		// Values come from the docker process environment (see secretEnv).
		"-e", "AEJAR_SESSION_TOKEN",
		"-e", "CLAUDE_CODE_OAUTH_TOKEN",
		"-e", "COHERENCE_API_KEY",
	}
	if cfg.Runtime != "" {
		args = append(args, "--runtime", cfg.Runtime)
	}
	return append(args, cfg.Image)
}

func secretEnv(cfg Config, req *aejarv1.StartSessionRequest) []string {
	return []string{
		"AEJAR_SESSION_TOKEN=" + req.SessionToken,
		"CLAUDE_CODE_OAUTH_TOKEN=" + cfg.ClaudeToken,
		"COHERENCE_API_KEY=" + cfg.CoherenceKey,
	}
}

// Prepare writes the session's directories and generated config.
// chown is os.Lchown in production; tests pass a no-op.
func Prepare(cfg Config, req *aejarv1.StartSessionRequest, chown func(string, int, int) error) error {
	files, err := sessioncfg.Generate(sessioncfg.Input{
		TaskID: req.TaskId, Profile: req.Profile, Prompt: req.Prompt,
		ClaudeSessionID: req.ClaudeSessionId, Model: req.Model, AejardURL: cfg.AejardURL,
	})
	if err != nil {
		return err
	}
	root := filepath.Join(cfg.LocalSessionsDir, req.TaskId)
	for _, d := range []string{"work", "home", "run", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			return err
		}
	}
	// etc is regenerated on every start (it carries --resume) and stays
	// owned by the spawner; the session mounts it read-only. The session
	// user must be able to read it, and it holds no secrets (tokens reach
	// the session only through its environment).
	if err := os.Chmod(filepath.Join(root, "etc"), 0o755); err != nil {
		return err
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, "etc", name), body, 0o644); err != nil {
			return err
		}
	}
	claudeJSON := filepath.Join(root, "home", ".claude.json")
	if _, err := os.Stat(claudeJSON); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(claudeJSON, sessioncfg.ClaudeJSON(), 0o600); err != nil {
			return err
		}
	}
	// A stale socket from a previous run would make the channel server fail.
	os.Remove(filepath.Join(root, "run", "channel.sock"))
	for _, d := range []string{"work", "home", "run"} {
		if err := filepath.Walk(filepath.Join(root, d), func(p string, _ os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			return chown(p, AgentUID, AgentUID)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Spawner implements aejarv1.SpawnerServiceServer.
type Spawner struct {
	aejarv1.UnimplementedSpawnerServiceServer
	Cfg   Config
	Exec  Exec
	Chown func(string, int, int) error

	pollInterval time.Duration
}

// waitGone waits until no container has this name. Containers run with
// --rm, so after a stop docker may still be removing the old one.
func (s *Spawner) waitGone(ctx context.Context, name string) error {
	interval := s.pollInterval
	if interval == 0 {
		interval = 500 * time.Millisecond
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := s.Exec(ctx, nil, "inspect", "--format", "{{.State.Status}}", name); err != nil {
			return nil // no such container
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s still exists after 60s", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (s *Spawner) StartSession(ctx context.Context, req *aejarv1.StartSessionRequest) (*aejarv1.StartSessionResponse, error) {
	if err := Validate(req); err != nil {
		return nil, err
	}
	if err := Prepare(s.Cfg, req, s.Chown); err != nil {
		return nil, status.Errorf(codes.Internal, "prepare session: %v", err)
	}
	// A leftover container (crash, restart) is replaced; wait until docker
	// has finished removing it or the name is still taken.
	s.Exec(ctx, nil, "rm", "-f", ContainerName(req.TaskId))
	if err := s.waitGone(ctx, ContainerName(req.TaskId)); err != nil {
		return nil, status.Errorf(codes.Unavailable, "start session: %v", err)
	}
	if _, err := s.Exec(ctx, secretEnv(s.Cfg, req), RunArgs(s.Cfg, req)...); err != nil {
		return nil, status.Errorf(codes.Internal, "start session: %v", err)
	}
	return &aejarv1.StartSessionResponse{Container: ContainerName(req.TaskId)}, nil
}

func (s *Spawner) StopSession(ctx context.Context, req *aejarv1.StopSessionRequest) (*aejarv1.StopSessionResponse, error) {
	if !taskIDRe.MatchString(req.TaskId) {
		return nil, status.Errorf(codes.InvalidArgument, "bad task id %q", req.TaskId)
	}
	// SIGTERM lets Claude Code record the session; --rm then removes the
	// container. Transcripts persist in the session's home dir.
	if out, err := s.Exec(ctx, nil, "stop", "-t", fmt.Sprint(s.Cfg.StopTimeoutSec), ContainerName(req.TaskId)); err != nil &&
		!strings.Contains(string(out), "No such container") {
		return nil, status.Errorf(codes.Internal, "stop session: %v", err)
	}
	return &aejarv1.StopSessionResponse{}, nil
}

func (s *Spawner) SendKeys(ctx context.Context, req *aejarv1.SendKeysRequest) (*aejarv1.SendKeysResponse, error) {
	if !taskIDRe.MatchString(req.TaskId) {
		return nil, status.Errorf(codes.InvalidArgument, "bad task id %q", req.TaskId)
	}
	name, uid := ContainerName(req.TaskId), fmt.Sprint(AgentUID)
	// -l sends the text literally: tmux does not interpret key names in it.
	if _, err := s.Exec(ctx, nil, "exec", "-u", uid, name, "tmux", "send-keys", "-t", "main", "-l", req.Text); err != nil {
		return nil, status.Errorf(codes.Internal, "send keys: %v", err)
	}
	if _, err := s.Exec(ctx, nil, "exec", "-u", uid, name, "tmux", "send-keys", "-t", "main", "Enter"); err != nil {
		return nil, status.Errorf(codes.Internal, "send keys: %v", err)
	}
	return &aejarv1.SendKeysResponse{}, nil
}

func (s *Spawner) CaptureScreen(ctx context.Context, req *aejarv1.CaptureScreenRequest) (*aejarv1.CaptureScreenResponse, error) {
	if !taskIDRe.MatchString(req.TaskId) {
		return nil, status.Errorf(codes.InvalidArgument, "bad task id %q", req.TaskId)
	}
	hist := req.HistoryLines
	if hist < 0 || hist > 2000 {
		hist = 200
	}
	// -p prints the pane as text (no escape sequences unless -e).
	out, err := s.Exec(ctx, nil, "exec", "-u", fmt.Sprint(AgentUID), ContainerName(req.TaskId),
		"tmux", "capture-pane", "-p", "-t", "main", "-S", fmt.Sprintf("-%d", hist))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "capture: %v", err)
	}
	return &aejarv1.CaptureScreenResponse{Text: string(out)}, nil
}

func (s *Spawner) ListSessions(ctx context.Context, _ *aejarv1.ListSessionsRequest) (*aejarv1.ListSessionsResponse, error) {
	out, err := s.Exec(ctx, nil, "ps", "-a", "--filter", "label=aejar.managed=true",
		"--format", `{{.Label "aejar.task"}}	{{.Names}}	{{.State}}`)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	resp := &aejarv1.ListSessionsResponse{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 3 {
			resp.Sessions = append(resp.Sessions, &aejarv1.SessionContainer{TaskId: f[0], Container: f[1], State: f[2]})
		}
	}
	return resp, nil
}

// CleanToken removes all whitespace from a token secret. Tokens never contain
// whitespace, but a value copied from a wrapped terminal line does, and Claude
// Code rejects a header with a line break in it.
func CleanToken(s string) string {
	return strings.Join(strings.Fields(s), "")
}
