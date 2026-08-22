// Package surface defines the interaction layer on top of a transport.
//
// A transport moves messages; a surface decides what they mean and what to
// show. Several surfaces can share one transport: on Slack, a "chat" surface
// turns mentions into task threads while a "feed" surface mirrors approvals
// and results into an ops channel.
//
// Inbound:  transport.Inbound --Surface.Handle--> []Intent --> Coordinator
// Outbound: Coordinator --> Event --Surface.Render--> []transport.Outbound
package surface

import (
	"context"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/executor"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// Surface interprets inbound traffic and renders coordinator events.
type Surface interface {
	// Name identifies the surface in config and prompt ids.
	Name() string
	// Transport is the name of the transport this surface is bound to.
	Transport() string
	// Handle interprets one inbound message. ok=false means "not mine",
	// and the coordinator offers it to the next surface on the transport.
	Handle(ctx context.Context, in transport.Inbound) (intents []Intent, ok bool)
	// Render converts a coordinator event into messages for the transport.
	// Returning nil posts nothing.
	Render(ev Event) []transport.Outbound
}

// Intent is something a human asked for. Concrete types below.
type Intent interface{ isIntent() }

// RunTask starts a new task on Thread.
type RunTask struct {
	Thread transport.ThreadID
	Agent  string // definition name; "" = coordinator default
	Prompt string
}

// FollowUp sends Text to the task on Thread, resuming it if needed.
type FollowUp struct {
	Thread transport.ThreadID
	Text   string
}

// Cancel stops the task on Thread.
type Cancel struct{ Thread transport.ThreadID }

// Status asks for the state of the task on Thread.
type Status struct{ Thread transport.ThreadID }

// ListAgents asks for the agent definitions.
type ListAgents struct{ Thread transport.ThreadID }

// Decide answers a permission prompt.
type Decide struct {
	PromptID string
	Choice   string
}

// Say posts Text on Thread without involving a task (help, usage errors).
type Say struct {
	Thread transport.ThreadID
	Text   string
}

func (RunTask) isIntent()    {}
func (FollowUp) isIntent()   {}
func (Cancel) isIntent()     {}
func (Status) isIntent()     {}
func (ListAgents) isIntent() {}
func (Decide) isIntent()     {}
func (Say) isIntent()        {}

// EventKind classifies coordinator events.
type EventKind string

const (
	EventStarted    EventKind = "started"    // task created
	EventResumed    EventKind = "resumed"    // idle session picked up again
	EventAgent      EventKind = "agent"      // Agent carries the agent event
	EventPermission EventKind = "permission" // Agent is a needs_permission; PromptID set
	EventFinished   EventKind = "finished"   // process exited; Task.Status is final
	EventReply      EventKind = "reply"      // Text answers a Status/ListAgents/Say
	EventError      EventKind = "error"      // Text explains a failure
)

// Event is what the coordinator tells surfaces about.
type Event struct {
	Kind     EventKind
	Thread   transport.ThreadID
	Task     *store.TaskState // nil for Reply/Error without a task
	TaskID   executor.TaskID
	Agent    *agent.Event // EventAgent, EventPermission
	PromptID string       // EventPermission: id the Decide intent must echo
	Text     string       // EventReply, EventError
}
