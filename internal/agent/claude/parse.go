package claude

import (
	"encoding/json"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
)

// parsed is the result of translating one stdout line.
type parsed struct {
	Events     []agent.Event
	Permission *permissionReq   // non-nil for control_request/can_use_tool
	Control    *line            // any other control_request (answered with {})
	Response   *controlResponse // control_response: the answer to a request dancer sent
}

type permissionReq struct {
	RequestID string
	Event     agent.Event
}

// translate converts one raw NDJSON line into normalized events.
// Unknown line types yield an empty parsed value, never an error.
func translate(raw []byte, now time.Time) (parsed, error) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return parsed{}, err
	}
	base := agent.Event{At: now, Session: l.SessionID, Raw: raw}
	if l.ParentToolUseID != nil {
		base.ParentID = *l.ParentToolUseID
	}
	var p parsed
	emit := func(ev agent.Event) { p.Events = append(p.Events, ev) }

	// Only assistant/user lines carry a Messages API message; other line
	// types put other things in the field (system/permission_denied: a
	// string), so it is decoded here and nowhere else.
	var msg apiMessage
	if (l.Type == "assistant" || l.Type == "user") && len(l.Message) > 0 {
		if err := json.Unmarshal(l.Message, &msg); err != nil {
			return parsed{}, err
		}
	}

	switch l.Type {
	case "system":
		switch l.Subtype {
		case "init":
			ev := base
			ev.Type = agent.EventInit
			ev.Model = l.Model
			ev.Mode = fromCLIMode(l.PermissionMode)
			ev.Version = l.Version
			ev.Workdir = l.Cwd
			switch l.APIKeySource {
			case "none":
				ev.Billing = agent.BillingSubscription
			case "":
				ev.Billing = agent.BillingUnknown
			default:
				ev.Billing = agent.BillingAPIKey
			}
			emit(ev)
		case "permission_denied":
			// The refusal also reaches the agent as an is_error tool_result
			// on the next line; this event is what tells the humans and the
			// log that it was policy, not the tool, that said no.
			ev := base
			ev.Type = agent.EventToolDenied
			ev.Tool = l.ToolName
			ev.ToolID = l.ToolUseID
			ev.Text = l.DecisionReason
			var reason string
			if json.Unmarshal(l.Message, &reason) == nil && reason != "" {
				ev.Text = reason
			}
			emit(ev)
		}
	case "assistant":
		for _, c := range msg.Content {
			switch c.Type {
			case "text":
				ev := base
				ev.Type = agent.EventText
				ev.Text = c.Text
				emit(ev)
			case "tool_use":
				ev := base
				ev.Type = agent.EventToolUse
				ev.Tool = c.Name
				ev.ToolID = c.ID
				ev.ToolInput = c.Input
				emit(ev)
			}
		}
	case "user":
		for _, c := range msg.Content {
			if c.Type != "tool_result" {
				continue
			}
			ev := base
			ev.Type = agent.EventToolResult
			ev.ToolID = c.ToolUseID
			ev.Text = toolResultText(c.Content)
			if c.IsError {
				ev.Tool = "error"
			}
			emit(ev)
		}
	case "result":
		ev := base
		ev.Type = agent.EventResult
		ev.Text = l.Result
		ev.Cost = l.TotalCostUSD
		if l.IsError {
			ev.Type = agent.EventError
		}
		emit(ev)
	case "control_request":
		if l.Request == nil {
			break
		}
		if l.Request.Subtype == "can_use_tool" {
			ev := base
			ev.Type = agent.EventNeedsPermission
			ev.Tool = l.Request.ToolName
			ev.ToolID = l.Request.ToolUseID
			ev.ToolInput = l.Request.Input
			ev.Text = l.Request.Description
			if l.Request.AgentID != "" {
				ev.ParentID = l.Request.AgentID
			}
			if l.Request.ToolName == "AskUserQuestion" {
				ev.Type = agent.EventQuestion
				ev.Questions = parseQuestions(l.Request.Input)
			}
			p.Permission = &permissionReq{RequestID: l.RequestID, Event: ev}
		} else {
			lc := l
			p.Control = &lc
		}
	case "control_response":
		if l.Response != nil {
			rc := *l.Response
			p.Response = &rc
		}
	}
	return p, nil
}

// parseUsage reads a get_usage answer into agent.Usage: nil when the plan
// has no rate-limit windows to report (API key, an unavailable lookup, an
// unexpected shape).
func parseUsage(raw json.RawMessage) *agent.Usage {
	var in usageResponse
	if err := json.Unmarshal(raw, &in); err != nil || in.RateLimits == nil {
		return nil
	}
	u := &agent.Usage{Plan: in.SubscriptionType}
	add := func(name string, w *usageWindow) {
		if w == nil || w.Utilization == nil {
			return
		}
		uw := agent.UsageWindow{Name: name, Used: *w.Utilization}
		if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
			uw.ResetsAt = t
		}
		u.Windows = append(u.Windows, uw)
	}
	add("5h", in.RateLimits.FiveHour)
	add("7d", in.RateLimits.SevenDay)
	for i := range in.RateLimits.ModelScoped {
		m := &in.RateLimits.ModelScoped[i]
		if m.DisplayName != "" {
			add(m.DisplayName, &m.usageWindow)
		}
	}
	if len(u.Windows) == 0 {
		return nil
	}
	return u
}

// parseQuestions decodes the AskUserQuestion tool input.
func parseQuestions(input map[string]any) []agent.Question {
	raw, _ := json.Marshal(input)
	var in struct {
		Questions []struct {
			Header      string `json:"header"`
			Question    string `json:"question"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(raw, &in)
	out := make([]agent.Question, 0, len(in.Questions))
	for _, q := range in.Questions {
		aq := agent.Question{Header: q.Header, Text: q.Question, MultiSelect: q.MultiSelect}
		for _, o := range q.Options {
			aq.Options = append(aq.Options, agent.Option{Label: o.Label, Description: o.Description})
		}
		out = append(out, aq)
	}
	return out
}
