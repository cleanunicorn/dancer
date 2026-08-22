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
	Kind    string // "inbound", "outbound", "agent", "decision"
	Payload []byte // JSON of the original message
}

// TaskState is the projection the coordinator reads.
type TaskState struct {
	ID         executor.TaskID
	Thread     transport.ThreadID
	Definition agent.Definition
	Session    string
	Status     string // "queued", "running", "waiting_permission", "done", "failed"
	LastSeq    int64
}

// Store persists the log and projections.
type Store interface {
	Append(ctx context.Context, r Record) (seq int64, err error)
	// Replay streams records with Seq > after, in order.
	Replay(ctx context.Context, after int64, fn func(Record) error) error

	PutTask(ctx context.Context, t TaskState) error
	GetTask(ctx context.Context, id executor.TaskID) (TaskState, error)
	ListTasks(ctx context.Context, status string) ([]TaskState, error)
	// LatestTaskForThread returns the most recently updated task on a thread.
	LatestTaskForThread(ctx context.Context, thread transport.ThreadID) (TaskState, error)

	PutDefinition(ctx context.Context, d agent.Definition) error
	GetDefinition(ctx context.Context, name string) (agent.Definition, error)
	ListDefinitions(ctx context.Context) ([]agent.Definition, error)
	DeleteDefinition(ctx context.Context, name string) error
}

// Task statuses.
const (
	StatusQueued            = "queued"
	StatusRunning           = "running"
	StatusWaitingPermission = "waiting_permission"
	StatusIdle              = "idle" // turn finished, session resumable
	StatusDone              = "done"
	StatusFailed            = "failed"
	StatusCancelled         = "cancelled"
)
