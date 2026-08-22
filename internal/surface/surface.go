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

// RunTask starts a new task on Thread. With neither Agent nor Prompt the
// coordinator asks for them on the thread (agent picker, then prompt).
type RunTask struct {
	Thread transport.ThreadID
	Agent  string // definition name; "" = the channel's or coordinator's default
	Prompt string
	User   string // transport user id of who asked (transport.Inbound.UserID); the task's requester
}

// SetDefault sets the default agent of the channel Thread belongs to, or
// with an empty Agent reports the current one.
type SetDefault struct {
	Thread transport.ThreadID
	Agent  string
}

// FollowUp sends Text to the task on Thread, resuming it if needed.
type FollowUp struct {
	Thread transport.ThreadID
	Text   string
	User   string // transport user id of who wrote it; the requester if this starts a task
}

// Cancel stops the task on Thread.
type Cancel struct{ Thread transport.ThreadID }

// CloseThread ends the conversation on Thread: the task running there is
// cancelled and the transport stops following the thread, so later chatter
// in it is ignored. Addressing the bot in the thread again reopens it.
type CloseThread struct{ Thread transport.ThreadID }

// Status asks for the state of the task on Thread.
type Status struct{ Thread transport.ThreadID }

// ListAgents asks for the agent definitions.
type ListAgents struct{ Thread transport.ThreadID }

// AddAgent starts the guided "agent add" flow on Thread: the coordinator
// asks for each setting in turn and saves the new definition.
type AddAgent struct{ Thread transport.ThreadID }

// EditAgent starts the guided "agent edit" flow on Thread for the
// definition Agent (picked from a list when empty).
type EditAgent struct {
	Thread transport.ThreadID
	Agent  string
}

// DeleteAgent asks to confirm and then removes the definition Agent
// (picked from a list when empty).
type DeleteAgent struct {
	Thread transport.ThreadID
	Agent  string
}

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

func (RunTask) isIntent()     {}
func (FollowUp) isIntent()    {}
func (Cancel) isIntent()      {}
func (CloseThread) isIntent() {}
func (Status) isIntent()      {}
func (ListAgents) isIntent()  {}
func (AddAgent) isIntent()    {}
func (EditAgent) isIntent()   {}
func (DeleteAgent) isIntent() {}
func (SetDefault) isIntent()  {}
func (Decide) isIntent()      {}
func (Say) isIntent()         {}

// EventKind classifies coordinator events.
type EventKind string

const (
	EventStarted    EventKind = "started"    // task created
	EventResumed    EventKind = "resumed"    // idle session picked up again
	EventAgent      EventKind = "agent"      // Agent carries the agent event
	EventPermission EventKind = "permission" // Agent is a needs_permission; PromptID set
	EventQuestion   EventKind = "question"   // Agent is a question; Question + PromptID set
	EventAllowed    EventKind = "allowed"    // Agent is a needs_permission a decider approved; Text says what ran and why
	EventFinished   EventKind = "finished"   // process exited; Task.Status is final
	EventHeartbeat  EventKind = "heartbeat"  // the task is still at it (or just stopped being); Task.Status says which
	EventClosed     EventKind = "closed"     // the conversation on Thread was closed
	EventReply      EventKind = "reply"      // Text answers a Status/ListAgents/Say
	EventError      EventKind = "error"      // Text explains a failure
)

// Event is what the coordinator tells surfaces about.
//
// EventHeartbeat is the coordinator's periodic word on a live task: it
// is sent every few seconds while an agent turn is running, once more
// when the turn leaves that state (a decision is pending, the turn ended,
// dancer is shutting down), and right after a follow-up was handed to a
// live process. Surfaces use it to show that something is happening —
// the chat surface keeps a status line alive with it — and to take that
// display down when Task.Status is no longer running.
type Event struct {
	Kind     EventKind
	Thread   transport.ThreadID
	Task     *store.TaskState // nil for Reply/Error without a task
	TaskID   executor.TaskID
	Agent    *agent.Event    // EventAgent, EventPermission
	PromptID string          // EventPermission/EventQuestion: id the Decide intent must echo
	Question *agent.Question // EventQuestion: the single question this event carries
	Text     string          // EventReply, EventError
}
