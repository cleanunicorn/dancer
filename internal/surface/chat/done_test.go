package chat

import (
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
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
		name string
		t    *turn
		res  agent.Event
		want string
	}{
		{
			"a restart left no turn to report on",
			nil,
			agent.Event{Type: agent.EventResult, Model: "sonnet-5", Billing: agent.BillingSubscription},
			"✅ done · sonnet-5",
		},
		{
			// One kind of tool: the count already said it, so no breakdown.
			"one tool, said once",
			turnOf(use(agent.ToolRead, "/a.go"), use(agent.ToolRead, "/b.go")),
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
			agent.Event{Type: agent.EventResult, Model: "opus-5", Cost: 0.0125, Billing: agent.BillingAPIKey},
			"✅ done · 1m12s · 7 tool calls (3 Read, 2 Edit, 1 Bash) · 2 files changed · opus-5 · $0.013",
		},
		{
			// An MCP tool's "mcp__<server>__<tool>" would be the whole
			// line, so the breakdown keeps the tool's own name.
			"an MCP tool is named by its tool",
			turnOf(use("mcp__github__create_issue", ""), use(agent.ToolBash, "")),
			agent.Event{Type: agent.EventResult, Billing: agent.BillingSubscription},
			"✅ done · 1m12s · 2 tool calls (1 Bash, 1 create_issue)",
		},
	} {
		if got := doneLine(tc.t, &tc.res, end); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
