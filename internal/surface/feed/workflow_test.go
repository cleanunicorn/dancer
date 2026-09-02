package feed

import (
	"strings"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// TestFeedMirrorsWorkflowMoves: the ops channel never falls behind a
// workflow any more than it falls behind a task — one line per move, with
// the thread it runs on.
func TestFeedMirrorsWorkflowMoves(t *testing.T) {
	s := New("ops", "slack", "C-ops/1.0", false)
	st := &workflow.State{Thread: "C-dev/1.0", Def: workflow.Definition{Name: "feature"}}
	msgs := s.Render(surface.Event{Kind: surface.EventWorkflow, Thread: "C-dev/1.0", Text: "▶️ 1/2 *implement*", Workflow: st})
	if len(msgs) != 1 || msgs[0].Thread != "C-ops/1.0" {
		t.Fatalf("rendered = %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "*feature*") || !strings.Contains(msgs[0].Text, "C-dev/1.0") {
		t.Fatalf("line = %q", msgs[0].Text)
	}
	if msgs := s.Render(surface.Event{Kind: surface.EventWorkflow, Thread: "C-dev/1.0"}); msgs != nil {
		t.Fatalf("event without a run rendered = %+v", msgs)
	}
}
