package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/decider"
	"github.com/cleanunicorn/dancer/internal/environment"
	envlocal "github.com/cleanunicorn/dancer/internal/environment/local"
	execlocal "github.com/cleanunicorn/dancer/internal/executor/local"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/store/sqlite"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/surface/chat"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// stubDecider answers with a fixed verdict and records the questions asked.
type stubDecider struct {
	verdict decider.Verdict
	err     error

	mu    sync.Mutex
	asked []decider.Question
}

func (s *stubDecider) Name() string { return "stub" }
func (s *stubDecider) Decide(_ context.Context, q decider.Question) (decider.Verdict, error) {
	s.mu.Lock()
	s.asked = append(s.asked, q)
	s.mu.Unlock()
	if s.err != nil {
		return decider.Verdict{}, s.err
	}
	v := s.verdict
	v.By = s.Name()
	return decider.Validate(q, v)
}
func (s *stubDecider) questions() []decider.Question {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]decider.Question(nil), s.asked...)
}

// startWithDecider brings up a coordinator over a task left interrupted by
// an earlier run, which is the one decision dancer asks today.
func startWithDecider(t *testing.T, d decider.Decider, uses []string) (*fakeTransport, store.Store, transport.ThreadID) {
	t.Helper()
	th := transport.ThreadID("C-dev/30.0")
	def := agent.Definition{Name: "coder", Kind: "fake", Environment: environment.Spec{Kind: environment.KindLocal}}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutDefinition(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTask(context.Background(), store.TaskState{ID: "t-1", Transport: "slack", Thread: th,
		Definition: def, Session: "sess-1", Status: store.StatusInterrupted, Prompt: "do it"}); err != nil {
		t.Fatal(err)
	}
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.AutoResume = true
	c.Decider = d
	c.DeciderUses = uses
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)
	<-tr.ready
	return tr, st, th
}

func TestDeciderCanLeaveATaskForAHuman(t *testing.T) {
	d := &stubDecider{verdict: decider.Verdict{Action: "wait", Reason: "the same build failed twice already"}}
	tr, _, th := startWithDecider(t, d, []string{kindResume})

	tr.waitFor(t, th, "the same build failed twice already; reply in this thread to continue")
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, o := range tr.out {
		if o.Text == "⏯️ resuming session" {
			t.Fatalf("task was resumed against the verdict: %+v", tr.out)
		}
	}
}

func TestDeciderWordsTheResume(t *testing.T) {
	d := &stubDecider{verdict: decider.Verdict{Action: "continue",
		Prompt: "You were three files into the retry refactor; finish it and run the tests.",
		Reason: "clearly mid-refactor"}}
	tr, _, th := startWithDecider(t, d, []string{kindResume})

	tr.waitFor(t, th, "echo:You were three files into the retry refactor")

	// The question carries the task's facts, not the agent's own text.
	qs := d.questions()
	if len(qs) != 1 || qs[0].Kind != kindResume || qs[0].Task != "t-1" {
		t.Fatalf("questions = %+v", qs)
	}
	f, ok := qs[0].Facts.(resumeFacts)
	if !ok || f.Agent != "coder" || f.StatusAtStop != store.StatusInterrupted || !f.HasSession {
		t.Fatalf("facts = %+v", qs[0].Facts)
	}
	if qs[0].Static.Action != "continue" || qs[0].Static.Prompt != defaultResumePrompt {
		t.Fatalf("static answer = %+v", qs[0].Static)
	}
}

func TestDeciderFailureFallsBackToTheRules(t *testing.T) {
	d := &stubDecider{err: errors.New("claude: exit status 1")}
	tr, _, th := startWithDecider(t, d, []string{kindResume})
	tr.waitFor(t, th, "echo:"+defaultResumePrompt)
}

func TestDeciderIsNotAskedAboutKindsItMayNotAnswer(t *testing.T) {
	d := &stubDecider{verdict: decider.Verdict{Action: "wait", Reason: "no"}}
	tr, _, th := startWithDecider(t, d, nil) // no kinds allowed
	tr.waitFor(t, th, "echo:"+defaultResumePrompt)
	if qs := d.questions(); len(qs) != 0 {
		t.Fatalf("decider was asked anyway: %+v", qs)
	}
}

func TestDecisionsAreOnTheRecord(t *testing.T) {
	d := &stubDecider{verdict: decider.Verdict{Action: "wait", Reason: "stale request"}}
	tr, st, th := startWithDecider(t, d, []string{kindResume})
	tr.waitFor(t, th, "stale request")

	var found int
	err := st.Replay(context.Background(), 0, func(r store.Record) error {
		if r.Kind != "decision" || r.Task != "t-1" {
			return nil
		}
		found++
		for _, want := range []string{`"kind":"resume"`, `"action":"wait"`, `"by":"stub"`, "stale request", `"last_prompt":"do it"`} {
			if !strings.Contains(string(r.Payload), want) {
				t.Fatalf("decision record %s is missing %s", r.Payload, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("decision records = %d, want 1", found)
	}
}
