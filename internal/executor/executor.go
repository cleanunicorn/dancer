// Package executor runs one task: it provisions an Environment, starts an
// Agent in it, forwards events to the coordinator, and relays permission
// decisions back. Executors are in-process workers for now; the interface
// is the boundary that lets them become remote later.
package executor

import (
	"context"

	"github.com/cleanunicorn/dispatch/internal/agent"
)

// TaskID identifies a unit of work the coordinator asked for.
type TaskID string

// Task is what the coordinator hands to an executor.
type Task struct {
	ID         TaskID
	Definition agent.Definition
	Prompt     string
	Session    string // non-empty: resume this agent session
	// Files are attachments the human sent with the prompt. The executor
	// copies them into the environment before the agent starts and tells
	// it where they are; the agent reads them from disk like any file.
	Files []agent.File
}

// Sink receives what happens while a task runs.
type Sink interface {
	OnEvent(ctx context.Context, id TaskID, ev agent.Event)
	// AwaitDecision blocks until a human answers the permission request.
	AwaitDecision(ctx context.Context, id TaskID, ev agent.Event) (agent.PermissionDecision, error)
}

// Executor runs tasks.
type Executor interface {
	// Run executes the task to completion, reporting through sink.
	Run(ctx context.Context, t Task, sink Sink) error
	// Send delivers a follow-up message to a running task. Files travel
	// the way Task.Files do: into the environment, paths told to the agent.
	Send(ctx context.Context, id TaskID, text string, files []agent.File) error
	// Cancel stops a running task.
	Cancel(ctx context.Context, id TaskID) error
	// IsRunning reports whether the task has a live process. A finished
	// turn keeps its process warm for idle_timeout, so this says "warm or
	// working", not "mid-turn" — the distinction a workflow step's model
	// override needs (a warm process would ignore the new model).
	IsRunning(id TaskID) bool
	// Check runs one shell command in the task's environment and reports
	// the exit code and what it printed. It is how a workflow step is
	// judged on something the log cannot mine — "the tests pass" is not a
	// pull request — by dispatch running the command itself rather than
	// believing the agent's account of it.
	//
	// It borrows the task's live environment, so it has to be called
	// while the process is still there: after the turn ends, before the
	// idle timeout tears a per-task container down. ErrNotRunning when
	// it is already gone.
	Check(ctx context.Context, id TaskID, cmd string) (code int, output string, err error)
}
