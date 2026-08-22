package coordinator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/decider"
	"github.com/cleanunicorn/dancer/internal/executor"
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
	f.OutsideWorkdir = outsideWorkdir(pathArgument(ev.ToolInput), f.Workdir)
	resume := c.factsForResume(ctx, st) // the same thread tail, read once
	f.LastHumanMessage = resume.LastHumanMessage
	f.RecentEvents = resume.RecentEvents
	f.AlreadyAllowed = c.autoAllowedSoFar(ctx, st.ID)
	return f
}

// autoAllowedSoFar counts the tool calls a decider has approved for a task,
// from the log, so the count survives a restart along with the session.
func (c *Coordinator) autoAllowedSoFar(ctx context.Context, id executor.TaskID) int {
	n := 0
	for _, v := range c.taskVerdicts(ctx, id, c.maxDecisions()) {
		if v.Question.Kind == kindPermission && v.Verdict.Action == actionAllow {
			n++
		}
	}
	return n
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
//	Read(/repo/*)     a Read call under /repo/
//	Bash(go test:*)   a Bash call running "go test …"
//	Bash(*)           any Bash call
//
// A prefix must end at a boundary, so "go test" does not match
// "go testrunner-that-deletes-things". A command is matched in every
// segment a shell would run — `go test ./... && rm -rf /` is three words
// of the operator's prefix followed by something they never allowed, so
// each segment has to match; a command that hides code in a substitution,
// or writes somewhere through a redirection, matches nothing at all. A
// path is cleaned first, so `/repo/../etc/shadow` is not "under /repo".
// The pattern itself is parsed by decider.ParseAllow, the same parser
// config validation ran; one it rejects matches nothing here.
func matchesTool(pattern string, ev agent.Event) bool {
	a, err := decider.ParseAllow(pattern)
	if err != nil || !strings.EqualFold(a.Tool, ev.Tool) {
		return false
	}
	if a.Any {
		return true
	}
	raw := rawInput(ev.ToolInput) // not flattened: newlines separate commands
	if strings.TrimSpace(raw) == "" {
		return false
	}
	if isShellTool(ev.Tool) {
		raw = harmlessRedirects.Replace(raw) // 2>&1 is not a second command
		if hidesCode(raw) {
			return false // a substitution can run anything; no prefix covers it
		}
		for _, seg := range shellSegments(raw) {
			if !hasPrefixAtBoundary(seg, a.Arg) {
				return false
			}
		}
		return true
	}
	return hasPrefixAtBoundary(cleanPath(oneLine(raw)), cleanPath(a.Arg))
}

// cleanPath normalises a path argument (or a path prefix) before it is
// compared: `..` and doubled separators resolved, a trailing slash gone.
// Only absolute paths are touched — a relative one cannot be resolved
// here and is left to fail the comparison against an absolute prefix.
func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) {
		return p
	}
	return filepath.Clean(p)
}

// outsideWorkdir reports whether an absolute path argument resolves to
// somewhere outside the working directory. A relative path is not known
// to be outside (the agent resolves it against the workdir), and a
// sibling such as /srv/repo-other is not inside /srv/repo.
func outsideWorkdir(p, workdir string) bool {
	if p == "" || workdir == "" || !filepath.IsAbs(p) || !filepath.IsAbs(workdir) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(workdir), filepath.Clean(p))
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isShellTool reports whether a tool's input is a command line rather than
// a plain argument, and so has to be read the way a shell would read it.
func isShellTool(tool string) bool {
	switch tool {
	case "Bash", "BashOutput", "Shell", "Run":
		return true
	}
	return false
}

// hidesCode reports whether a command can do something the prefix cannot
// be checked against: a substitution, an eval of piped input, a here-doc,
// or a redirection — `go test ./... > .git/HEAD` starts with the
// operator's prefix and overwrites a file they never named. The
// redirections that only join or discard the streams a command already
// has (2>&1, >/dev/null) are the exception: they cannot write a file.
func hidesCode(cmd string) bool {
	for _, s := range []string{"$(", "`", "${", "<(", ">(", "eval "} {
		if strings.Contains(cmd, s) {
			return true
		}
	}
	return strings.ContainsAny(cmd, "<>")
}

var harmlessRedirects = strings.NewReplacer(
	"2>&1", "", "1>&2", "", "&>/dev/null", "", "2>/dev/null", "", ">/dev/null", "",
	"&> /dev/null", "", "2> /dev/null", "", "> /dev/null", "", "</dev/null", "", "< /dev/null", "")

// shellSegments splits a command on the operators that start a new command.
func shellSegments(cmd string) []string {
	fields := strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ';' || r == '|' || r == '&' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return []string{cmd}
	}
	return out
}

// hasPrefixAtBoundary reports whether s starts with prefix and the prefix
// ends where a token does — so "go test" covers "go test ./..." but not
// "go testrunner". A prefix that already ends in a separator ("/repo/")
// needs no boundary of its own.
func hasPrefixAtBoundary(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	rem := s[len(prefix):]
	if rem == "" {
		return true
	}
	switch prefix[len(prefix)-1] {
	case '/', ':', '-', '=', '_', '.':
		return true // the pattern named a directory or an option prefix
	}
	switch rem[0] {
	case ' ', '\t', '/', ':', '=':
		return true
	}
	return false
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
// thing this feature must not have — so the notice goes to every surface,
// the same way the prompt it replaced would have: an approvals feed sees
// what ran instead of what it would have been asked.
func (c *Coordinator) noteAutoAllowed(ctx context.Context, st store.TaskState, ev agent.Event, v decider.Verdict) {
	what := ev.Tool
	if in := oneLine(summarizeInput(ev.ToolInput)); in != "" {
		what += " " + in
	}
	text := "🔓 allowed automatically: `" + truncate(what, factLine) + "`"
	if v.Reason != "" {
		text += " — " + v.Reason
	}
	text += "\n_say `cancel` to stop this task_"
	c.Log.Info("tool call auto-allowed", "task", st.ID, "tool", ev.Tool, "reason", v.Reason)
	tt, e := st, ev
	c.broadcast(ctx, surface.Event{Kind: surface.EventAllowed, Thread: st.Thread, TaskID: st.ID, Task: &tt, Agent: &e, Text: text})
}
