// Package agent defines the coding-agent abstraction.
//
// An Agent (Claude Code, Codex, ...) is a process started inside an
// Environment that speaks a line-oriented JSON protocol. Each implementation
// translates its native protocol into the normalized Event type so the
// executor and coordinator never see vendor-specific messages.
package agent

import (
	"context"
	"time"

	"github.com/cleanunicorn/dancer/internal/environment"
)

// Kind names an agent implementation.
type Kind string

const (
	KindClaude Kind = "claude"
	KindCodex  Kind = "codex"
)

// PermissionMode mirrors the agent's own permission levels.
type PermissionMode string

const (
	PermissionManual      PermissionMode = "manual"      // ask for every tool
	PermissionAcceptEdits PermissionMode = "acceptEdits" // auto-accept file edits, ask for the rest
	PermissionAuto        PermissionMode = "auto"
	PermissionBypass      PermissionMode = "bypassPermissions"
)

// Definition is a reusable agent configuration stored in the DB.
// An instance is Definition + Environment + SessionID.
type Definition struct {
	Name           string
	Kind           Kind
	Model          string
	SystemPrompt   string
	AllowedTools   []string
	PermissionMode PermissionMode
	SubAgents      map[string]any // passed through as --agents JSON
	MCPConfig      string         // path or inline JSON for --mcp-config
	Environment    environment.Spec
}

// EventType is the normalized event vocabulary.
type EventType string

const (
	EventInit            EventType = "init"             // session started; Session, Model, Mode, Version, Workdir set
	EventText            EventType = "text"             // assistant text (full or delta)
	EventToolUse         EventType = "tool_use"         // agent invoked a tool
	EventToolResult      EventType = "tool_result"      // tool finished
	EventToolDenied      EventType = "tool_denied"      // the agent's own policy refused a tool call; Tool, ToolID, Text (reason) set
	EventNeedsPermission EventType = "needs_permission" // agent blocked on approval
	EventQuestion        EventType = "question"         // agent asks the human (AskUserQuestion); Questions set
	EventResult          EventType = "result"           // turn finished
	EventError           EventType = "error"
)

// Event is one normalized message from an agent.
type Event struct {
	Type      EventType
	At        time.Time
	Session   string // agent-native session id, used for Resume
	Text      string // EventText, EventError, EventResult summary
	Tool      string // EventToolUse / EventToolResult / EventToolDenied / EventNeedsPermission
	ToolInput map[string]any
	ToolID    string         // correlates ToolUse, ToolResult, NeedsPermission
	ParentID  string         // non-empty when emitted by a sub-agent
	Partial   bool           // EventText: true for streaming deltas
	Questions []Question     // EventQuestion
	Files     []File         // files the agent referred to, fetched from its environment
	Cost      float64        // EventResult: USD at API list prices (see Billing)
	Model     string         // EventInit: model the session resolved to
	Mode      PermissionMode // EventInit: permission mode the agent runs with
	Version   string         // EventInit: agent CLI version
	Workdir   string         // EventInit: working directory the agent reports
	Billing   Billing        // EventInit, EventResult: how this session is paid for
	Raw       []byte         // vendor message, kept for the event log
}

// Billing says whether Cost is a real charge or an API-equivalent estimate.
type Billing string

const (
	BillingUnknown      Billing = ""
	BillingAPIKey       Billing = "api_key"      // metered; Cost is what was charged
	BillingSubscription Billing = "subscription" // flat plan; Cost is what it would have cost at API rates
)

// File is an attachment pulled out of the agent's environment.
type File struct {
	Name string // base name
	Path string // path as the agent wrote it
	Data []byte `json:"-"` // contents; not written to the event log
}

// Question is one question an agent asks the human.
type Question struct {
	Header      string
	Text        string
	Options     []Option
	MultiSelect bool
}

// Option is one selectable answer.
type Option struct {
	Label       string
	Description string
}

// PermissionDecision answers an EventNeedsPermission or EventQuestion.
type PermissionDecision struct {
	ToolID string
	Allow  bool
	Reason string
	// Answers maps Question.Text to the chosen label (or free text) for
	// EventQuestion; nil for plain permissions.
	Answers map[string]string
}

// Run is a live agent turn.
type Run interface {
	// Events streams normalized events until the turn ends. Closed on exit.
	Events() <-chan Event
	// Send delivers a follow-up user message into the running session.
	Send(ctx context.Context, text string) error
	// Decide answers a pending permission request.
	Decide(ctx context.Context, d PermissionDecision) error
	// Stop terminates the agent process.
	Stop() error
}

// Agent is the interface every agent implementation provides.
type Agent interface {
	Kind() Kind
	// Start begins a new session in env with the given prompt.
	Start(ctx context.Context, env environment.Environment, def Definition, prompt string) (Run, error)
	// Resume continues an existing session.
	Resume(ctx context.Context, env environment.Environment, def Definition, session, prompt string) (Run, error)
}
