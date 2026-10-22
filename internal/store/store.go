// Package store owns aejar's SQLite database and its schema.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Querier is satisfied by *sql.DB and *sql.Tx.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// migrations run in order; each entry is applied once and recorded in
// schema_version. Never edit an applied entry; append a new one.
var migrations = []string{
	`CREATE TABLE tasks (
		id                TEXT PRIMARY KEY,
		title             TEXT NOT NULL,
		prompt            TEXT NOT NULL DEFAULT '',
		class             INTEGER NOT NULL,
		state             INTEGER NOT NULL,
		user_bump         INTEGER NOT NULL DEFAULT 0,
		wake_boost        INTEGER NOT NULL DEFAULT 0,
		wait_reason       TEXT NOT NULL DEFAULT '',
		profile           TEXT NOT NULL DEFAULT 'plan',
		claude_session_id TEXT NOT NULL DEFAULT '',
		doc_folder        TEXT NOT NULL DEFAULT '',
		created_at        INTEGER NOT NULL,
		updated_at        INTEGER NOT NULL
	);
	CREATE TABLE sessions (
		token_hash  TEXT PRIMARY KEY,
		task_id     TEXT NOT NULL REFERENCES tasks(id),
		profile     TEXT NOT NULL,
		sensitive   INTEGER NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL,
		revoked_at  INTEGER
	);
	CREATE TABLE approvals (
		id           TEXT PRIMARY KEY,
		task_id      TEXT NOT NULL DEFAULT '',
		class        INTEGER NOT NULL,
		state        INTEGER NOT NULL,
		summary      TEXT NOT NULL,
		payload      TEXT NOT NULL,
		payload_hash TEXT NOT NULL,
		tool_name    TEXT NOT NULL DEFAULT '',
		requested_by TEXT NOT NULL,
		decided_by   TEXT NOT NULL DEFAULT '',
		reason       TEXT NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		decided_at   INTEGER,
		expires_at   INTEGER NOT NULL
	);
	CREATE TABLE allow_once (
		approval_id TEXT PRIMARY KEY REFERENCES approvals(id),
		task_id     TEXT NOT NULL,
		tool_name   TEXT NOT NULL,
		input_hash  TEXT NOT NULL,
		expires_at  INTEGER NOT NULL,
		used_at     INTEGER
	);
	CREATE TABLE audit_events (
		seq       INTEGER PRIMARY KEY AUTOINCREMENT,
		ts        INTEGER NOT NULL,
		actor     TEXT NOT NULL,
		action    TEXT NOT NULL,
		subject   TEXT NOT NULL,
		payload   TEXT NOT NULL,
		prev_hash TEXT NOT NULL,
		hash      TEXT NOT NULL
	);
	CREATE TABLE kv (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE INDEX tasks_state ON tasks(state);
	CREATE INDEX approvals_task ON approvals(task_id, state);
	CREATE INDEX audit_subject ON audit_events(subject);`,
	// Messages for a task's session (owner chat, review comments, approval
	// results). Kept until delivered into a running session.
	`CREATE TABLE session_events (
		id           TEXT PRIMARY KEY,
		task_id      TEXT NOT NULL REFERENCES tasks(id),
		kind         TEXT NOT NULL,
		content      TEXT NOT NULL,
		created_at   INTEGER NOT NULL,
		delivered_at INTEGER,
		via          TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX session_events_pending ON session_events(task_id, delivered_at);`,
	// Whether Claude Code registered the session's channel: '' (not known
	// yet), 'ready', or 'unavailable' (events are typed instead).
	`ALTER TABLE sessions ADD COLUMN channel TEXT NOT NULL DEFAULT '';`,
}

// Open opens (creating if needed) the database at path and applies
// migrations. Use ":memory:" only in tests.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One connection serialises all writes, which keeps the audit chain
	// append strictly ordered and avoids SQLITE_BUSY. Load is tiny.
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// GetKV returns the value for key, or "" when unset.
func GetKV(ctx context.Context, q Querier, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetKV upserts key.
func SetKV(ctx context.Context, q Querier, key, value string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
