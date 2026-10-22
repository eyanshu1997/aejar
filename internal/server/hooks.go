package server

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/auth"
	"github.com/eyanshu1997/aejar/internal/canon"
	"github.com/eyanshu1997/aejar/internal/limits"
	"github.com/eyanshu1997/aejar/internal/policy"
	"github.com/eyanshu1997/aejar/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ClaudeHook handles every Claude Code HTTP hook. Only PreToolUse returns a
// decision; the rest feed state and the audit log.
func (s *Server) ClaudeHook(ctx context.Context, req *aejarv1.ClaudeHookRequest) (*aejarv1.ClaudeHookResponse, error) {
	p := auth.From(ctx)
	if p.Kind != auth.Session {
		return nil, status.Error(codes.PermissionDenied, "hooks must carry a session token")
	}
	switch req.Event {
	case "pre-tool":
		return s.preTool(ctx, p, req)
	case "post-tool":
		payload := map[string]any{"tool": req.ToolName, "input_hash": inputHash(req)}
		if req.ToolName == "SendMessage" && req.ToolInput != nil {
			// Messages between agents are part of the decision record.
			payload["input"] = req.ToolInput.AsMap()
		}
		s.audit(ctx, p.Name, "tool.used", p.TaskID, payload)
	case "stop":
		return &aejarv1.ClaudeHookResponse{}, s.onStop(ctx, p)
	case "stop-failure":
		return &aejarv1.ClaudeHookResponse{}, s.onStopFailure(ctx, p, req)
	case "session-start":
		if req.SessionId != "" {
			if _, err := s.DB.ExecContext(ctx, `UPDATE tasks SET claude_session_id = ?, updated_at = ? WHERE id = ?`,
				req.SessionId, s.Now().UnixNano(), p.TaskID); err != nil {
				return nil, err
			}
		}
		s.audit(ctx, p.Name, "session.started", p.TaskID, map[string]any{"claude_session_id": req.SessionId, "source": req.Source})
	case "channel-ready", "channel-unavailable":
		// Sent by the session's entrypoint once Claude Code's startup screen
		// shows whether the channel was registered. Only Claude Code knows;
		// the MCP server cannot tell.
		state := strings.TrimPrefix(req.Event, "channel-")
		if _, err := s.DB.ExecContext(ctx, `UPDATE sessions SET channel = ? WHERE token_hash = ?`, state, p.TokenHash); err != nil {
			return nil, err
		}
		s.audit(ctx, p.Name, "session.channel_"+state, p.TaskID, map[string]any{"message": req.Message})
		if _, err := s.flushEvents(ctx, p.TaskID); err != nil {
			return nil, err
		}
	case "session-end":
		s.audit(ctx, p.Name, "session.ended", p.TaskID, map[string]any{"reason": req.Reason})
	case "notification":
		s.audit(ctx, p.Name, "session.notification", p.TaskID, map[string]any{"type": req.NotificationType, "message": req.Message})
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown hook event %q", req.Event)
	}
	return &aejarv1.ClaudeHookResponse{}, nil
}

func inputHash(req *aejarv1.ClaudeHookRequest) string {
	input := map[string]any{}
	if req.ToolInput != nil {
		input = req.ToolInput.AsMap()
	}
	h, _ := canon.Hash(input)
	return h
}

func (s *Server) preTool(ctx context.Context, p auth.Principal, req *aejarv1.ClaudeHookRequest) (*aejarv1.ClaudeHookResponse, error) {
	var input map[string]any
	if req.ToolInput != nil {
		input = req.ToolInput.AsMap()
	}
	d := policy.Decide(s.Policy, policy.Input{
		Profile: p.Profile, Sensitive: p.Sensitive, ToolName: req.ToolName, ToolInput: input,
	})
	hash := inputHash(req)
	record := map[string]any{"tool": req.ToolName, "tier": d.Classification.Tier.String(), "input_hash": hash}

	switch d.Verdict {
	case policy.Pass:
		if d.Classification.Sensitive && !p.Sensitive {
			if _, err := s.DB.ExecContext(ctx, `UPDATE sessions SET sensitive = 1 WHERE token_hash = ?`, p.TokenHash); err != nil {
				return nil, err
			}
			s.audit(ctx, p.Name, "session.sensitive", p.TaskID, map[string]any{"tool": req.ToolName})
		}
		s.audit(ctx, p.Name, "tool.pass", p.TaskID, record)
		// No decision: Claude Code's own permission rules still apply.
		return &aejarv1.ClaudeHookResponse{}, nil

	case policy.NeedsApproval:
		ok, approvalID, err := s.consumeAllowOnce(ctx, p.TaskID, req.ToolName, hash)
		if err != nil {
			return nil, err
		}
		if ok {
			record["approval"] = approvalID
			s.audit(ctx, p.Name, "tool.allowed_once", p.TaskID, record)
			return decision("allow", "approved by the owner ("+approvalID+")"), nil
		}
		record["reason"] = d.Reason
		s.audit(ctx, p.Name, "tool.needs_approval", p.TaskID, record)
		return decision("deny", d.Reason), nil

	default:
		record["reason"] = d.Reason
		s.audit(ctx, p.Name, "tool.denied", p.TaskID, record)
		return decision("deny", d.Reason), nil
	}
}

func decision(verdict, reason string) *aejarv1.ClaudeHookResponse {
	return &aejarv1.ClaudeHookResponse{HookSpecificOutput: &aejarv1.HookSpecificOutput{
		HookEventName:            "PreToolUse",
		PermissionDecision:       verdict,
		PermissionDecisionReason: reason,
	}}
}

// consumeAllowOnce atomically uses an unexpired approval for exactly this
// task, tool and input.
func (s *Server) consumeAllowOnce(ctx context.Context, taskID, tool, hash string) (bool, string, error) {
	now := s.Now().UnixNano()
	var id string
	err := s.DB.QueryRowContext(ctx, `UPDATE allow_once SET used_at = ?
		WHERE approval_id = (SELECT approval_id FROM allow_once
			WHERE task_id = ? AND tool_name = ? AND input_hash = ? AND used_at IS NULL AND expires_at > ?
			ORDER BY expires_at LIMIT 1)
		RETURNING approval_id`, now, taskID, tool, hash, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return err == nil, id, err
}

// onStop runs when a session goes idle. A session idle while it waits on the
// owner gives its slot back.
func (s *Server) onStop(ctx context.Context, p auth.Principal) error {
	var pending int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id = ? AND state = ?`,
		p.TaskID, aejarv1.ApprovalState_APPROVAL_STATE_PENDING).Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		s.audit(ctx, p.Name, "session.idle", p.TaskID, nil)
		return nil
	}
	if err := s.stopTask(ctx, p.TaskID, aejarv1.TaskState_TASK_STATE_WAITING, "approval", "waiting for owner approval"); err != nil {
		return err
	}
	s.Kick()
	return nil
}

func (s *Server) onStopFailure(ctx context.Context, p auth.Principal, req *aejarv1.ClaudeHookRequest) error {
	payload := map[string]any{"error": req.Error, "details": req.ErrorDetails, "message": req.LastAssistantMessage}
	if req.Error != "rate_limit" {
		s.audit(ctx, p.Name, "session.error", p.TaskID, payload)
		return nil
	}
	reset, parsed := limits.ResetTime(req.LastAssistantMessage+" "+req.ErrorDetails, s.Now(), s.Sched.Location)
	// Running sessions are left alone: Claude Code continues them itself at a
	// 5-hour reset. aejard only stops starting new work until the reset.
	if reset.After(s.limitBlockedUntil(ctx)) {
		if err := store.SetKV(ctx, s.DB, kvLimitBlockedUntil, strconv.FormatInt(reset.UnixNano(), 10)); err != nil {
			return err
		}
	}
	payload["blocked_until"] = reset.UTC().Format("2006-01-02T15:04:05Z")
	payload["reset_parsed"] = parsed
	s.audit(ctx, p.Name, "limit.hit", p.TaskID, payload)
	return nil
}
