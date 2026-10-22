package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/auth"
	"github.com/eyanshu1997/aejar/internal/runner"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// channelGrace is how long a running session may go without its channel
// coming up before queued events are typed into the terminal instead.
const channelGrace = 2 * time.Minute

var eventKinds = map[string]bool{"chat": true, "comment": true, "action_result": true}

// SendToTask queues a message for the task's session and delivers it now if
// the session is running.
func (s *Server) SendToTask(ctx context.Context, req *aejarv1.SendToTaskRequest) (*aejarv1.SendToTaskResponse, error) {
	if strings.TrimSpace(req.Text) == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	kind := req.Kind
	if kind == "" {
		kind = "chat"
	}
	if !eventKinds[kind] {
		return nil, status.Errorf(codes.InvalidArgument, "unknown kind %q", kind)
	}
	if _, err := s.loadTask(ctx, req.Id); err != nil {
		return nil, err
	}
	id, err := s.queueEvent(ctx, auth.From(ctx).Name, req.Id, kind, req.Text)
	if err != nil {
		return nil, err
	}
	delivered, err := s.flushEvents(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return &aejarv1.SendToTaskResponse{EventId: id, Delivered: delivered[id]}, nil
}

func (s *Server) queueEvent(ctx context.Context, actor, taskID, kind, content string) (string, error) {
	id := newID("e")
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO session_events (id, task_id, kind, content, created_at)
		VALUES (?, ?, ?, ?, ?)`, id, taskID, kind, content, s.Now().UnixNano()); err != nil {
		return "", err
	}
	s.audit(ctx, actor, "event.queued", taskID, map[string]any{"event": id, "kind": kind, "text": content})
	return id, nil
}

// flushEvents delivers pending events, oldest first, through the channel of a
// running task. It stops at the first failure so order is kept.
func (s *Server) flushEvents(ctx context.Context, taskID string) (map[string]bool, error) {
	delivered := map[string]bool{}
	var state int32
	if err := s.DB.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&state); err != nil {
		return nil, err
	}
	if aejarv1.TaskState(state) != aejarv1.TaskState_TASK_STATE_RUNNING {
		return delivered, nil
	}
	var channel string
	err := s.DB.QueryRowContext(ctx, `SELECT channel FROM sessions WHERE task_id = ? AND revoked_at IS NULL
		ORDER BY rowid DESC LIMIT 1`, taskID).Scan(&channel)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if channel == "" {
		// Not known yet whether Claude Code registered the channel: wait
		// for the session's report (or the grace-period fallback).
		return delivered, nil
	}
	events, err := s.pendingEvents(ctx, taskID, time.Time{})
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		via := "channel"
		if channel == "ready" {
			err = s.Runner.Deliver(ctx, taskID, ev)
		} else {
			via = "terminal"
			err = s.Runner.Type(ctx, taskID, typedForm(ev))
		}
		if err != nil {
			// Keep order: stop here and retry on the next flush.
			break
		}
		if err := s.markDelivered(ctx, taskID, ev.ID, via); err != nil {
			return nil, err
		}
		delivered[ev.ID] = true
	}
	return delivered, nil
}

// typedForm is how an event looks when typed into the terminal: one line,
// tagged so Claude can tell it apart from text the owner typed while attached.
func typedForm(ev runner.Event) string {
	return fmt.Sprintf("[aejar %s %s] %s", ev.Kind, ev.ID, strings.Join(strings.Fields(ev.Content), " "))
}

// pendingEvents returns undelivered events, optionally only those queued
// before olderThan.
func (s *Server) pendingEvents(ctx context.Context, taskID string, olderThan time.Time) ([]runner.Event, error) {
	query := `SELECT id, kind, content FROM session_events WHERE task_id = ? AND delivered_at IS NULL`
	args := []any{taskID}
	if !olderThan.IsZero() {
		query += ` AND created_at < ?`
		args = append(args, olderThan.UnixNano())
	}
	rows, err := s.DB.QueryContext(ctx, query+` ORDER BY rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runner.Event
	for rows.Next() {
		var ev runner.Event
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.Content); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Server) markDelivered(ctx context.Context, taskID, eventID, via string) error {
	if _, err := s.DB.ExecContext(ctx, `UPDATE session_events SET delivered_at = ?, via = ? WHERE id = ?`,
		s.Now().UnixNano(), via, eventID); err != nil {
		return err
	}
	s.audit(ctx, "aejard", "event.delivered", taskID, map[string]any{"event": eventID, "via": via})
	return nil
}

// typeFallback types events into sessions whose channel never came up. Each
// event is wrapped so Claude can tell it apart from a prompt the owner typed.
func (s *Server) typeFallback(ctx context.Context) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT e.task_id FROM session_events e JOIN tasks t ON t.id = e.task_id
		WHERE e.delivered_at IS NULL AND t.state = ? AND t.updated_at < ?`,
		aejarv1.TaskState_TASK_STATE_RUNNING, s.Now().Add(-channelGrace).UnixNano())
	if err != nil {
		log.Printf("fallback query: %v", err)
		return
	}
	var tasks []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		tasks = append(tasks, id)
	}
	rows.Close()
	for _, taskID := range tasks {
		if _, err := s.flushEvents(ctx, taskID); err != nil {
			continue
		}
		events, err := s.pendingEvents(ctx, taskID, s.Now().Add(-channelGrace))
		if err != nil {
			continue
		}
		for _, ev := range events {
			if err := s.Runner.Type(ctx, taskID, typedForm(ev)); err != nil {
				log.Printf("type fallback %s: %v", taskID, err)
				break
			}
			s.markDelivered(ctx, taskID, ev.ID, "terminal")
		}
	}
}
