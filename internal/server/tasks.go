package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/auth"
	"github.com/eyanshu1997/aejar/internal/policy"
	"github.com/eyanshu1997/aejar/internal/scheduler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const taskColumns = `id, title, prompt, class, state, user_bump, wake_boost, wait_reason, profile,
	claude_session_id, doc_folder, created_at, updated_at, model`

func (s *Server) scanTask(row interface{ Scan(...any) error }) (*aejarv1.Task, error) {
	var t aejarv1.Task
	var class, state, wake int32
	var created, updated int64
	err := row.Scan(&t.Id, &t.Title, &t.Prompt, &class, &state, &t.UserBump, &wake, &t.WaitReason, &t.Profile,
		&t.ClaudeSessionId, &t.DocFolder, &created, &updated, &t.Model)
	if err != nil {
		return nil, err
	}
	t.Class, t.State = aejarv1.TaskClass(class), aejarv1.TaskState(state)
	t.CreatedAt = timestamppb.New(time.Unix(0, created))
	t.UpdatedAt = timestamppb.New(time.Unix(0, updated))
	t.Score = int32(scheduler.Score(s.Sched, scheduler.Task{
		Class: scheduler.Class(class), UserBump: int(t.UserBump), WakeBoost: int(wake), CreatedAt: time.Unix(0, created),
	}, s.Now()))
	return &t, nil
}

func (s *Server) loadTask(ctx context.Context, id string) (*aejarv1.Task, error) {
	t, err := s.scanTask(s.DB.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Errorf(codes.NotFound, "task %s not found", id)
	}
	return t, err
}

func (s *Server) CreateTask(ctx context.Context, req *aejarv1.CreateTaskRequest) (*aejarv1.Task, error) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, status.Error(codes.InvalidArgument, "title is required")
	}
	class := req.Class
	if class == aejarv1.TaskClass_TASK_CLASS_UNSPECIFIED {
		class = aejarv1.TaskClass_TASK_CLASS_P1
	}
	profile := req.Profile
	if profile == "" {
		profile = "plan"
	}
	if _, ok := policy.Profiles[profile]; !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown profile %q", profile)
	}
	if req.Model != "" && !policy.Models[req.Model] {
		return nil, status.Errorf(codes.InvalidArgument, "unknown model %q (sonnet, opus or haiku)", req.Model)
	}
	id := newID("t")
	now := s.Now().UnixNano()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO tasks (id, title, prompt, class, state, profile, doc_folder, model, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, title, req.Prompt, class, aejarv1.TaskState_TASK_STATE_QUEUED,
		profile, req.DocFolder, req.Model, now, now)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, auth.From(ctx).Name, "task.created", id, map[string]any{"title": title, "class": class.String(), "profile": profile, "model": req.Model})
	s.Kick()
	return s.loadTask(ctx, id)
}

func (s *Server) GetTask(ctx context.Context, req *aejarv1.GetTaskRequest) (*aejarv1.Task, error) {
	return s.loadTask(ctx, req.Id)
}

func (s *Server) ListTasks(ctx context.Context, req *aejarv1.ListTasksRequest) (*aejarv1.ListTasksResponse, error) {
	query := `SELECT ` + taskColumns + ` FROM tasks`
	var args []any
	if len(req.States) > 0 {
		query += ` WHERE state IN (?` + strings.Repeat(",?", len(req.States)-1) + `)`
		for _, st := range req.States {
			args = append(args, st)
		}
	}
	query += ` ORDER BY created_at DESC LIMIT 500`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resp := &aejarv1.ListTasksResponse{}
	for rows.Next() {
		t, err := s.scanTask(rows)
		if err != nil {
			return nil, err
		}
		resp.Tasks = append(resp.Tasks, t)
	}
	return resp, rows.Err()
}

func (s *Server) SetPriority(ctx context.Context, req *aejarv1.SetPriorityRequest) (*aejarv1.Task, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE tasks SET user_bump = ?, updated_at = ? WHERE id = ?`,
		req.UserBump, s.Now().UnixNano(), req.Id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.Id)
	}
	s.audit(ctx, auth.From(ctx).Name, "task.priority", req.Id, map[string]any{"user_bump": req.UserBump})
	s.Kick()
	return s.loadTask(ctx, req.Id)
}

func (s *Server) PauseTask(ctx context.Context, req *aejarv1.PauseTaskRequest) (*aejarv1.Task, error) {
	t, err := s.loadTask(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	switch t.State {
	case aejarv1.TaskState_TASK_STATE_RUNNING:
		if err := s.stopTask(ctx, t.Id, aejarv1.TaskState_TASK_STATE_PAUSED, "", "paused by owner"); err != nil {
			return nil, err
		}
	case aejarv1.TaskState_TASK_STATE_QUEUED, aejarv1.TaskState_TASK_STATE_WAITING, aejarv1.TaskState_TASK_STATE_LIMIT_BLOCKED:
		if _, err := s.DB.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
			aejarv1.TaskState_TASK_STATE_PAUSED, s.Now().UnixNano(), t.Id); err != nil {
			return nil, err
		}
		s.audit(ctx, auth.From(ctx).Name, "task.paused", t.Id, nil)
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "task is %s", t.State)
	}
	return s.loadTask(ctx, t.Id)
}

func (s *Server) ResumeTask(ctx context.Context, req *aejarv1.ResumeTaskRequest) (*aejarv1.Task, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		aejarv1.TaskState_TASK_STATE_QUEUED, s.Now().UnixNano(), req.Id, aejarv1.TaskState_TASK_STATE_PAUSED)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "task %s is not paused", req.Id)
	}
	s.audit(ctx, auth.From(ctx).Name, "task.resumed", req.Id, nil)
	s.Kick()
	return s.loadTask(ctx, req.Id)
}

func (s *Server) FinishTask(ctx context.Context, req *aejarv1.FinishTaskRequest) (*aejarv1.Task, error) {
	t, err := s.loadTask(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	switch t.State {
	case aejarv1.TaskState_TASK_STATE_DONE:
	case aejarv1.TaskState_TASK_STATE_RUNNING:
		if err := s.stopTask(ctx, t.Id, aejarv1.TaskState_TASK_STATE_DONE, "", "finished by the owner"); err != nil {
			return nil, err
		}
	default:
		if _, err := s.DB.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
			aejarv1.TaskState_TASK_STATE_DONE, s.Now().UnixNano(), t.Id); err != nil {
			return nil, err
		}
		s.audit(ctx, auth.From(ctx).Name, "task.finished", t.Id, nil)
	}
	s.Kick()
	return s.loadTask(ctx, t.Id)
}

func (s *Server) SummarizeTask(ctx context.Context, req *aejarv1.SummarizeTaskRequest) (*aejarv1.TaskSummary, error) {
	t, err := s.loadTask(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	events, err := s.recentEvents(ctx, t.Id, 10)
	if err != nil {
		return nil, err
	}
	approvals, err := s.ListApprovals(ctx, &aejarv1.ListApprovalsRequest{
		TaskId: t.Id, States: []aejarv1.ApprovalState{aejarv1.ApprovalState_APPROVAL_STATE_PENDING},
	})
	if err != nil {
		return nil, err
	}
	return &aejarv1.TaskSummary{Task: t, RecentEvents: events, PendingApprovals: approvals.Approvals}, nil
}

func (s *Server) GetStatus(ctx context.Context, _ *aejarv1.GetStatusRequest) (*aejarv1.Status, error) {
	st := &aejarv1.Status{Cap: int32(s.cap(ctx))}
	rows, err := s.DB.QueryContext(ctx, `SELECT state, COUNT(*) FROM tasks GROUP BY state`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state, n int32
		if err := rows.Scan(&state, &n); err != nil {
			rows.Close()
			return nil, err
		}
		switch aejarv1.TaskState(state) {
		case aejarv1.TaskState_TASK_STATE_RUNNING:
			st.Running = n
		case aejarv1.TaskState_TASK_STATE_QUEUED:
			st.Queued = n
		case aejarv1.TaskState_TASK_STATE_WAITING:
			st.Waiting = n
		}
	}
	rows.Close()
	if until := s.limitBlockedUntil(ctx); until.After(s.Now()) {
		st.LimitBlockedUntil = timestamppb.New(until)
	}
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE state = ?`,
		aejarv1.ApprovalState_APPROVAL_STATE_PENDING).Scan(&st.PendingApprovals)
	return st, err
}

// RequestCapChange creates a POLICY approval; the cap changes only when the
// owner approves it.
func (s *Server) RequestCapChange(ctx context.Context, req *aejarv1.RequestCapChangeRequest) (*aejarv1.Approval, error) {
	if req.Cap < 1 || req.Cap > 8 {
		return nil, status.Error(codes.InvalidArgument, "cap must be between 1 and 8")
	}
	return s.createApproval(ctx, auth.From(ctx).Name, "", aejarv1.ApprovalClass_APPROVAL_CLASS_POLICY,
		"Change concurrent session cap", map[string]any{"kind": "cap", "cap": req.Cap}, "")
}
