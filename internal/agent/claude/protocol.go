package claude

import "encoding/json"

// Wire types for the Claude Code stream-json protocol (claude 2.1.x).
// Only the fields dancer reads are declared; everything else is ignored.

// line is the envelope of every NDJSON line on stdout.
type line struct {
	Type            string           `json:"type"`
	Subtype         string           `json:"subtype,omitempty"`
	SessionID       string           `json:"session_id,omitempty"`
	ParentToolUseID *string          `json:"parent_tool_use_id,omitempty"`
	Message         json.RawMessage  `json:"message,omitempty"` // apiMessage on assistant/user lines; a string on system/permission_denied
	RequestID       string           `json:"request_id,omitempty"`
	Request         *controlRequest  `json:"request,omitempty"`
	Response        *controlResponse `json:"response,omitempty"` // type=control_response

	// type=system subtype=permission_denied: the CLI's own policy (a rule,
	// hook or the auto-mode classifier) refused a tool call. Message is a
	// human-readable string here.
	ToolName       string `json:"tool_name,omitempty"`
	ToolUseID      string `json:"tool_use_id,omitempty"`
	DecisionReason string `json:"decision_reason,omitempty"`

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

// controlResponse is the payload of a type=control_response line: the
// CLI's answer to a control request dancer sent (initialize, get_usage).
type controlResponse struct {
	Subtype   string          `json:"subtype"` // "success" or "error"
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// usageResponse is the part of a get_usage answer dancer reads: the
// plan's rate-limit windows. The CLI calls the request experimental
// (claude 2.1.240); every field is optional here so a changed shape
// degrades to "no usage", never to a parse error.
type usageResponse struct {
	SubscriptionType    string `json:"subscription_type"`
	RateLimitsAvailable bool   `json:"rate_limits_available"`
	RateLimits          *struct {
		FiveHour    *usageWindow `json:"five_hour"`
		SevenDay    *usageWindow `json:"seven_day"`
		ModelScoped []struct {
			DisplayName string `json:"display_name"`
			usageWindow
		} `json:"model_scoped"`
	} `json:"rate_limits"`
}

type usageWindow struct {
	Utilization *float64 `json:"utilization"` // percent used; null when the window does not apply
	ResetsAt    string   `json:"resets_at"`   // RFC 3339; null/empty when unknown
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
