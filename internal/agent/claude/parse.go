package claude

import (
	"encoding/json"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
)

// parsed is the result of translating one stdout line.
type parsed struct {
	Events     []agent.Event
	Permission *permissionReq // non-nil for control_request/can_use_tool
	Control    *line          // any other control_request (answered with {})
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

	switch l.Type {
	case "system":
		if l.Subtype == "init" {
			ev := base
			ev.Type = agent.EventInit
			ev.Text = l.Model
			emit(ev)
		}
	case "assistant":
		if l.Message == nil {
			break
		}
		for _, c := range l.Message.Content {
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
		if l.Message == nil {
			break
		}
		for _, c := range l.Message.Content {
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
	}
	return p, nil
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
