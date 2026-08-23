// Package store is the append-only event log. Every channel message, agent
// event and decision is written here; the coordinator's "current state" is
// a projection over this log, which is what makes crash-recovery a replay.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/executor"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// ErrNotFound is returned when a task or definition does not exist.
var ErrNotFound = errors.New("store: not found")

// Record is one row in the log.
type Record struct {
	Seq     int64 // monotonically increasing, assigned by the store
	At      time.Time
	Task    executor.TaskID
	Thread  transport.ThreadID
	Kind    string // "inbound", "outbound", "agent", "decision" (a button click), "verdict" (a decider's answer), "closed"
	Payload []byte // JSON of the original message
}

// TaskState is the projection the coordinator reads.
type TaskState struct {
	ID         executor.TaskID
	Transport  string // transport the thread belongs to ("slack", "terminal")
	Thread     transport.ThreadID
	Definition agent.Definition
	// Requester is the transport user id of the human who started the
	// task. Surfaces address them when the agent finishes or waits for
	// an answer, so a muted thread still reaches the one person it is for.
	// Set once when the task is created and never reassigned: a follow-up
	// by someone else still addresses the original requester. Empty for
	// tasks recorded before the column existed, which address nobody.
	Requester string
	Session   string
	// Model is the model the session resolved to, as the agent reported
	// it on its first turn (agent.EventInit). Definition.Model is what
	// was asked for and may be empty; this is what answered.
	Model   string
	Status  string // "queued", "running", "waiting_permission", "done", "failed"
	LastSeq int64
	// Prompt is the last prompt handed to the agent. A restart re-runs a
	// task that never reached a session with it.
	Prompt string
	// Resumes counts consecutive automatic resumes after a restart, so a
	// task that keeps dying with dancer cannot restart-loop forever. It is
	// cleared by any turn that ends on its own.
	Resumes int
	// UpdatedAt is when the projection was last written (set by the store).
	UpdatedAt time.Time
}

// Store persists the log and projections.
type Store interface {
	Append(ctx context.Context, r Record) (seq int64, err error)
	// Replay streams records with Seq > after, in order.
	Replay(ctx context.Context, after int64, fn func(Record) error) error
	// ThreadRecords returns the last limit records of a thread, oldest
	// first. It is how the coordinator reconstructs what was going on in a
	// thread without replaying the whole log.
	ThreadRecords(ctx context.Context, thread transport.ThreadID, limit int) ([]Record, error)
	// ThreadRecordsOfKind is ThreadRecords for one kind: the last human
	// message of a thread is one "inbound" record, however much the agent
	// and dancer posted after it.
	ThreadRecordsOfKind(ctx context.Context, thread transport.ThreadID, kind string, limit int) ([]Record, error)
	// ThreadHeadOfKind is the opposite end: the first limit records of
	// one kind on a thread, oldest first. The first "inbound" record is
	// what a thread is about, whatever was said after.
	ThreadHeadOfKind(ctx context.Context, thread transport.ThreadID, kind string, limit int) ([]Record, error)
	// TaskRecords returns the last limit records of one kind about a task,
	// oldest first. Counting and reading back a task's verdicts goes
	// through here, so that state is a projection of the log rather than
	// a counter that forgets on restart.
	TaskRecords(ctx context.Context, task executor.TaskID, kind string, limit int) ([]Record, error)

	PutTask(ctx context.Context, t TaskState) error
	GetTask(ctx context.Context, id executor.TaskID) (TaskState, error)
	ListTasks(ctx context.Context, status string) ([]TaskState, error)
	// LatestTaskForThread returns the most recently updated task on a thread.
	LatestTaskForThread(ctx context.Context, thread transport.ThreadID) (TaskState, error)

	PutDefinition(ctx context.Context, d agent.Definition) error
	GetDefinition(ctx context.Context, name string) (agent.Definition, error)
	ListDefinitions(ctx context.Context) ([]agent.Definition, error)
	DeleteDefinition(ctx context.Context, name string) error

	// Threads: closing is a property of the thread, not of a task on it,
	// so it outlives the status transitions of whatever ran there.
	// SetThreadClosed(.., false) reopens a closed thread.
	SetThreadClosed(ctx context.Context, thread transport.ThreadID, closed bool) error
	ClosedThreads(ctx context.Context) ([]transport.ThreadID, error)

	// Flows: one per thread; PutFlow replaces an existing one.
	PutFlow(ctx context.Context, f FlowState) error
	ListFlows(ctx context.Context) ([]FlowState, error)
	DeleteFlow(ctx context.Context, thread transport.ThreadID) error

	// Users of the web UI and their sessions (see transport/web). PutUser
	// replaces an existing one; GetUser and GetSession return ErrNotFound.
	PutUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, name string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	DeleteUser(ctx context.Context, name string) error
	PutSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, token string) (Session, error)
	DeleteSession(ctx context.Context, token string) error
	DeleteUserSessions(ctx context.Context, user string) error
}

// User is an account of the web UI. Password is the encoded hash
// (transport/web makes and checks it); the name is the Inbound.UserID
// the user's messages carry.
type User struct {
	Name      string
	Password  string
	CreatedAt time.Time
}

// Session is a web login. Token is the hash of what the browser holds,
// so a copy of the database logs nobody in.
type Session struct {
	Token     string
	User      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// FlowState is a multi-step conversation with a human (e.g. "agent add")
// in progress on a thread. The answers given so far are kept so a restart
// can replay them and continue with the next question.
type FlowState struct {
	Thread    transport.ThreadID
	Transport string
	Surface   string // surface that runs the flow
	Kind      string // "agent_add"
	Answers   []string
}

// Task statuses.
const (
	StatusQueued            = "queued"
	StatusRunning           = "running"
	StatusWaitingPermission = "waiting_permission"
	StatusIdle              = "idle"        // turn finished, session resumable
	StatusInterrupted       = "interrupted" // stopped by a dancer shutdown, session resumable
	StatusDone              = "done"
	StatusFailed            = "failed"
	StatusCancelled         = "cancelled"
)
