// Package feed is a read-mostly surface: it mirrors task starts, permission
// prompts and results from every task into one fixed thread (e.g. an #ops
// channel), and accepts decisions on the prompts it posted. It never
// starts tasks, so it can share a transport with a chat surface.
//
// What it mirrors it mirrors whole: the cost, the plan usage and the work
// overview on a finished turn are rendered with the chat surface's own
// helpers, because someone watching the ops channel is deciding whether to
// go and look at exactly the same evidence.
package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/surface/chat"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// Surface implements surface.Surface.
type Surface struct {
	name      string
	transport string
	thread    transport.ThreadID
	// Approvals: also post permission prompts (with buttons) to the feed.
	Approvals bool
}

// New binds a feed to a transport thread.
func New(name, transportName string, thread transport.ThreadID, approvals bool) *Surface {
	return &Surface{name: name, transport: transportName, thread: thread, Approvals: approvals}
}

func (s *Surface) Name() string      { return s.name }
func (s *Surface) Transport() string { return s.transport }

func (s *Surface) Handle(ctx context.Context, in transport.Inbound) ([]surface.Intent, bool) {
	if in.Decision != nil && strings.HasPrefix(in.Decision.PromptID, s.name+":") {
		return []surface.Intent{surface.Decide{PromptID: in.Decision.PromptID, Choice: in.Decision.Choice}}, true
	}
	// Plain messages in the feed thread are ignored, not forwarded.
	if in.Thread == s.thread && in.Decision == nil {
		return nil, true
	}
	return nil, false
}

func (s *Surface) Render(ev surface.Event) []transport.Outbound {
	out := func(text string) []transport.Outbound {
		return []transport.Outbound{{Thread: s.thread, Text: text}}
	}
	switch ev.Kind {
	case surface.EventStarted:
		return out(fmt.Sprintf("▶️ `%s` started — agent *%s* on %s", ev.TaskID, ev.Task.Definition.Name, ev.Thread))
	case surface.EventPermission:
		if !s.Approvals {
			return nil
		}
		text := fmt.Sprintf("🔐 `%s` — *%s* wants to run:\n```%s```", ev.TaskID, ev.Agent.Tool, truncate(describeInput(ev.Agent), 2500))
		return []transport.Outbound{{Thread: s.thread, Text: text, Prompt: &transport.Prompt{ID: s.name + ":" + ev.PromptID, Choices: []string{"allow", "deny"}}}}
	case surface.EventAllowed:
		// The feed that would have shown the prompt shows what ran instead.
		if !s.Approvals || ev.Agent == nil {
			return nil
		}
		text := fmt.Sprintf("🔓 `%s` — *%s* ran without asking:\n```%s```", ev.TaskID, ev.Agent.Tool, describeInput(ev.Agent))
		if ev.Text != "" {
			text += "\n" + ev.Text
		}
		return out(text)
	case surface.EventQuestion:
		if !s.Approvals {
			return nil
		}
		text := fmt.Sprintf("❓ `%s` — *%s*: %s", ev.TaskID, ev.Question.Header, ev.Question.Text)
		p := &transport.Prompt{ID: s.name + ":" + ev.PromptID, Question: ev.Question.Text}
		for _, o := range ev.Question.Options {
			p.Options = append(p.Options, transport.Option{Value: o.Label, Label: o.Label, Description: o.Description})
		}
		return []transport.Outbound{{Thread: s.thread, Text: text, Prompt: p}}
	case surface.EventAgent:
		if ev.Agent == nil {
			return nil
		}
		switch ev.Agent.Type {
		case agent.EventResult:
			line := fmt.Sprintf("✅ `%s` done", ev.TaskID)
			if cost := chat.FormatCost(ev.Agent); cost != "" {
				line += " · " + cost
			}
			return out(chat.WithOverview(line+" — "+truncate(ev.Agent.Text, 300), ev.Work))
		case agent.EventUsage:
			if u := chat.FormatUsage(ev.Agent); u != "" {
				return out(fmt.Sprintf("📊 `%s` · %s", ev.TaskID, u))
			}
		case agent.EventError:
			return out(chat.WithOverview(fmt.Sprintf("❌ `%s` — %s", ev.TaskID, truncate(ev.Agent.Text, 300)), ev.Work))
		}
	case surface.EventFinished:
		if ev.Task.Status == store.StatusFailed || ev.Task.Status == store.StatusCancelled {
			return out(fmt.Sprintf("⏹️ `%s` %s", ev.TaskID, ev.Task.Status))
		}
	case surface.EventError:
		if ev.TaskID != "" {
			return out(fmt.Sprintf("❌ `%s` — %s", ev.TaskID, ev.Text))
		}
	}
	return nil
}

func describeInput(ev *agent.Event) string {
	if cmd, ok := ev.ToolInput["command"].(string); ok {
		return cmd
	}
	if p, ok := ev.ToolInput["file_path"].(string); ok {
		return p
	}
	b, _ := json.Marshal(ev.ToolInput)
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
