package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/audit"
	"github.com/eyanshu1997/aejar/internal/auth"
	"github.com/eyanshu1997/aejar/internal/canon"
	"github.com/eyanshu1997/aejar/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Approval lifetimes. An ACTION approval is for one exact call, so it is short.
const (
	actionTTL = 24 * time.Hour
	otherTTL  = 7 * 24 * time.Hour
)

func (s *Server) RequestApproval(ctx context.Context, req *aejarv1.RequestApprovalRequest) (*aejarv1.Approval, error) {
	p := auth.From(ctx)
	taskID := req.TaskId
	if p.Kind == auth.Session {
		// A session can only ask on behalf of its own task.
		taskID = p.TaskID
	}
	if req.Class == aejarv1.ApprovalClass_APPROVAL_CLASS_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "class is required")
	}
	if strings.TrimSpace(req.Summary) == "" {
		return nil, status.Error(codes.InvalidArgument, "summary is required")
	}
	if req.Class == aejarv1.ApprovalClass_APPROVAL_CLASS_ACTION && req.ToolName == "" {
		return nil, status.Error(codes.InvalidArgument, "action approvals need tool_name and the exact tool input as payload")
	}
	payload := map[string]any{}
	if req.Payload != nil {
		payload = req.Payload.AsMap()
	}
	return s.createApproval(ctx, p.Name, taskID, req.Class, req.Summary, payload, req.ToolName)
}

func (s *Server) createApproval(ctx context.Context, actor, taskID string, class aejarv1.ApprovalClass, summary string, payload map[string]any, tool string) (*aejarv1.Approval, error) {
	body, err := canon.JSON(payload)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "payload: %v", err)
	}
	ttl := otherTTL
	if class == aejarv1.ApprovalClass_APPROVAL_CLASS_ACTION {
		ttl = actionTTL
	}
	id, now := newID("a"), s.Now()
	hash := canon.HashBytes(body)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO approvals (id, task_id, class, state, summary, payload, payload_hash,
		tool_name, requested_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, taskID, class, aejarv1.ApprovalState_APPROVAL_STATE_PENDING, summary, string(body), hash,
		tool, actor, now.UnixNano(), now.Add(ttl).UnixNano())
	if err != nil {
		return nil, err
	}
	if _, err := audit.Append(ctx, tx, now, actor, "approval.requested", id, map[string]any{
		"task": taskID, "class": class.String(), "tool": tool, "payload_hash": hash, "summary": summary,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.loadApproval(ctx, id)
}

func (s *Server) DecideApproval(ctx context.Context, req *aejarv1.DecideApprovalRequest) (*aejarv1.Approval, error) {
	p := auth.From(ctx)
	if p.Kind != auth.Owner {
		return nil, status.Error(codes.PermissionDenied, "only the owner decides approvals")
	}
	a, err := s.loadApproval(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	if a.State != aejarv1.ApprovalState_APPROVAL_STATE_PENDING {
		return nil, status.Errorf(codes.FailedPrecondition, "approval is already %s", a.State)
	}
	if now.After(a.ExpiresAt.AsTime()) {
		s.DB.ExecContext(ctx, `UPDATE approvals SET state = ? WHERE id = ?`, aejarv1.ApprovalState_APPROVAL_STATE_EXPIRED, a.Id)
		s.audit(ctx, "aejard", "approval.expired", a.Id, nil)
		return nil, status.Error(codes.FailedPrecondition, "approval expired")
	}
	if req.PayloadHash != a.PayloadHash {
		return nil, status.Error(codes.FailedPrecondition, "payload_hash does not match: the request changed since you looked at it")
	}

	state := aejarv1.ApprovalState_APPROVAL_STATE_REJECTED
	if req.Approve {
		state = aejarv1.ApprovalState_APPROVAL_STATE_APPROVED
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE approvals SET state = ?, decided_by = ?, reason = ?, decided_at = ?
		WHERE id = ? AND state = ?`, state, p.Name, req.Reason, now.UnixNano(), a.Id, aejarv1.ApprovalState_APPROVAL_STATE_PENDING)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, status.Error(codes.Aborted, "approval changed concurrently")
	}
	if req.Approve {
		if err := s.applyApproval(ctx, tx, a); err != nil {
			return nil, err
		}
	}
	if _, err := audit.Append(ctx, tx, now, p.Name, "approval.decided", a.Id, map[string]any{
		"approved": req.Approve, "reason": req.Reason, "payload_hash": a.PayloadHash, "task": a.TaskId,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if a.TaskId != "" {
		// Tell the session the outcome; it arrives when the session next runs.
		result := "rejected"
		if req.Approve {
			result = "approved"
			if a.Class == aejarv1.ApprovalClass_APPROVAL_CLASS_ACTION {
				result += ": retry the exact same " + a.ToolName + " call once"
			}
		}
		if req.Reason != "" {
			result += " (" + req.Reason + ")"
		}
		if _, err := s.queueEvent(ctx, "aejard", a.TaskId, "action_result", "Approval "+a.Id+" ("+a.Summary+") was "+result+"."); err != nil {
			return nil, err
		}
		if err := s.wakeIfUnblocked(ctx, a.TaskId); err != nil {
			return nil, err
		}
		if _, err := s.flushEvents(ctx, a.TaskId); err != nil {
			return nil, err
		}
	}
	return s.loadApproval(ctx, a.Id)
}

// applyApproval grants what an approved request asks for, inside the
// decision's transaction.
func (s *Server) applyApproval(ctx context.Context, tx *sql.Tx, a *aejarv1.Approval) error {
	switch a.Class {
	case aejarv1.ApprovalClass_APPROVAL_CLASS_ACTION:
		// One use of exactly this tool with exactly this input.
		_, err := tx.ExecContext(ctx, `INSERT INTO allow_once (approval_id, task_id, tool_name, input_hash, expires_at)
			VALUES (?, ?, ?, ?, ?)`, a.Id, a.TaskId, a.ToolName, a.PayloadHash, a.ExpiresAt.AsTime().UnixNano())
		return err
	case aejarv1.ApprovalClass_APPROVAL_CLASS_POLICY:
		payload := a.Payload.AsMap()
		if payload["kind"] == "cap" {
			n, _ := payload["cap"].(float64)
			if n < 1 {
				return status.Error(codes.InvalidArgument, "bad cap in policy payload")
			}
			return store.SetKV(ctx, tx, kvCap, strconv.Itoa(int(n)))
		}
	}
	return nil
}

const approvalColumns = `id, task_id, class, state, summary, payload, payload_hash, tool_name, requested_by,
	decided_by, reason, created_at, decided_at, expires_at`

func scanApproval(row interface{ Scan(...any) error }) (*aejarv1.Approval, error) {
	var a aejarv1.Approval
	var class, state int32
	var payload string
	var created, expires int64
	var decided sql.NullInt64
	if err := row.Scan(&a.Id, &a.TaskId, &class, &state, &a.Summary, &payload, &a.PayloadHash, &a.ToolName,
		&a.RequestedBy, &a.DecidedBy, &a.Reason, &created, &decided, &expires); err != nil {
		return nil, err
	}
	a.Class, a.State = aejarv1.ApprovalClass(class), aejarv1.ApprovalState(state)
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return nil, err
	}
	st, err := structpb.NewStruct(m)
	if err != nil {
		return nil, err
	}
	a.Payload = st
	a.CreatedAt = timestamppb.New(time.Unix(0, created))
	a.ExpiresAt = timestamppb.New(time.Unix(0, expires))
	if decided.Valid {
		a.DecidedAt = timestamppb.New(time.Unix(0, decided.Int64))
	}
	return &a, nil
}

func (s *Server) loadApproval(ctx context.Context, id string) (*aejarv1.Approval, error) {
	a, err := scanApproval(s.DB.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Errorf(codes.NotFound, "approval %s not found", id)
	}
	return a, err
}

func (s *Server) GetApproval(ctx context.Context, req *aejarv1.GetApprovalRequest) (*aejarv1.Approval, error) {
	a, err := s.loadApproval(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if p := auth.From(ctx); p.Kind == auth.Session && a.TaskId != p.TaskID {
		return nil, status.Errorf(codes.NotFound, "approval %s not found", req.Id)
	}
	return a, nil
}

func (s *Server) ListApprovals(ctx context.Context, req *aejarv1.ListApprovalsRequest) (*aejarv1.ListApprovalsResponse, error) {
	query := `SELECT ` + approvalColumns + ` FROM approvals WHERE 1 = 1`
	var args []any
	if len(req.States) > 0 {
		query += ` AND state IN (?` + strings.Repeat(",?", len(req.States)-1) + `)`
		for _, st := range req.States {
			args = append(args, st)
		}
	}
	if req.TaskId != "" {
		query += ` AND task_id = ?`
		args = append(args, req.TaskId)
	}
	query += ` ORDER BY created_at DESC LIMIT 500`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resp := &aejarv1.ListApprovalsResponse{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		resp.Approvals = append(resp.Approvals, a)
	}
	return resp, rows.Err()
}
