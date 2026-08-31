package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// TestDoneLine: what the closing line says beyond "done" — the tools the
// turn reached for, whether any file came out changed, the model, the
// cost. A bare count answered neither of the first two.
func TestDoneLine(t *testing.T) {
	start := time.Unix(0, 0)
	end := start.Add(72 * time.Second)

	use := func(tool, path string) *agent.Event {
		in := map[string]any{}
		if path != "" {
			in["file_path"] = path
		}
		return &agent.Event{Type: agent.EventToolUse, Tool: tool, ToolInput: in}
	}
	turnOf := func(evs ...*agent.Event) *turn {
		tn := &turn{started: start}
		for _, e := range evs {
			tn.note(e)
		}
		return tn
	}

	for _, tc := range []struct {
		name  string
		t     *turn
		model string
		res   agent.Event
		want  string
	}{
		{
			"a restart left no turn to report on",
			nil,
			"sonnet-5",
			agent.Event{Type: agent.EventResult, Billing: agent.BillingSubscription},
			"✅ done · sonnet-5",
		},
		{
			// One kind of tool: the count already said it, so no breakdown.
			"one tool, said once",
			turnOf(use(agent.ToolRead, "/a.go"), use(agent.ToolRead, "/b.go")),
			"",
			agent.Event{Type: agent.EventResult, Billing: agent.BillingSubscription},
			"✅ done · 1m12s · 2 tool calls",
		},
		{
			"what it reached for, and what came out changed",
			turnOf(
				use(agent.ToolRead, "/a.go"), use(agent.ToolRead, "/b.go"), use(agent.ToolRead, "/c.go"),
				use(agent.ToolEdit, "/a.go"), use(agent.ToolEdit, "/a.go"), use(agent.ToolWrite, "/new.go"),
				use(agent.ToolBash, ""),
			),
			"opus-5",
			agent.Event{Type: agent.EventResult, Cost: 0.0125, Billing: agent.BillingAPIKey},
			"✅ done · 1m12s · 7 tool calls (3 Read, 2 Edit, 1 Bash) · 2 files changed · opus-5 · $0.013",
		},
		{
			// An MCP tool's "mcp__<server>__<tool>" would be the whole
			// line, so the breakdown keeps the tool's own name.
			"an MCP tool is named by its tool",
			turnOf(use("mcp__github__create_issue", ""), use(agent.ToolBash, "")),
			"",
			agent.Event{Type: agent.EventResult, Billing: agent.BillingSubscription},
			"✅ done · 1m12s · 2 tool calls (1 Bash, 1 create_issue)",
		},
	} {
		if got := doneLine(tc.t, tc.model, &tc.res, end); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// TestDoneLineNamesTheModelFromTheInit: the model on the closing line is
// the one the CLI announced when the session started, not one read off
// the result — a driver puts a model there only when the turn carried
// out a "/model" switch, so reading the result alone left the line
// naming a model on no turn but that one. A follow-up turn has no init
// of its own, so the name has to outlive the turn that heard it.
func TestDoneLineNamesTheModelFromTheInit(t *testing.T) {
	s := New("chat", "slack", false)
	th := transport.ThreadID("C1/1.0")
	task := &store.TaskState{Definition: agent.Definition{Name: "coder", Kind: agent.KindClaude,
		Environment: environment.Spec{Kind: environment.KindLocal}}}
	render := func(a *agent.Event) []transport.Outbound {
		return lines(s.Render(surface.Event{Kind: surface.EventAgent, Thread: th, Task: task, Agent: a}))
	}
	// done ends a turn. switched is the model a "/model" the CLI carried
	// out reported, which is the only thing a driver puts on a result.
	done := func(switched string) string {
		out := render(&agent.Event{Type: agent.EventResult, Model: switched, Billing: agent.BillingSubscription})
		if len(out) != 1 {
			t.Fatalf("got %d closing messages", len(out))
		}
		return out[0].Text
	}

	render(&agent.Event{Type: agent.EventInit, Model: "claude-haiku-4-5-20251001", Mode: agent.PermissionManual})
	if got := done(""); !strings.Contains(got, "claude-haiku-4-5-20251001") {
		t.Errorf("first turn: %q names no model", got)
	}
	// The next turn is a follow-up into the live process: no init, and the
	// model is still the one the session reported.
	render(&agent.Event{Type: agent.EventText, Text: "on it"})
	if got := done(""); !strings.Contains(got, "claude-haiku-4-5-20251001") {
		t.Errorf("follow-up turn: %q names no model", got)
	}
	// A "/model" switch the CLI carried out is reported on the result and
	// stands until an init says otherwise.
	if got := done("opus"); !strings.Contains(got, "opus") {
		t.Errorf("after a switch: %q", got)
	}
	if got := done(""); !strings.Contains(got, "opus") {
		t.Errorf("the switch did not stick: %q", got)
	}
	// A sub-agent's init is another session and must not rename this one.
	render(&agent.Event{Type: agent.EventInit, Model: "claude-haiku-4-5-20251001", ParentID: "toolu_1"})
	if got := done(""); !strings.Contains(got, "opus") {
		t.Errorf("a sub-agent renamed the session: %q", got)
	}
}
