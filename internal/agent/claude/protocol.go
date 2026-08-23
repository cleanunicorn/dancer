package claude

import "encoding/json"

// Wire types for the Claude Code stream-json protocol (claude 2.1.x).
// Only the fields dancer reads are declared; everything else is ignored.

// line is the envelope of every NDJSON line on stdout.
type line struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype,omitempty"`
	SessionID       string          `json:"session_id,omitempty"`
	ParentToolUseID *string         `json:"parent_tool_use_id,omitempty"`
	Message         json.RawMessage `json:"message,omitempty"` // object on assistant/user lines, plain string on some system lines
	RequestID       string          `json:"request_id,omitempty"`
	Request         *controlRequest `json:"request,omitempty"`

	// type=system subtype=init
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
	APIKeySource   string `json:"apiKeySource,omitempty"` // "none" = OAuth/subscription login
	Cwd            string `json:"cwd,omitempty"`
	Version        string `json:"claude_code_version,omitempty"`

	// type=result
	IsError      bool    `json:"is_error,omitempty"`
	Result       string  `json:"result,omitempty"`
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
	NumTurns     int     `json:"num_turns,omitempty"`
}

// apiMessage is the Anthropic Messages API message embedded in
// assistant/user lines.
type apiMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// apiMessage decodes the line's message field. It is nil when the line has
// none or when the field is not an object: type=system subtype=permission_denied
// (a tool call refused by the CLI's own policy, e.g. the auto-mode classifier)
// puts a human-readable string there, and the refusal reaches the agent as an
// ordinary is_error tool_result on the next line, so there is nothing to read.
func (l *line) apiMessage() (*apiMessage, error) {
	if len(l.Message) == 0 || l.Message[0] != '{' {
		return nil, nil
	}
	var m apiMessage
	if err := json.Unmarshal(l.Message, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

type contentBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// controlRequest is the payload of a type=control_request line.
type controlRequest struct {
	Subtype     string         `json:"subtype"`
	ToolName    string         `json:"tool_name,omitempty"`
	DisplayName string         `json:"display_name,omitempty"`
	Input       map[string]any `json:"input,omitempty"`
	Description string         `json:"description,omitempty"`
	ToolUseID   string         `json:"tool_use_id,omitempty"`
	AgentID     string         `json:"agent_id,omitempty"`
}

// Outbound (stdin) messages.

type userMessage struct {
	Type    string         `json:"type"`
	Message userMsgPayload `json:"message"`
}

type userMsgPayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type controlRequestOut struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"`
	Request   map[string]any `json:"request"`
}

type controlResponseOut struct {
	Type     string              `json:"type"`
	Response controlResponseBody `json:"response"`
}

type controlResponseBody struct {
	Subtype   string `json:"subtype"`
	RequestID string `json:"request_id"`
	Response  any    `json:"response,omitempty"`
	Error     string `json:"error,omitempty"`
}

type permissionAllow struct {
	Behavior     string         `json:"behavior"`
	UpdatedInput map[string]any `json:"updatedInput"`
}

type permissionDeny struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message"`
}

// toolResultText flattens a tool_result content field, which is either a
// string or an array of content blocks.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		out := ""
		for _, b := range blocks {
			if b.Type == "text" {
				out += b.Text
			}
		}
		return out
	}
	return string(raw)
}
