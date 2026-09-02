package chat

import (
	"strings"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// TestRenderWorkflowEvent: a workflow's moves are ordinary lines addressed
// to whoever started it — not keyed, so they never fight the agent's own
// status line for a place.
func TestRenderWorkflowEvent(t *testing.T) {
	s := New("chat", "slack", false)
	th := transport.ThreadID("C1/1.0")
	st := &workflow.State{Thread: th, User: "u1", Def: workflow.Definition{Name: "feature"}}
	msgs := s.Render(surface.Event{Kind: surface.EventWorkflow, Thread: th, Text: "▶️ 1/2 *implement*", Workflow: st})
	if len(msgs) != 1 || msgs[0].Thread != th || msgs[0].Key != "" || msgs[0].Mention != "u1" || msgs[0].Text != "▶️ 1/2 *implement*" {
		t.Fatalf("rendered = %+v", msgs)
	}
}

// TestStatusCarriesTheWorkflow: an answered `status` says where the run
// stands, the same line the run's own events carry.
func TestStatusCarriesTheWorkflow(t *testing.T) {
	s := New("chat", "slack", false)
	th := transport.ThreadID("C1/1.0")
	st := &workflow.State{Thread: th, Def: workflow.Definition{Name: "feature"},
		Status: workflow.RunRunning, Steps: []workflow.StepState{{Name: "implement", Status: workflow.StepRunning}}}
	msgs := s.Render(surface.Event{Kind: surface.EventReply, Thread: th, Text: "task `t1` — agent *coder*", Workflow: st})
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "🧗 workflow *feature* — ") {
		t.Fatalf("status line = %+v", msgs)
	}
	// A run that is over is history, not state: `status` leaves it off.
	done := *st
	done.Status = workflow.RunDone
	msgs = s.Render(surface.Event{Kind: surface.EventReply, Thread: th, Text: "task `t1` — agent *coder*", Workflow: &done})
	if len(msgs) != 1 || strings.Contains(msgs[0].Text, "workflow") {
		t.Fatalf("status line after the run = %+v", msgs)
	}
}
