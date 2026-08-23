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
	def := agent.Definition{Name: "coder", Kind: "fake",
		Environment: environment.Spec{Kind: environment.KindLocal, Workdir: t.TempDir()}}
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

// TestDeciderAnswerOutsideTheOptionsIsDiscarded: the seam enforces the
// contract, not each implementation. A decider that returns a nil error
// with an action nobody offered must not reach the task.
func TestDeciderAnswerOutsideTheOptionsIsDiscarded(t *testing.T) {
	d := &rawDecider{verdict: decider.Verdict{Action: "allow_all",
		Prompt: "rm -rf /", Reason: "trust me", By: "rogue"}}
	tr, _, th := startWithDecider(t, d, []string{kindResume})
	tr.waitFor(t, th, "echo:"+defaultResumePrompt) // the rules answered
}

// rawDecider skips decider.Validate, the way a third-party implementation
// could. Only the coordinator stands between it and the task.
type rawDecider struct{ verdict decider.Verdict }

func (r *rawDecider) Name() string { return "raw" }
func (r *rawDecider) Decide(context.Context, decider.Question) (decider.Verdict, error) {
	return r.verdict, nil
}

// TestBudgetIsSpentOnlyOnQuestionsADeciderSaw: a question answered by the
// rules — wrong kind, no decider — must not use up the task's budget, or
// tool prompts would quietly switch the resume decider off.
func TestBudgetIsSpentOnlyOnQuestionsADeciderSaw(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := New(st, nil, nil, nil, nil)
	c.Decider = &stubDecider{verdict: decider.Verdict{Action: actionWait}}
	c.DeciderUses = []string{kindResume}
	ctx := context.Background()

	q := decider.Question{Kind: kindPermission, Task: "t-1",
		Options: []string{actionAllow, actionAsk}, Static: decider.Verdict{Action: actionAsk}}
	for i := 0; i < 50; i++ {
		if v := c.decide(ctx, q); v.Action != actionAsk || v.By != "static" {
			t.Fatalf("permission answer = %+v", v)
		}
	}
	if spent := len(c.taskVerdicts(ctx, "t-1", 0)); spent != 0 {
		t.Fatalf("budget spent on questions no decider saw: %d", spent)
	}
	resume := decider.Question{Kind: kindResume, Task: "t-1",
		Options: []string{actionContinue, actionWait}, Static: decider.Verdict{Action: actionContinue}}
	if v := c.decide(ctx, resume); v.Action != actionWait {
		t.Fatalf("the resume decider was disabled by tool prompts: %+v", v)
	}
}

// TestFailedDeciderDoesNotSpendTheBudget: a backend that is down answers
// nothing, so nothing is charged — once it is back, it is asked again.
func TestFailedDeciderDoesNotSpendTheBudget(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := New(st, nil, nil, nil, nil)
	d := &stubDecider{err: errors.New("401 unauthorized")}
	c.Decider = d
	c.DeciderUses = []string{kindResume}
	c.MaxDecisionsPerTask = 5
	ctx := context.Background()

	q := decider.Question{Kind: kindResume, Task: "t-1",
		Options: []string{actionContinue, actionWait}, Static: decider.Verdict{Action: actionContinue}}
	for i := 0; i < 20; i++ {
		if v := c.decide(ctx, q); v.Action != actionContinue || v.By != "static" {
			t.Fatalf("answer while the decider is down = %+v", v)
		}
	}
	if spent := len(c.taskVerdicts(ctx, "t-1", 0)); spent != 0 {
		t.Fatalf("failed calls were charged to the task: %d", spent)
	}
	d.err = nil
	d.verdict = decider.Verdict{Action: actionWait}
	if v := c.decide(ctx, q); v.Action != actionWait {
		t.Fatalf("the decider stayed switched off after it recovered: %+v", v)
	}
	if spent := len(c.taskVerdicts(ctx, "t-1", 0)); spent != 1 {
		t.Fatalf("an answered question should be the only one on the record: %d", spent)
	}
}

func TestDecisionsAreOnTheRecord(t *testing.T) {
	d := &stubDecider{verdict: decider.Verdict{Action: "wait", Reason: "stale request"}}
	tr, st, th := startWithDecider(t, d, []string{kindResume})
	tr.waitFor(t, th, "stale request")

	var found int
	err := st.Replay(context.Background(), 0, func(r store.Record) error {
		if r.Kind != recordVerdict || r.Task != "t-1" {
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

// TestBudgetSurvivesARestart: the per-task budget is a projection of the
// log, so a new coordinator on the same store starts where the old one
// stopped rather than handing the task a fresh set of questions.
func TestBudgetSurvivesARestart(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	q := decider.Question{Kind: kindResume, Task: "t-1", Thread: "th",
		Options: []string{actionContinue, actionWait}, Static: decider.Verdict{Action: actionContinue}}

	first := New(st, nil, nil, nil, nil)
	first.Decider = &stubDecider{verdict: decider.Verdict{Action: actionWait}}
	first.DeciderUses = []string{kindResume}
	first.MaxDecisionsPerTask = 3
	for i := 0; i < 2; i++ {
		if v := first.decide(ctx, q); v.Action != actionWait {
			t.Fatalf("decision %d = %+v", i, v)
		}
	}

	second := New(st, nil, nil, nil, nil)
	second.Decider = &stubDecider{verdict: decider.Verdict{Action: actionWait}}
	second.DeciderUses = []string{kindResume}
	second.MaxDecisionsPerTask = 3
	if v := second.decide(ctx, q); v.Action != actionWait || v.By != "stub" {
		t.Fatalf("third question should still reach the decider: %+v", v)
	}
	if v := second.decide(ctx, q); v.Action != actionContinue || v.By != "static" {
		t.Fatalf("fourth question should hit the budget carried over the restart: %+v", v)
	}
	if got := len(second.taskVerdicts(ctx, "t-1", 0)); got != 3 {
		t.Fatalf("verdicts on record = %d, want 3", got)
	}
}

// A refusal by the agent's own policy is a fact the decider sees, and it
// closes the refused call so it is not reported as still in flight.
func TestFactsRecordAPolicyRefusal(t *testing.T) {
	ev := agent.Event{Type: agent.EventToolDenied, Tool: "Bash", ToolID: "1", Text: "Permission for this action was denied by the Claude Code auto mode classifier. Reason: Blocked by classifier."}
	got := describeEvent(ev)
	if !strings.HasPrefix(got, "tool_denied Bash: Permission") {
		t.Errorf("describeEvent = %q", got)
	}
}
