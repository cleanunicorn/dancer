// Package sqlite implements store.Store on a single SQLite file
// (modernc.org/sqlite, pure Go, no cgo).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/executor"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/transport"
)

const schema = `
CREATE TABLE IF NOT EXISTS log (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,
	at      TEXT NOT NULL,
	task    TEXT NOT NULL,
	thread  TEXT NOT NULL,
	kind    TEXT NOT NULL,
	payload BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS log_task ON log(task, seq);

CREATE TABLE IF NOT EXISTS tasks (
	id         TEXT PRIMARY KEY,
	thread     TEXT NOT NULL,
	definition BLOB NOT NULL,
	session    TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	last_seq   INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_thread ON tasks(thread, updated_at);

CREATE TABLE IF NOT EXISTS definitions (
	name TEXT PRIMARY KEY,
	body BLOB NOT NULL
);
`

// Store is a SQLite-backed store.Store.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate adds columns introduced after the first release.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(tasks)`)
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	rows.Close()
	if !cols["transport"] {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN transport TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Append(ctx context.Context, r store.Record) (int64, error) {
	if r.At.IsZero() {
		r.At = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO log(at, task, thread, kind, payload) VALUES(?,?,?,?,?)`,
		r.At.UTC().Format(time.RFC3339Nano), string(r.Task), string(r.Thread), r.Kind, r.Payload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Replay(ctx context.Context, after int64, fn func(store.Record) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, task, thread, kind, payload FROM log WHERE seq > ? ORDER BY seq`, after)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r store.Record
		var at, task, thread string
		if err := rows.Scan(&r.Seq, &at, &task, &thread, &r.Kind, &r.Payload); err != nil {
			return err
		}
		r.At, _ = time.Parse(time.RFC3339Nano, at)
		r.Task, r.Thread = executor.TaskID(task), transport.ThreadID(thread)
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) PutTask(ctx context.Context, t store.TaskState) error {
	def, err := json.Marshal(t.Definition)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tasks(id, transport, thread, definition, session, status, last_seq, updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET transport=excluded.transport, thread=excluded.thread, definition=excluded.definition,
			session=excluded.session, status=excluded.status, last_seq=excluded.last_seq, updated_at=excluded.updated_at`,
		string(t.ID), t.Transport, string(t.Thread), def, t.Session, t.Status, t.LastSeq, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetTask(ctx context.Context, id executor.TaskID) (store.TaskState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, transport, thread, definition, session, status, last_seq FROM tasks WHERE id = ?`, string(id))
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return t, store.ErrNotFound
	}
	return t, err
}

func (s *Store) ListTasks(ctx context.Context, status string) ([]store.TaskState, error) {
	q := `SELECT id, transport, thread, definition, session, status, last_seq FROM tasks`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.TaskState
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LatestTaskForThread returns the most recently updated task on a thread.
func (s *Store) LatestTaskForThread(ctx context.Context, thread transport.ThreadID) (store.TaskState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, transport, thread, definition, session, status, last_seq FROM tasks WHERE thread = ? ORDER BY updated_at DESC LIMIT 1`, string(thread))
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return t, store.ErrNotFound
	}
	return t, err
}

type scanner interface{ Scan(dest ...any) error }

func scanTask(sc scanner) (store.TaskState, error) {
	var t store.TaskState
	var id, thread string
	var def []byte
	if err := sc.Scan(&id, &t.Transport, &thread, &def, &t.Session, &t.Status, &t.LastSeq); err != nil {
		return t, err
	}
	t.ID, t.Thread = executor.TaskID(id), transport.ThreadID(thread)
	if err := json.Unmarshal(def, &t.Definition); err != nil {
		return t, fmt.Errorf("sqlite: task %s definition: %w", id, err)
	}
	return t, nil
}

func (s *Store) PutDefinition(ctx context.Context, d agent.Definition) error {
	if d.Name == "" {
		return fmt.Errorf("sqlite: definition name is required")
	}
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO definitions(name, body) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET body=excluded.body`, d.Name, body)
	return err
}

func (s *Store) GetDefinition(ctx context.Context, name string) (agent.Definition, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM definitions WHERE name = ?`, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.Definition{}, store.ErrNotFound
	}
	if err != nil {
		return agent.Definition{}, err
	}
	var d agent.Definition
	return d, json.Unmarshal(body, &d)
}

func (s *Store) ListDefinitions(ctx context.Context) ([]agent.Definition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM definitions ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agent.Definition
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var d agent.Definition
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDefinition removes a definition by name.
func (s *Store) DeleteDefinition(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM definitions WHERE name = ?`, name)
	return err
}
