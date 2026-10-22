package runner

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
)

// Spawner runs sessions through aejar-spawner and delivers events by writing
// to each session's channel socket, which aejard reaches through the shared
// sessions directory.
type Spawner struct {
	Client      aejarv1.SpawnerServiceClient
	SessionsDir string
}

func (s *Spawner) Start(ctx context.Context, spec Spec) error {
	_, err := s.Client.StartSession(ctx, &aejarv1.StartSessionRequest{
		TaskId: spec.TaskID, Profile: spec.Profile, Prompt: spec.Prompt,
		ClaudeSessionId: spec.ClaudeSessionID, SessionToken: spec.SessionToken, Model: spec.Model,
	})
	return err
}

func (s *Spawner) Stop(ctx context.Context, taskID, reason string) error {
	_, err := s.Client.StopSession(ctx, &aejarv1.StopSessionRequest{TaskId: taskID, Reason: reason})
	return err
}

func (s *Spawner) Type(ctx context.Context, taskID, text string) error {
	_, err := s.Client.SendKeys(ctx, &aejarv1.SendKeysRequest{TaskId: taskID, Text: text})
	return err
}

func (s *Spawner) Screen(ctx context.Context, taskID string, historyLines int) (string, error) {
	r, err := s.Client.CaptureScreen(ctx, &aejarv1.CaptureScreenRequest{TaskId: taskID, HistoryLines: int32(historyLines)})
	if err != nil {
		return "", err
	}
	return r.Text, nil
}

func (s *Spawner) Deliver(ctx context.Context, taskID string, ev Event) error {
	line, err := json.Marshal(map[string]any{"id": ev.ID, "kind": ev.Kind, "content": ev.Content})
	if err != nil {
		return err
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "unix", filepath.Join(s.SessionsDir, taskID, "run", "channel.sock"))
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Write(append(line, '\n'))
	return err
}
