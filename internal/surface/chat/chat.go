// Package chat is the conversational surface: commands in a thread start
// tasks, plain messages in a task thread are follow-ups, permission
// requests become prompts, results are posted back.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// Surface implements surface.Surface.
type Surface struct {
	name      string
	transport string
	// Verbose also posts tool calls and tool errors.
	Verbose bool
}

// New binds a chat surface to a transport.
func New(name, transportName string, verbose bool) *Surface {
	return &Surface{name: name, transport: transportName, Verbose: verbose}
}

func (s *Surface) Name() string      { return s.name }
func (s *Surface) Transport() string { return s.transport }

const help = "Commands:\n" +
	"• `<prompt>` — start a task with the default agent\n" +
	"• `run <agent> <prompt>` — start a task with a specific agent\n" +
	"• any other message in a task thread — follow-up to that task\n" +
	"• `status` — task on this thread\n" +
	"• `cancel` — stop the task on this thread\n" +
	"• `agents` — list agent definitions\n" +
	"• `add agent` — define a new agent, question by question"

func (s *Surface) Handle(ctx context.Context, in transport.Inbound) ([]surface.Intent, bool) {
	if in.Decision != nil {
		if !strings.HasPrefix(in.Decision.PromptID, s.name+":") {
			return nil, false
		}
		return []surface.Intent{surface.Decide{PromptID: in.Decision.PromptID, Choice: in.Decision.Choice}}, true
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, true
	}
	cmd, rest := splitWord(text)
	switch strings.ToLower(cmd) {
	case "help", "?":
		return []surface.Intent{surface.Say{Thread: in.Thread, Text: help}}, true
	case "run":
		name, prompt := splitWord(rest)
		if prompt == "" && name == "" {
			return []surface.Intent{surface.Say{Thread: in.Thread, Text: "usage: `run <agent> <prompt>`"}}, true
		}
		return []surface.Intent{surface.RunTask{Thread: in.Thread, Agent: name, Prompt: prompt}}, true
	case "status":
		return []surface.Intent{surface.Status{Thread: in.Thread}}, true
	case "cancel", "stop":
		return []surface.Intent{surface.Cancel{Thread: in.Thread}}, true
	case "agents", "defs", "definitions":
		return []surface.Intent{surface.ListAgents{Thread: in.Thread}}, true
	case "add", "new", "create", "define":
		if w, _ := splitWord(rest); strings.EqualFold(w, "agent") || strings.EqualFold(w, "definition") {
			return []surface.Intent{surface.AddAgent{Thread: in.Thread}}, true
		}
	}
	return []surface.Intent{surface.FollowUp{Thread: in.Thread, Text: text}}, true
}

func (s *Surface) Render(ev surface.Event) []transport.Outbound {
	out := func(text string) []transport.Outbound {
		return []transport.Outbound{{Thread: ev.Thread, Text: text}}
	}
	switch ev.Kind {
	case surface.EventStarted:
		return out(fmt.Sprintf("▶️ task `%s` started with agent *%s* (%s)", ev.TaskID, ev.Task.Definition.Name, ev.Task.Definition.Environment.Kind))
	case surface.EventResumed:
		return out("⏯️ resuming session")
	case surface.EventPermission:
		text := fmt.Sprintf("🔐 *%s* wants to run:\n```%s```", ev.Agent.Tool, describeInput(ev.Agent))
		return []transport.Outbound{{Thread: ev.Thread, Text: text, Prompt: &transport.Prompt{ID: s.name + ":" + ev.PromptID, Choices: []string{"allow", "deny"}}}}
	case surface.EventQuestion:
		return []transport.Outbound{{Thread: ev.Thread, Text: questionText(ev.Question), Prompt: questionPrompt(s.name+":"+ev.PromptID, ev.Question)}}
	case surface.EventAgent:
		return s.renderAgent(ev)
	case surface.EventFinished:
		switch ev.Task.Status {
		case store.StatusCancelled:
			return out("⏹️ cancelled")
		case store.StatusFailed:
			return out("❌ task failed")
		}
		return nil
	case surface.EventReply:
		return out(ev.Text)
	case surface.EventError:
		return out("❌ " + ev.Text)
	}
	return nil
}

func (s *Surface) renderAgent(ev surface.Event) []transport.Outbound {
	a := ev.Agent
	if a == nil {
		return nil
	}
	var text string
	switch a.Type {
	case agent.EventText:
		if a.ParentID != "" && !s.Verbose {
			return nil
		}
		text = a.Text
	case agent.EventToolUse:
		if !s.Verbose {
			return nil
		}
		text = fmt.Sprintf("🔧 %s `%s`", a.Tool, truncate(describeInput(a), 200))
	case agent.EventToolResult:
		if !s.Verbose || a.Tool != "error" {
			return nil
		}
		text = "⚠️ tool error: " + truncate(a.Text, 300)
	case agent.EventResult:
		text = "✅ done · " + FormatCost(a)
	case agent.EventError:
		text = "❌ " + a.Text
	default:
		return nil
	}
	var files []transport.File
	for _, f := range a.Files {
		files = append(files, transport.File{Name: f.Name, Data: f.Data})
	}
	if text == "" && len(files) == 0 {
		return nil
	}
	return []transport.Outbound{{Thread: ev.Thread, Text: text, Files: files}}
}

// FormatCost renders a result's cost: a plain charge for API-key runs, an
// API-equivalent estimate for subscription logins.
func FormatCost(a *agent.Event) string {
	switch a.Billing {
	case agent.BillingSubscription:
		return fmt.Sprintf("≈$%.2f API-equiv", a.Cost)
	case agent.BillingAPIKey:
		return fmt.Sprintf("$%.3f", a.Cost)
	}
	return fmt.Sprintf("$%.3f", a.Cost)
}

// questionText renders a question with its numbered options.
func questionText(q *agent.Question) string {
	var b strings.Builder
	if q.Header != "" {
		fmt.Fprintf(&b, "❓ *%s* — ", q.Header)
	} else {
		b.WriteString("❓ ")
	}
	b.WriteString(q.Text)
	for i, o := range q.Options {
		fmt.Fprintf(&b, "\n%d. *%s*", i+1, o.Label)
		if o.Description != "" {
			fmt.Fprintf(&b, " — %s", o.Description)
		}
	}
	b.WriteString("\n_Pick an option or reply in this thread with your own answer._")
	return b.String()
}

// questionPrompt builds the transport prompt for a question.
func questionPrompt(id string, q *agent.Question) *transport.Prompt {
	p := &transport.Prompt{ID: id, Question: q.Text, FreeText: true}
	for _, o := range q.Options {
		p.Options = append(p.Options, transport.Option{Value: o.Label, Label: o.Label, Description: o.Description})
	}
	return p
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

func splitWord(s string) (string, string) {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t\n")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
