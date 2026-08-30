package feed

import (
	"strings"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/work"
)

const feedThread = transport.ThreadID("C-ops/1.0")

// TestFeedMirrorsTheWork: someone watching the ops channel is deciding
// whether to go and look at exactly the same evidence as someone in the
// thread, so a finished turn carries the pull request there too — whether
// the turn ended well or badly.
func TestFeedMirrorsTheWork(t *testing.T) {
	s := New("feed", "slack", feedThread, false)
	w := &work.State{
		Repo:   "o/r",
		Branch: "fix-47",
		PR:     &work.Ref{Repo: "o/r", Kind: work.KindPR, Number: 51, URL: "https://github.com/o/r/pull/51"},
	}
	for _, tc := range []struct {
		name string
		ev   agent.Event
		lead string
	}{
		{"a turn that finished", agent.Event{Type: agent.EventResult, Text: "opened it"}, "✅"},
		{"a turn that failed", agent.Event{Type: agent.EventError, Text: "the build fell over"}, "❌"},
	} {
		ev := tc.ev
		out := s.Render(surface.Event{Kind: surface.EventAgent, TaskID: "t1", Task: &store.TaskState{}, Agent: &ev, Work: w})
		if len(out) != 1 {
			t.Fatalf("%s: %d messages, want one", tc.name, len(out))
		}
		if out[0].Thread != feedThread {
			t.Errorf("%s: posted to %s, not the feed's own thread", tc.name, out[0].Thread)
		}
		for _, want := range []string{tc.lead, "🔀 <https://github.com/o/r/pull/51|#51>", "🌿 `fix-47`"} {
			if !strings.Contains(out[0].Text, want) {
				t.Errorf("%s: missing %q:\n%s", tc.name, want, out[0].Text)
			}
		}
	}
}

// TestFeedWithoutWork: a task that never went near a repository reads in
// the ops channel exactly as it did before the overview existed.
func TestFeedWithoutWork(t *testing.T) {
	s := New("feed", "slack", feedThread, false)
	ev := agent.Event{Type: agent.EventResult, Text: "summarised the notes"}
	out := s.Render(surface.Event{Kind: surface.EventAgent, TaskID: "t2", Task: &store.TaskState{}, Agent: &ev})
	if len(out) != 1 || strings.Contains(out[0].Text, "\n") {
		t.Errorf("an overview was added to a task with no work to show: %+v", out)
	}
}
