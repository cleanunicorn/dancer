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
	"github.com/cleanunicorn/dancer/internal/environment"
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
	"• `<prompt>` — start a task with this channel's default agent\n" +
	"• `run <agent> <prompt>` — start a task with a specific agent\n" +
	"• `run` — pick the agent from a list, then type the prompt\n" +
	"• `default <agent>` — set this channel's default agent (`default` shows it)\n" +
	"• any other message in a task thread — follow-up to that task\n" +
	"• `status` — task on this thread\n" +
	"• `cancel` — stop the task on this thread\n" +
	"• `close` — stop the task and end this thread (mention me here to reopen it)\n" +
	"• `agent list` — list agent definitions (`agents` for short)\n" +
	"• `agent add` — define a new agent, question by question\n" +
	"• `agent edit <name>` — change an agent's model, environment, permissions, tools or prompt\n" +
	"• `agent delete <name>` — remove an agent (asks to confirm)"

const agentUsage = "usage: `agent list` · `agent add` · `agent edit [name]` · `agent delete [name]`"

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
		return []surface.Intent{surface.RunTask{Thread: in.Thread, Agent: name, Prompt: prompt}}, true
	case "default":
		name, extra := splitWord(rest)
		if extra != "" {
			return []surface.Intent{surface.Say{Thread: in.Thread, Text: "usage: `default <agent>` or `default`"}}, true
		}
		return []surface.Intent{surface.SetDefault{Thread: in.Thread, Agent: name}}, true
	case "status":
		return []surface.Intent{surface.Status{Thread: in.Thread}}, true
	case "cancel", "stop":
		return []surface.Intent{surface.Cancel{Thread: in.Thread}}, true
	case "close":
		return []surface.Intent{surface.CloseThread{Thread: in.Thread}}, true
	case "agents", "defs", "definitions":
		return []surface.Intent{surface.ListAgents{Thread: in.Thread}}, true
	case "agent", "definition":
		return s.agentCommand(in, rest), true
	case "add", "edit", "delete", "remove":
		// The pre-namespace spelling: point at the new one rather than
		// sending "add agent" to a Claude session as a prompt.
		if w, tail := splitWord(rest); strings.EqualFold(w, "agent") && !strings.Contains(tail, " ") {
			return []surface.Intent{surface.Say{Thread: in.Thread, Text: fmt.Sprintf("`%s agent` is now `agent %s` — %s", strings.ToLower(cmd), strings.ToLower(cmd), agentUsage)}}, true
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
	case surface.EventClosed:
		return out("✅ thread closed — mention me here to pick it up again")
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
	case agent.EventInit:
		text = describeInit(ev)
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

// describeInit renders the session details an agent reports when it starts
// or resumes: the model it actually runs (the definition may leave it to the
// CLI's default), its permission mode, CLI version, billing and where it runs.
func describeInit(ev surface.Event) string {
	a := ev.Agent
	var parts []string
	if ev.Task != nil {
		parts = append(parts, fmt.Sprintf("*%s*", ev.Task.Definition.Name))
	}
	if a.Model != "" {
		parts = append(parts, "`"+a.Model+"`")
	}
	if a.Mode != "" {
		parts = append(parts, string(a.Mode))
	}
	if a.Version != "" {
		parts = append(parts, "claude "+a.Version)
	}
	switch a.Billing {
	case agent.BillingSubscription:
		parts = append(parts, "subscription")
	case agent.BillingAPIKey:
		parts = append(parts, "API key")
	}
	if ev.Task != nil {
		parts = append(parts, describeEnvironment(ev.Task.Definition.Environment, a.Workdir))
	} else if a.Workdir != "" {
		parts = append(parts, a.Workdir)
	}
	return "🤖 " + strings.Join(parts, " · ")
}

// describeEnvironment names where the agent runs: kind, then the docker
// image or ssh host, then the working directory (the one the agent reports,
// falling back to the definition's).
func describeEnvironment(spec environment.Spec, workdir string) string {
	kind := string(spec.Kind)
	if kind == "" {
		kind = string(environment.KindLocal)
	}
	s := kind
	switch spec.Kind {
	case environment.KindDocker:
		if spec.Image != "" {
			s += " " + spec.Image
		}
	case environment.KindSSH:
		if spec.Host != "" {
			s += " " + spec.Host
		}
	}
	if workdir == "" {
		workdir = spec.Workdir
	}
	if workdir != "" {
		s += " " + workdir
	}
	return s
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

// agentCommand handles the `agent <sub> [name]` namespace.
func (s *Surface) agentCommand(in transport.Inbound, rest string) []surface.Intent {
	sub, tail := splitWord(rest)
	name, extra := splitWord(tail)
	usage := func() []surface.Intent { return []surface.Intent{surface.Say{Thread: in.Thread, Text: agentUsage}} }
	switch strings.ToLower(sub) {
	case "", "list", "ls":
		if name != "" {
			return usage()
		}
		return []surface.Intent{surface.ListAgents{Thread: in.Thread}}
	case "add", "new", "create":
		if name != "" {
			return usage()
		}
		return []surface.Intent{surface.AddAgent{Thread: in.Thread}}
	case "edit", "update", "change":
		if extra != "" {
			return usage()
		}
		return []surface.Intent{surface.EditAgent{Thread: in.Thread, Agent: name}}
	case "delete", "remove", "rm":
		if extra != "" {
			return usage()
		}
		return []surface.Intent{surface.DeleteAgent{Thread: in.Thread, Agent: name}}
	}
	return usage()
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
