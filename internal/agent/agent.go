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
	EventInit            EventType = "init"             // session started; Session set
	EventText            EventType = "text"             // assistant text (full or delta)
	EventToolUse         EventType = "tool_use"         // agent invoked a tool
	EventToolResult      EventType = "tool_result"      // tool finished
	EventNeedsPermission EventType = "needs_permission" // agent blocked on approval
	EventResult          EventType = "result"           // turn finished
	EventError           EventType = "error"
)

// Event is one normalized message from an agent.
type Event struct {
	Type      EventType
	At        time.Time
	Session   string // agent-native session id, used for Resume
	Text      string // EventText, EventError, EventResult summary
	Tool      string // EventToolUse / EventToolResult / EventNeedsPermission
	ToolInput map[string]any
	ToolID    string // correlates ToolUse, ToolResult, NeedsPermission
	ParentID  string // non-empty when emitted by a sub-agent
	Partial   bool   // EventText: true for streaming deltas
	Cost      float64
	Raw       []byte // vendor message, kept for the event log
}

// PermissionDecision answers an EventNeedsPermission.
type PermissionDecision struct {
	ToolID string
	Allow  bool
	Reason string
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
