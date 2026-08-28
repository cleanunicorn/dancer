package coordinator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	envlocal "github.com/cleanunicorn/dispatch/internal/environment/local"
	execlocal "github.com/cleanunicorn/dispatch/internal/executor/local"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/store/sqlite"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/surface/chat"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// TestLiveStatus: while a task runs the thread gets heartbeats (a keyed
// status line) and its root message carries ⏳; a permission prompt
// swaps that for ✋ and the status line leaves; the answer brings both
// back; the end of the turn swaps the mark for 📬 — the thread now waits
// for its next message — and a follow-up brings ⏳ back.
func TestLiveStatus(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.Heartbeat = 20 * time.Millisecond
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/1.0")
	tr.say(th, "run coder do the thing")
	status := tr.waitFor(t, th, "⏳ starting")
	if status.Key == "" {
		t.Fatalf("status line is not keyed: %+v", status)
	}
	prompt := tr.waitFor(t, th, "wants to run")
	waitReactions(t, tr, th, waitingReaction)
	// The status line was taken down before the prompt went out.
	tr.mu.Lock()
	var removed bool
	for _, o := range tr.out {
		if o.Thread == th && o.Key != "" && o.Text == "" {
			removed = true
		}
		if o.Prompt != nil && !removed {
			t.Errorf("prompt posted while the status line was still up")
		}
	}
	tr.mu.Unlock()

	tr.decide(th, prompt.Prompt.ID, "allow")
	done := tr.waitFor(t, th, "✅ done")
	if !strings.Contains(done.Text, "tool call") && !strings.Contains(done.Text, "s ·") {
		t.Errorf("closing line carries no duration: %q", done.Text)
	}
	waitReactions(t, tr, th, answeredReaction)

	// A heartbeat is not an event worth logging.
	var n int
	st.Replay(ctx, 0, func(r store.Record) error {
		if r.Kind == "outbound" && strings.Contains(string(r.Payload), "⏳") {
			n++
		}
		return nil
	})
	if n > 2 {
		t.Errorf("%d status redraws were written to the event log", n)
	}

	// A follow-up takes the 📬 down for ⏳ (the fake answers too fast to
	// catch it up) and the end of that turn puts it back.
	tr.say(th, "and again")
	tr.waitForN(t, th, "✅ done", 2)
	waitReactions(t, tr, th, answeredReaction)
	if !tr.unreacted(th, answeredReaction) {
		t.Errorf("📬 stayed on while the follow-up ran")
	}
}

func waitReactions(t *testing.T, tr *fakeTransport, th transport.ThreadID, want ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := tr.reactions(th); strings.Join(got, ",") == strings.Join(want, ",") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("reactions on %s = %v, want %v", th, tr.reactions(th), want)
}
