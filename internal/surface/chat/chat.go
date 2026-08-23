// Package chat is the conversational surface: commands in a thread start
// tasks, plain messages in a task thread are follow-ups, permission
// requests become prompts, results are posted back.
//
// While a turn runs the thread carries one live status line — what the
// agent is doing, for how long, how many tools it used — kept current by
// the coordinator's heartbeats and by tool events, and always the last
// message in the thread. It is a keyed message (transport.Outbound.Key),
// so Slack edits it in place and the terminal redraws it. The line goes
// away when the agent asks the human something (the prompt says it all)
// and when the turn ends, replaced by a closing line with the outcome,
// duration, tool count and the charge on an API key. On a subscription
// the charge is nobody's bill, so the closing line carries none and the
// agent's usage report (agent.EventUsage, a moment after the result)
// becomes a meter line instead: one bar per plan window, percent used.
//
// The lines that need the human — a turn's closing line, an error, a
// permission or question prompt, a notice that a restart left the task
// for them to pick up — address the task's requester
// (transport.Outbound.Mention), so whoever started the task is notified
// even with the thread muted, and nobody else is. "⏹️ cancelled" is the
// exception: the human who asked for it is already there. Lines without
// a task (help, a wizard's questions) have nobody to address.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

	// now is the clock the status line reads; tests replace it.
	now func() time.Time

	mu       sync.Mutex
	lastInit map[transport.ThreadID]string // last session-details line posted per thread
	turns    map[transport.ThreadID]*turn  // running turn per thread, for the status line
}

// New binds a chat surface to a transport.
func New(name, transportName string, verbose bool) *Surface {
	return &Surface{name: name, transport: transportName, Verbose: verbose, now: time.Now}
}

func (s *Surface) Name() string      { return s.name }
func (s *Surface) Transport() string { return s.transport }

const help = "Commands:\n" +
	"• `<prompt>` — start a task with this channel's default agent\n" +
	"• `run <agent> <prompt>` — start a task with a specific agent\n" +
	"• `run` — pick the agent from a list, then type the prompt\n" +
	"• `default <agent>` — set this channel's default agent (`default` shows it)\n" +
	"• any other message in a task thread — follow-up to that task\n" +
	"• attach files or images to a prompt or follow-up — they are copied into the agent's environment\n" +
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
	if text == "" && len(in.Files) == 0 {
		return nil, true
	}
	// Attachments ride along with a prompt or a follow-up; a command
	// (`status`, `cancel`, …) has no use for them and drops them.
	cmd, rest := splitWord(text)
	switch strings.ToLower(cmd) {
	case "help", "?":
		return []surface.Intent{surface.Say{Thread: in.Thread, Text: help}}, true
	case "run":
		name, prompt := splitWord(rest)
		return []surface.Intent{surface.RunTask{Thread: in.Thread, Agent: name, Prompt: prompt, User: in.UserID, Files: in.Files}}, true
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
	return []surface.Intent{surface.FollowUp{Thread: in.Thread, Text: text, User: in.UserID, Files: in.Files}}, true
}

// Render turns a coordinator event into messages. Besides the lines a
// human reads, it keeps one live status line per thread while a turn is
// running (see status): posted under statusKey so the transport edits it
// in place, moved below every ordinary message so it stays the last thing
// in the thread, and taken down when the turn ends or waits for a human.
func (s *Surface) Render(ev surface.Event) []transport.Outbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	say := func(text string) []transport.Outbound {
		return []transport.Outbound{{Thread: ev.Thread, Text: text}}
	}
	// tell is say for the lines the requester must not miss.
	tell := func(text string) []transport.Outbound {
		return []transport.Outbound{{Thread: ev.Thread, Text: text, Mention: requester(ev)}}
	}
	var msgs []transport.Outbound
	switch ev.Kind {
	case surface.EventStarted:
		s.begin(ev.Thread, now)
		msgs = say(fmt.Sprintf("▶️ task `%s` started with agent *%s* (%s)", ev.TaskID, ev.Task.Definition.Name, ev.Task.Definition.Environment.Kind))
	case surface.EventResumed:
		s.begin(ev.Thread, now)
		msgs = say(fmt.Sprintf("⏯️ resuming session with agent *%s*", ev.Task.Definition.Name))
	case surface.EventHeartbeat:
		return s.heartbeat(ev, now)
	case surface.EventPermission:
		if t := s.turns[ev.Thread]; t != nil {
			t.activity = describeTool(ev.Agent)
		}
		// Cut a huge input here, where the code fence can still be closed;
		// Slack's section block holds 3000 characters.
		text := fmt.Sprintf("🔐 *%s* wants to run:\n```%s```", ev.Agent.Tool, truncate(describeInput(ev.Agent), 2500))
		return s.hideThen(ev.Thread, []transport.Outbound{{Thread: ev.Thread, Text: text, Mention: requester(ev), Prompt: &transport.Prompt{ID: s.name + ":" + ev.PromptID, Choices: []string{"allow", "deny"}}}})
	case surface.EventQuestion:
		return s.hideThen(ev.Thread, []transport.Outbound{{Thread: ev.Thread, Text: questionText(ev.Question), Mention: requester(ev), Prompt: questionPrompt(s.name+":"+ev.PromptID, ev.Question)}})
	case surface.EventAgent:
		return s.renderAgent(ev, now)
	case surface.EventFinished:
		t := s.turns[ev.Thread]
		var closing []transport.Outbound
		switch ev.Task.Status {
		case store.StatusCancelled:
			closing = say("⏹️ cancelled")
		case store.StatusFailed:
			if t == nil || !t.errored { // else the error line said it already
				closing = tell("❌ task failed")
			}
		}
		return s.endWith(ev.Thread, closing)
	case surface.EventClosed:
		return s.endWith(ev.Thread, say("✅ thread closed — mention me here to pick it up again"))
	case surface.EventReply, surface.EventAllowed:
		msgs = say(ev.Text)
	case surface.EventNotice:
		msgs = tell(ev.Text)
	case surface.EventError:
		if t := s.turns[ev.Thread]; t != nil {
			t.errored = true
		}
		msgs = tell("❌ " + ev.Text)
	default:
		return nil
	}
	return s.withStatus(ev.Thread, msgs, now)
}

func (s *Surface) renderAgent(ev surface.Event, now time.Time) []transport.Outbound {
	a := ev.Agent
	if a == nil {
		return nil
	}
	t := s.turns[ev.Thread]
	if t == nil && a.Type != agent.EventResult && a.Type != agent.EventError && a.Type != agent.EventUsage {
		// A follow-up handed to a live process starts a turn without a
		// started/resumed event; the first thing the agent says is it.
		t = s.begin(ev.Thread, now)
	}
	var text string
	markdown := false
	switch a.Type {
	case agent.EventInit:
		t.phase = phaseWorking
		if a.ParentID != "" {
			return nil // a sub-agent's session, not the one the human talks to
		}
		text = s.initLine(ev)
	case agent.EventText:
		t.phase = phaseWorking
		t.activity = ""
		if a.ParentID != "" && !s.Verbose {
			return nil
		}
		text, markdown = a.Text, true
	case agent.EventToolUse:
		t.phase = phaseWorking
		t.tools++
		t.activity = describeTool(a)
		if !s.Verbose {
			return s.refresh(ev.Thread, now)
		}
		text = fmt.Sprintf("🔧 %s `%s`", a.Tool, truncate(describeInput(a), 200))
	case agent.EventToolResult:
		t.activity = ""
		if !s.Verbose || a.Tool != "error" {
			return s.refresh(ev.Thread, now)
		}
		text = "⚠️ tool error: " + truncate(a.Text, 300)
	case agent.EventToolDenied:
		// Policy said no, not the tool: shown whatever Verbose is, because the
		// agent will change plan (or stop) and the human should know why.
		t.activity = ""
		text = fmt.Sprintf("🚫 *%s* refused by the agent's own policy: %s", a.Tool, truncate(a.Text, 300))
	case agent.EventResult:
		return s.endWith(ev.Thread, []transport.Outbound{{Thread: ev.Thread, Text: doneLine(t, a, now), Mention: requester(ev), Files: files(a)}})
	case agent.EventUsage:
		// Lands after the closing line — or, if the human was quick,
		// inside the next turn, where it slots in above the status line.
		text = UsageMeter(a)
	case agent.EventError:
		if t != nil {
			t.errored = true
		}
		return s.endWith(ev.Thread, []transport.Outbound{{Thread: ev.Thread, Text: "❌ " + a.Text, Mention: requester(ev), Files: files(a)}})
	default:
		return nil
	}
	fs := files(a)
	if text == "" && len(fs) == 0 {
		return s.refresh(ev.Thread, now)
	}
	return s.withStatus(ev.Thread, []transport.Outbound{{Thread: ev.Thread, Text: text, Files: fs, Markdown: markdown}}, now)
}

// requester is who to address on the lines that need a human: the user
// who started the event's task, "" when the event has no task (help, a
// wizard question) or the task predates requesters.
func requester(ev surface.Event) string {
	if ev.Task == nil {
		return ""
	}
	return ev.Task.Requester
}

func files(a *agent.Event) []transport.File {
	var out []transport.File
	for _, f := range a.Files {
		out = append(out, transport.File{Name: f.Name, Data: f.Data})
	}
	return out
}

// statusKey is the Outbound.Key of a thread's live status line. One per
// thread is enough: the line is removed before a turn ends, and a removal
// that was lost (a restart, a failed delete) is healed by the next turn
// editing the leftover instead of adding to it.
const statusKey = "status"

// refreshEvery bounds how often tool activity alone redraws the status
// line; a heartbeat redraws it regardless, so a skipped redraw is at most
// one heartbeat late. Slack's edit budget is modest and a busy agent can
// fire several tool calls a second.
const refreshEvery = 3 * time.Second

type phase int

const (
	phaseStarting phase = iota // environment and process coming up, nothing heard yet
	phaseWorking               // the agent is doing something
	phaseWaiting               // a prompt is open; the status line is down
)

// turn is what the status line knows about the running turn on a thread.
type turn struct {
	started  time.Time
	phase    phase
	tools    int       // tool calls so far
	activity string    // current tool call, "" when thinking
	visible  bool      // a status line is posted right now
	shown    time.Time // when it was last posted
	errored  bool      // an error line went out this turn
}

// begin starts tracking a turn on th.
func (s *Surface) begin(th transport.ThreadID, now time.Time) *turn {
	if s.turns == nil {
		s.turns = map[transport.ThreadID]*turn{}
	}
	t := &turn{started: now}
	s.turns[th] = t
	return t
}

// endWith takes down th's status line, posts the closing msgs and stops
// tracking the turn.
func (s *Surface) endWith(th transport.ThreadID, msgs []transport.Outbound) []transport.Outbound {
	out := s.hideThen(th, msgs)
	delete(s.turns, th)
	return out
}

// heartbeat redraws the status line of a running task, or takes it down
// when the task is no longer running.
func (s *Surface) heartbeat(ev surface.Event, now time.Time) []transport.Outbound {
	if ev.Task == nil || ev.Task.Status != store.StatusRunning {
		return s.hideThen(ev.Thread, nil)
	}
	t := s.turns[ev.Thread]
	if t == nil {
		t = s.begin(ev.Thread, now)
	}
	t.phase = phaseWorking
	return s.show(ev.Thread, now)
}

// refresh redraws the status line if it has not been redrawn lately.
func (s *Surface) refresh(th transport.ThreadID, now time.Time) []transport.Outbound {
	t := s.turns[th]
	if t == nil || (t.visible && now.Sub(t.shown) < refreshEvery) {
		return nil
	}
	return s.show(th, now)
}

// show posts or redraws th's status line.
func (s *Surface) show(th transport.ThreadID, now time.Time) []transport.Outbound {
	t := s.turns[th]
	if t == nil || t.phase == phaseWaiting {
		return nil
	}
	t.visible, t.shown = true, now
	return []transport.Outbound{{Thread: th, Key: statusKey, Text: statusLine(t, now)}}
}

// hideThen removes th's status line if one is up, then posts msgs. Used
// when a turn ends or hands over to a human prompt.
func (s *Surface) hideThen(th transport.ThreadID, msgs []transport.Outbound) []transport.Outbound {
	t := s.turns[th]
	if t == nil {
		return msgs
	}
	t.phase = phaseWaiting
	if !t.visible {
		return msgs
	}
	t.visible = false
	return append([]transport.Outbound{{Thread: th, Key: statusKey}}, msgs...)
}

// withStatus posts msgs and keeps the status line below them: when one is
// up it is removed first and posted again after, so the live line stays
// the last message in the thread.
func (s *Surface) withStatus(th transport.ThreadID, msgs []transport.Outbound, now time.Time) []transport.Outbound {
	t := s.turns[th]
	if t == nil || t.phase == phaseWaiting {
		return msgs
	}
	var out []transport.Outbound
	if t.visible {
		out = append(out, transport.Outbound{Thread: th, Key: statusKey})
		t.visible = false
	}
	out = append(out, msgs...)
	return append(out, s.show(th, now)...)
}

// statusLine is what the live line says: the phase or the current tool,
// how long the turn has been going and how many tools it used.
func statusLine(t *turn, now time.Time) string {
	var b strings.Builder
	switch {
	case t.phase == phaseStarting:
		b.WriteString("⏳ starting")
	case t.activity != "":
		b.WriteString(t.activity)
	default:
		b.WriteString("⏳ thinking")
	}
	fmt.Fprintf(&b, " · %s", formatDuration(now.Sub(t.started)))
	if t.tools > 0 {
		fmt.Fprintf(&b, " · %s", plural(t.tools, "tool call"))
	}
	return b.String()
}

// doneLine is the turn's closing line: outcome, how long it took, how
// many tools it used and what it cost. Without a tracked turn (a restart
// in between) it is just outcome and cost.
func doneLine(t *turn, a *agent.Event, now time.Time) string {
	parts := []string{"✅ done"}
	if t != nil {
		parts = append(parts, formatDuration(now.Sub(t.started)))
		if t.tools > 0 {
			parts = append(parts, plural(t.tools, "tool call"))
		}
	}
	if cost := FormatCost(a); cost != "" {
		parts = append(parts, cost)
	}
	return strings.Join(parts, " · ")
}

// describeTool names a tool call for the status line: the tool and the
// gist of its input, kept short enough for one line.
func describeTool(a *agent.Event) string {
	in := strings.TrimSpace(describeInput(a))
	if i := strings.IndexByte(in, '\n'); i >= 0 {
		in = in[:i] + "…"
	}
	in = strings.ReplaceAll(in, "`", "'")
	if in == "" || in == "{}" || in == "null" {
		return "🔧 " + a.Tool
	}
	return fmt.Sprintf("🔧 %s `%s`", a.Tool, truncate(in, 80))
}

// formatDuration renders an elapsed time the way a human glances at it:
// 12s, 1m05s, 1h02m.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// initLine is the session-details line for an init event, or "" when the
// thread already saw the same one from this dancer process. A follow-up
// after idle_timeout resumes the CLI and reports the same details again;
// a restart clears the memory, so the line comes back when dancer does.
// Called with s.mu held.
func (s *Surface) initLine(ev surface.Event) string {
	text := describeInit(ev)
	if text == "" {
		return ""
	}
	if s.lastInit[ev.Thread] == text {
		return ""
	}
	if s.lastInit == nil {
		s.lastInit = map[transport.ThreadID]string{}
	}
	s.lastInit[ev.Thread] = text
	return text
}

// describeInit renders what an agent reports when its session starts or
// resumes: the model it actually runs (the definition may leave it to the
// CLI's default), its permission mode, CLI version, billing and where it
// runs. The ▶️/⏯️ line before it already says which agent. Empty when the
// agent reported nothing beyond a session id.
func describeInit(ev surface.Event) string {
	a, def := ev.Agent, ev.Task.Definition
	var parts []string
	if a.Model != "" {
		parts = append(parts, "`"+a.Model+"`")
	}
	if a.Mode != "" {
		parts = append(parts, string(a.Mode))
	}
	if a.Version != "" {
		parts = append(parts, string(def.Kind)+" "+a.Version)
	}
	switch a.Billing {
	case agent.BillingSubscription:
		parts = append(parts, "subscription")
	case agent.BillingAPIKey:
		parts = append(parts, "API key")
	}
	if len(parts) == 0 {
		return ""
	}
	env := def.Environment
	if a.Workdir != "" {
		env.Workdir = a.Workdir // the directory the agent reports beats the configured one
	}
	parts = append(parts, env.String())
	return "🤖 " + strings.Join(parts, " · ")
}

// FormatCost renders what a result cost: the charge for API-key runs. A
// subscription login has no charge to show — the estimate in dollars is
// not what the human pays — so it renders nothing; the plan's usage
// arrives separately (UsageMeter, FormatUsage). The stored event keeps
// the estimate in Cost either way.
func FormatCost(a *agent.Event) string {
	if a.Billing == agent.BillingSubscription {
		return ""
	}
	return fmt.Sprintf("$%.3f", a.Cost)
}

// UsageMeter renders a usage event as a line of meters, one per window —
// "📊 5h ▰▰▱▱▱▱▱▱▱▱ 15% · 7d ▰▰▰▱▱▱▱▱▱▱ 28%" — and, for a window that is
// nearly spent, when it resets. "" when the event carries no windows.
func UsageMeter(a *agent.Event) string {
	if s := formatUsage(a, meter); s != "" {
		return "📊 " + s
	}
	return ""
}

// FormatUsage is the compact form of UsageMeter, percentages only:
// "5h 15% · 7d 28%".
func FormatUsage(a *agent.Event) string {
	return formatUsage(a, nil)
}

// formatUsage is the windows of a usage event, percent used each, joined
// with " · "; bar, when given, draws something in front of every
// percentage.
func formatUsage(a *agent.Event, bar func(used float64) string) string {
	if a.Usage == nil || len(a.Usage.Windows) == 0 {
		return ""
	}
	parts := make([]string, 0, len(a.Usage.Windows))
	for _, w := range a.Usage.Windows {
		part := w.Name
		if bar != nil {
			part += " " + bar(w.Used)
		}
		part += fmt.Sprintf(" %.0f%%", w.Used)
		if w.Used >= usageResetAt && w.ResetsAt.After(a.At) {
			part += " (resets in " + formatSpan(w.ResetsAt.Sub(a.At)) + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " · ")
}

// formatSpan is formatDuration for spans that may run to days: "2d 3h",
// else "1h20m".
func formatSpan(d time.Duration) string {
	if d < 24*time.Hour {
		return formatDuration(d)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, hours)
}

// meter draws percent used as meterCells cells: ▰ filled, ▱ empty.
func meter(used float64) string {
	filled := int(math.Round(used / 100 * meterCells))
	filled = max(0, min(meterCells, filled))
	return strings.Repeat("▰", filled) + strings.Repeat("▱", meterCells-filled)
}

// meterCells is a meter's width; usageResetAt is the percent used from
// which a window's reset time is worth a few more characters.
const (
	meterCells   = 10
	usageResetAt = 80
)

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

// truncate cuts s to n bytes on a rune boundary, marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
