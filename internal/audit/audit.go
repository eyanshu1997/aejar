// Package audit is aejar's append-only, hash-chained audit log.
//
// Each event's hash covers the previous event's hash, so editing or deleting
// any row breaks verification from that row onward.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/eyanshu1997/aejar/internal/canon"
	"github.com/eyanshu1997/aejar/internal/store"
)

// Event is one audit row.
type Event struct {
	Seq      int64
	TS       time.Time
	Actor    string
	Action   string
	Subject  string
	Payload  json.RawMessage
	PrevHash string
	Hash     string
}

// Append writes an event. Callers inside a transaction must pass the tx so
// the event commits (or rolls back) with the change it describes.
func Append(ctx context.Context, q store.Querier, now time.Time, actor, action, subject string, payload any) (Event, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	body, err := canon.JSON(payload)
	if err != nil {
		return Event{}, fmt.Errorf("audit payload: %w", err)
	}
	var prev string
	err = q.QueryRowContext(ctx, `SELECT hash FROM audit_events ORDER BY seq DESC LIMIT 1`).Scan(&prev)
	if err != nil && err != sql.ErrNoRows {
		return Event{}, err
	}
	ev := Event{TS: now.UTC(), Actor: actor, Action: action, Subject: subject, Payload: body, PrevHash: prev}
	ev.Hash = hashOf(ev)
	res, err := q.ExecContext(ctx, `INSERT INTO audit_events (ts, actor, action, subject, payload, prev_hash, hash)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, ev.TS.UnixNano(), actor, action, subject, string(body), prev, ev.Hash)
	if err != nil {
		return Event{}, err
	}
	ev.Seq, _ = res.LastInsertId()
	return ev, nil
}

func hashOf(ev Event) string {
	return canon.HashBytes([]byte(strings.Join([]string{
		ev.PrevHash,
		strconv.FormatInt(ev.TS.UnixNano(), 10),
		ev.Actor,
		ev.Action,
		ev.Subject,
		string(ev.Payload),
	}, "\n")))
}

// List returns events after afterSeq, oldest first. An empty subject matches all.
func List(ctx context.Context, q store.Querier, afterSeq int64, limit int, subject string) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `SELECT seq, ts, actor, action, subject, payload, prev_hash, hash FROM audit_events WHERE seq > ?`
	args := []any{afterSeq}
	if subject != "" {
		query += ` AND subject = ?`
		args = append(args, subject)
	}
	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scan(rows)
}

// Recent returns the newest n events for subject, newest first.
func Recent(ctx context.Context, q store.Querier, subject string, n int) ([]Event, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq, ts, actor, action, subject, payload, prev_hash, hash
		FROM audit_events WHERE subject = ? ORDER BY seq DESC LIMIT ?`, subject, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scan(rows)
}

func scan(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var ev Event
		var ts int64
		var payload string
		if err := rows.Scan(&ev.Seq, &ts, &ev.Actor, &ev.Action, &ev.Subject, &payload, &ev.PrevHash, &ev.Hash); err != nil {
			return nil, err
		}
		ev.TS = time.Unix(0, ts).UTC()
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// VerifyResult reports the outcome of Verify.
type VerifyResult struct {
	OK          bool
	Events      int64
	BrokenAtSeq int64
	HeadHash    string
}

// Verify recomputes the whole chain.
func Verify(ctx context.Context, q store.Querier) (VerifyResult, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq, ts, actor, action, subject, payload, prev_hash, hash FROM audit_events ORDER BY seq`)
	if err != nil {
		return VerifyResult{}, err
	}
	defer rows.Close()
	events, err := scan(rows)
	if err != nil {
		return VerifyResult{}, err
	}
	res := VerifyResult{OK: true, Events: int64(len(events))}
	prev := ""
	for _, ev := range events {
		if ev.PrevHash != prev || hashOf(ev) != ev.Hash {
			res.OK = false
			res.BrokenAtSeq = ev.Seq
			return res, nil
		}
		prev = ev.Hash
	}
	res.HeadHash = prev
	return res, nil
}
