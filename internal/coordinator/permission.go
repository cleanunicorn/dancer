package coordinator

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/decider"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/surface"
)

// The permission question. dancer's own answer is always actionAsk — a
// prompt reached a human today, so that is the behaviour a decider has to
// improve on, and the one every failure falls back to.
const (
	kindPermission = "permission"
	actionAllow    = "allow"
)

// permissionFacts describes one tool call waiting for approval. As with a
// resume, every string here was written by an agent: data to judge.
type permissionFacts struct {
	Agent            string   `json:"agent"`
	Environment      string   `json:"environment"`
	Workdir          string   `json:"workdir,omitempty"`
	Tool             string   `json:"tool"`
	Input            string   `json:"input,omitempty"`
	FullInput        string   `json:"full_input,omitempty"`
	OutsideWorkdir   bool     `json:"outside_workdir,omitempty"`
	LastHumanMessage string   `json:"last_human_message,omitempty"`
	RecentEvents     []string `json:"recent_events,omitempty"`
	AlreadyAllowed   int      `json:"already_allowed_this_task,omitempty"`
}

// decidePermission answers whether a tool call may run without waking
// anyone. It returns false unless three things line up: the call matches
// the operator's auto-allow list, the decider is allowed to answer this
// kind, and it says allow. Anything else — no list, no decider, a refusal,
// a timeout — leaves the prompt on its way to a human.
func (c *Coordinator) decidePermission(ctx context.Context, st store.TaskState, ev agent.Event) (decider.Verdict, bool) {
	if !c.autoAllowed(ev) {
		return decider.Verdict{}, false // outside the ceiling: never even asked
	}
	v := c.decide(ctx, decider.Question{
		Kind: kindPermission, Task: string(st.ID), Thread: string(st.Thread),
		Options: []string{actionAllow, actionAsk},
		Facts:   c.permissionFacts(ctx, st, ev),
		Static:  decider.Verdict{Action: actionAsk},
	})
	return v, v.Action == actionAllow
}

func (c *Coordinator) permissionFacts(ctx context.Context, st store.TaskState, ev agent.Event) permissionFacts {
	f := permissionFacts{
		Agent:       st.Definition.Name,
		Environment: string(st.Definition.Environment.Kind),
		Workdir:     st.Definition.Environment.Workdir,
		Tool:        ev.Tool,
		Input:       truncate(oneLine(summarizeInput(ev.ToolInput)), factParagraph),
		FullInput:   truncate(oneLine(marshalInput(ev.ToolInput)), factParagraph),
	}
	if p := pathArgument(ev.ToolInput); p != "" && f.Workdir != "" && !strings.HasPrefix(p, f.Workdir) {
		f.OutsideWorkdir = true
	}
	resume := c.factsForResume(ctx, st) // the same thread tail, read once
	f.LastHumanMessage = resume.LastHumanMessage
	f.RecentEvents = resume.RecentEvents
	c.mu.Lock()
	f.AlreadyAllowed = c.allowed[st.ID]
	c.mu.Unlock()
	return f
}

// autoAllowed reports whether a tool call is inside the auto-allow list:
// the ceiling an operator sets in config for what a decider may approve.
// Empty (the default) means nothing may be approved without a human.
func (c *Coordinator) autoAllowed(ev agent.Event) bool {
	for _, pattern := range c.AutoAllow {
		if matchesTool(pattern, ev) {
			return true
		}
	}
	return false
}

// matchesTool matches an auto-allow pattern against a tool call. The
// syntax is the one definitions already use for allowed_tools:
//
//	Read              any Read call
//	Bash(go test:*)   a Bash call whose command starts with "go test"
//	Bash(*)           any Bash call
//
// The prefix must end at a word boundary, so "go test" does not match
// "go testrunner-that-deletes-things".
func matchesTool(pattern string, ev agent.Event) bool {
	pattern = strings.TrimSpace(pattern)
	name, rest, hasArgs := strings.Cut(pattern, "(")
	if !strings.EqualFold(strings.TrimSpace(name), ev.Tool) {
		return false
	}
	if !hasArgs {
		return true
	}
	arg := strings.TrimSuffix(strings.TrimSpace(rest), ")")
	arg = strings.TrimSuffix(arg, ":*")
	arg = strings.TrimSuffix(arg, "*")
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return true
	}
	call := oneLine(summarizeInput(ev.ToolInput))
	if !strings.HasPrefix(call, arg) {
		return false
	}
	rem := call[len(arg):]
	return rem == "" || rem[0] == ' ' || rem[0] == '/' || rem[0] == ':'
}

// marshalInput is the whole tool input, for a decision that turns on a
// field summarizeInput does not pick (an Edit's replacement text, say).
func marshalInput(in map[string]any) string {
	if len(in) == 0 {
		return ""
	}
	b, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	return string(b)
}

// pathArgument is the filesystem path a tool call names, if it names one.
func pathArgument(in map[string]any) string {
	for _, k := range []string{"file_path", "path", "notebook_path"} {
		if v, ok := in[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// noteAutoAllowed tells the thread what ran without asking, and how to
// stop the task if that was the wrong call. Silent privilege is the one
// thing this feature must not have.
func (c *Coordinator) noteAutoAllowed(ctx context.Context, st store.TaskState, ev agent.Event, v decider.Verdict) {
	c.mu.Lock()
	c.allowed[st.ID]++
	n := c.allowed[st.ID]
	c.mu.Unlock()
	what := ev.Tool
	if in := oneLine(summarizeInput(ev.ToolInput)); in != "" {
		what += " " + in
	}
	text := "🔓 allowed automatically: `" + truncate(what, factLine) + "`"
	if v.Reason != "" {
		text += " — " + v.Reason
	}
	text += "\n_say `cancel` to stop this task_"
	c.Log.Info("tool call auto-allowed", "task", st.ID, "tool", ev.Tool, "reason", v.Reason, "allowed_so_far", n)
	tt := st
	c.emitTo(ctx, st.Transport, surface.Event{Kind: surface.EventReply, Thread: st.Thread, TaskID: st.ID, Task: &tt, Text: text})
}
