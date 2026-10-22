// Package server implements aejard's gRPC services, the grpc-gateway REST
// surface, and the scheduler tick that drives the runner.
package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/audit"
	"github.com/eyanshu1997/aejar/internal/auth"
	"github.com/eyanshu1997/aejar/internal/canon"
	"github.com/eyanshu1997/aejar/internal/policy"
	"github.com/eyanshu1997/aejar/internal/runner"
	"github.com/eyanshu1997/aejar/internal/scheduler"
	"github.com/eyanshu1997/aejar/internal/store"
	"github.com/eyanshu1997/aejar/internal/sysinfo"
)

// kv keys.
const (
	kvCap               = "scheduler.cap"
	kvLimitBlockedUntil = "limit.blocked_until"
)

// wakeBoost is added to a task's score when it leaves WAITING.
const wakeBoost = 100

// Server holds aejard's state and implements every service.
type Server struct {
	aejarv1.UnimplementedTaskServiceServer
	aejarv1.UnimplementedApprovalServiceServer
	aejarv1.UnimplementedAuditServiceServer
	aejarv1.UnimplementedOpsServiceServer
	aejarv1.UnimplementedHookServiceServer

	DB        *sql.DB
	Auth      *auth.Authenticator
	Policy    policy.Config
	Sched     scheduler.Config
	Runner    runner.Runner
	Now       func() time.Time
	Resources func() scheduler.Resources

	// tickMu serialises scheduler ticks.
	tickMu sync.Mutex
	kick   chan struct{}
}

// New returns a Server with production defaults for unset fields.
func New(db *sql.DB, a *auth.Authenticator, r runner.Runner) *Server {
	s := &Server{
		DB:     db,
		Auth:   a,
		Policy: policy.DefaultConfig(),
		Sched:  scheduler.DefaultConfig(),
		Runner: r,
		Now:    time.Now,
		kick:   make(chan struct{}, 1),
	}
	s.Resources = func() scheduler.Resources {
		return scheduler.Resources{
			MemAvailableMB: sysinfo.MemAvailableMB(),
			MemPressure:    sysinfo.MemPressure(),
			Load1:          sysinfo.Load1(),
		}
	}
	return s
}

func newID(prefix string) string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// audit appends an audit event, logging (not failing) on error: a broken
// audit write must be visible but should not wedge the caller's response.
// Callers that must be atomic with their change use audit.Append on a tx.
func (s *Server) audit(ctx context.Context, actor, action, subject string, payload any) {
	if _, err := audit.Append(ctx, s.DB, s.Now(), actor, action, subject, payload); err != nil {
		log.Printf("audit append %s %s: %v", action, subject, err)
	}
}

// Kick asks the scheduler loop to tick soon.
func (s *Server) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run ticks the scheduler every interval and whenever kicked, until ctx ends.
func (s *Server) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil {
			log.Printf("scheduler tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

func (s *Server) cap(ctx context.Context) int {
	v, _ := store.GetKV(ctx, s.DB, kvCap)
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return s.Sched.Cap
}

func (s *Server) limitBlockedUntil(ctx context.Context) time.Time {
	v, _ := store.GetKV(ctx, s.DB, kvLimitBlockedUntil)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Tick runs one scheduling round: start what fits, preempt for P0.
func (s *Server) Tick(ctx context.Context) error {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	now := s.Now()

	rows, err := s.DB.QueryContext(ctx, `SELECT id, class, state, user_bump, wake_boost, created_at
		FROM tasks WHERE state IN (?, ?)`, aejarv1.TaskState_TASK_STATE_QUEUED, aejarv1.TaskState_TASK_STATE_RUNNING)
	if err != nil {
		return err
	}
	var tasks []scheduler.Task
	for rows.Next() {
		var t scheduler.Task
		var state int
		var created int64
		if err := rows.Scan(&t.ID, &t.Class, &state, &t.UserBump, &t.WakeBoost, &created); err != nil {
			rows.Close()
			return err
		}
		t.CreatedAt = time.Unix(0, created)
		if aejarv1.TaskState(state) == aejarv1.TaskState_TASK_STATE_RUNNING {
			t.State = scheduler.Running
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	res := s.Resources()
	res.LimitBlockedUntil = s.limitBlockedUntil(ctx)
	cfg := s.Sched
	cfg.Cap = s.cap(ctx)
	plan := scheduler.Decide(cfg, tasks, res, now)

	for _, id := range plan.Preempt {
		if err := s.stopTask(ctx, id, aejarv1.TaskState_TASK_STATE_QUEUED, "", "preempted for a P0 task"); err != nil {
			return err
		}
	}
	for _, id := range plan.Start {
		if err := s.startTask(ctx, id); err != nil {
			return err
		}
	}
	s.typeFallback(ctx)
	return nil
}

// startTask issues a session token, marks the task running and starts it.
func (s *Server) startTask(ctx context.Context, id string) error {
	token := auth.NewToken()
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var spec runner.Spec
	err = tx.QueryRowContext(ctx, `SELECT profile, prompt, claude_session_id FROM tasks WHERE id = ?`, id).
		Scan(&spec.Profile, &spec.Prompt, &spec.ClaudeSessionID)
	if err != nil {
		return err
	}
	spec.TaskID, spec.SessionToken = id, token
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (token_hash, task_id, profile, created_at) VALUES (?, ?, ?, ?)`,
		canon.HashBytes([]byte(token)), id, spec.Profile, now.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, wake_boost = 0, wait_reason = '', updated_at = ? WHERE id = ?`,
		aejarv1.TaskState_TASK_STATE_RUNNING, now.UnixNano(), id); err != nil {
		return err
	}
	if _, err := audit.Append(ctx, tx, now, "aejard", "task.started", id, map[string]any{
		"profile": spec.Profile, "resume": spec.ClaudeSessionID != "",
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.Runner.Start(ctx, spec)
}

// stopTask stops a running task, revokes its session tokens and moves it to
// state. A stopped session keeps its conversation for --resume.
func (s *Server) stopTask(ctx context.Context, id string, state aejarv1.TaskState, waitReason, why string) error {
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, wait_reason = ?, updated_at = ? WHERE id = ?`,
		state, waitReason, now.UnixNano(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at = ? WHERE task_id = ? AND revoked_at IS NULL`,
		now.UnixNano(), id); err != nil {
		return err
	}
	if _, err := audit.Append(ctx, tx, now, "aejard", "task.stopped", id, map[string]any{
		"to": state.String(), "why": why,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.Runner.Stop(ctx, id, why)
}

// wakeIfUnblocked moves a WAITING task back to the queue when it has no
// pending approvals left.
func (s *Server) wakeIfUnblocked(ctx context.Context, taskID string) error {
	var pending int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id = ? AND state = ?`,
		taskID, aejarv1.ApprovalState_APPROVAL_STATE_PENDING).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE tasks SET state = ?, wake_boost = ?, wait_reason = '', updated_at = ?
		WHERE id = ? AND state = ?`, aejarv1.TaskState_TASK_STATE_QUEUED, wakeBoost, s.Now().UnixNano(),
		taskID, aejarv1.TaskState_TASK_STATE_WAITING)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.audit(ctx, "aejard", "task.woken", taskID, map[string]any{"boost": wakeBoost})
		s.Kick()
	}
	return nil
}
