package server

import (
	"context"
	"encoding/json"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/audit"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func toProtoEvent(ev audit.Event) (*aejarv1.AuditEvent, error) {
	var m map[string]any
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		return nil, err
	}
	st, err := structpb.NewStruct(m)
	if err != nil {
		return nil, err
	}
	return &aejarv1.AuditEvent{
		Seq: ev.Seq, Ts: timestamppb.New(ev.TS), Actor: ev.Actor, Action: ev.Action,
		Subject: ev.Subject, Payload: st, PrevHash: ev.PrevHash, Hash: ev.Hash,
	}, nil
}

func toProtoEvents(events []audit.Event) ([]*aejarv1.AuditEvent, error) {
	out := make([]*aejarv1.AuditEvent, 0, len(events))
	for _, ev := range events {
		p, err := toProtoEvent(ev)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) recentEvents(ctx context.Context, subject string, n int) ([]*aejarv1.AuditEvent, error) {
	events, err := audit.Recent(ctx, s.DB, subject, n)
	if err != nil {
		return nil, err
	}
	return toProtoEvents(events)
}

func (s *Server) ListAuditEvents(ctx context.Context, req *aejarv1.ListAuditEventsRequest) (*aejarv1.ListAuditEventsResponse, error) {
	events, err := audit.List(ctx, s.DB, req.AfterSeq, int(req.Limit), req.Subject)
	if err != nil {
		return nil, err
	}
	out, err := toProtoEvents(events)
	return &aejarv1.ListAuditEventsResponse{Events: out}, err
}

func (s *Server) VerifyAudit(ctx context.Context, _ *aejarv1.VerifyAuditRequest) (*aejarv1.VerifyAuditResponse, error) {
	r, err := audit.Verify(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	return &aejarv1.VerifyAuditResponse{Ok: r.OK, Events: r.Events, BrokenAtSeq: r.BrokenAtSeq, HeadHash: r.HeadHash}, nil
}
