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
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// workflowFixture is a coordinator with the fake agent, a chat surface and
// the given workflow definitions, ready to hear `workflow <name> …`.
func workflowFixture(t *testing.T, tr *fakeTransport, ag fakeAgent, defs ...workflow.Definition) (*Coordinator, store.Store, context.CancelFunc) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	return workflowFixtureOn(t, st, tr, ag, defs...)
}

// workflowFixtureOn is workflowFixture over an existing store, so a restart
// test can replay the same database.
func workflowFixtureOn(t *testing.T, st store.Store, tr *fakeTransport, ag fakeAgent, defs ...workflow.Definition) (*Coordinator, store.Store, context.CancelFunc) {
	t.Helper()
	return workflowFixtureTuned(t, st, tr, ag, nil, defs...)
}

// workflowFixtureTuned is workflowFixtureOn with a last look at the
// coordinator before it starts. Anything a test wants to set — a planner,
// a config writer — has to be set here: once Run is going the fields
// belong to its goroutine, and assigning one from the test is a race.
func workflowFixtureTuned(t *testing.T, st store.Store, tr *fakeTransport, ag fakeAgent, tune func(*Coordinator), defs ...workflow.Definition) (*Coordinator, store.Store, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": ag}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	ex.DrainTimeout = time.Second
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	c.DrainTimeout = time.Second
	c.Workflows = defs
	if tune != nil {
		tune(c)
	}
	go c.Run(ctx)
	<-tr.ready
	return c, st, cancel
}

// featureFlow is the example that prompted the feature: implement, then a
// human says whether it goes on.
func featureFlow() workflow.Definition {
	return workflow.Definition{
		Name:        "feature",
		Description: "implement then approve",
		Steps: []workflow.Step{
			{Name: "implement", Agent: "coder", Prompt: "{{.Ask}}", Expect: workflow.ExpectPR},
			{Name: "approve", Gate: "{{.PR}} opened. Merge it?"},
		},
	}
}

// TestWorkflowRunsItsStepsAndGates: a named workflow runs its first step as
// a real agent turn, judges it on the pull request the log caught the step
// opening, and stops on its gate until somebody answers.
func TestWorkflowRunsItsStepsAndGates(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	_, st, _ := workflowFixture(t, tr, fakeAgent{}, featureFlow())
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow feature ship it")
	tr.waitFor(t, th, "🧗 workflow *feature* started")
	tr.waitFor(t, th, "▶️ 1/2 *implement* — asking *coder*")
	// The turn runs the prompt the template rendered: {{.Ask}} is what
	// the human typed after the workflow's name. fakeAgent's "ship" turn
	// opens pull request 51 and the step is judged on exactly that; the
	// gate renders the pull request it carried.
	q := tr.waitFor(t, th, "Merge it?")
	if q.Prompt == nil || q.Prompt.ID == "" {
		t.Fatalf("gate prompt = %+v", q)
	}
	if !strings.Contains(q.Text, "pull/51") && !strings.Contains(q.Text, "#51") {
		t.Errorf("gate does not name the pull request the step opened:\n%s", q.Text)
	}
	tr.decide(th, q.Prompt.ID, "Yes")
	tr.waitFor(t, th, "🏁 workflow *feature*")

	// The run is over: no row left, and the log says what it did.
	runs, err := st.ListWorkflows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("workflow rows left behind: %+v", runs)
	}
	recs, err := st.ThreadRecordsOfKind(context.Background(), th, recordWorkflow, 10)
	if err != nil || len(recs) == 0 {
		t.Fatalf("workflow records = %v err=%v", recs, err)
	}
}

// TestWorkflowStepEvidenceIsItsOwnWindow: a step is graded on the records
// *it* produced. Step one opened the pull request; step two asked for one
// and opened nothing, and the sighting from step one must not satisfy it —
// which is the whole reason the window has a floor.
func TestWorkflowStepEvidenceIsItsOwnWindow(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	_, _, _ = workflowFixture(t, tr, fakeAgent{}, workflow.Definition{
		Name: "strict",
		Steps: []workflow.Step{
			{Name: "implement", Agent: "coder", Prompt: "{{.Ask}}", Expect: workflow.ExpectPR},
			{Name: "verify", Prompt: "check on {{.PR}}", Expect: workflow.ExpectPR, OnFail: workflow.OnFailStop},
		},
	})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow strict ship it")
	tr.waitFor(t, th, "▶️ 2/2 *verify*")
	// The verify turn is a warm follow-up that opens nothing; without the
	// floor, step one's pull request would pass it.
	tr.waitFor(t, th, "step *verify* failed")
	tr.waitFor(t, th, "stopping")
}

// TestWorkflowCancelStopsTheRun: `cancel` ends the run where it is, and the
// row goes with it — a restart cannot resume what a human stopped.
func TestWorkflowCancelStopsTheRun(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	_, st, _ := workflowFixture(t, tr, fakeAgent{}, workflow.Definition{
		Name: "plain",
		Steps: []workflow.Step{
			{Name: "work", Agent: "coder", Prompt: "{{.Ask}}"},
			{Name: "approve", Gate: "Done?"},
		},
	})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow plain slow work")
	tr.waitFor(t, th, "▶️ 1/2 *work*")
	tr.waitFor(t, th, "started with agent *coder*") // a turn is really under way
	tr.say(th, "cancel")
	tr.waitFor(t, th, "⏹️ workflow *plain*")
	runs, err := st.ListWorkflows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("stopped workflow left a row: %+v", runs)
	}
}

// TestWorkflowRefusesASecondRunOnTheThread: while a run holds the thread,
// starting another is refused — five agent turns is not something to double
// by accident.
func TestWorkflowRefusesASecondRunOnTheThread(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	_, _, _ = workflowFixture(t, tr, fakeAgent{}, workflow.Definition{
		Name:  "plain",
		Steps: []workflow.Step{{Name: "work", Agent: "coder", Prompt: "{{.Ask}}"}},
	})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow plain slow work")
	tr.waitFor(t, th, "▶️ 1/1 *work*")
	tr.waitFor(t, th, "started with agent *coder*") // the step's turn is really running
	tr.say(th, "workflow plain again")
	tr.waitFor(t, th, "a turn is running on this thread")
}

// TestWorkflowsWordListsAndStartsNothing: the word that lists is not the
// word that runs, and a name that does not exist says so.
func TestWorkflowsWordListsAndStartsNothing(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	_, _, _ = workflowFixture(t, tr, fakeAgent{}, featureFlow())
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflows")
	if o := tr.waitFor(t, th, "*feature*"); !strings.Contains(o.Text, "implement then approve") || !strings.Contains(o.Text, "workflow <name>") {
		t.Fatalf("workflows list = %q", o.Text)
	}
	tr.say(th, "workflow")
	tr.waitFor(t, th, "usage:")
	tr.say(th, "workflow nosuch do things")
	tr.waitFor(t, th, "no workflow named")
	tr.say(th, "workflow feature")
	tr.waitFor(t, th, "usage:")
}

// TestWorkflowResumesAGateAfterRestart: a run waiting on a human is asked
// again when dispatch comes back, from the row the shutdown kept.
func TestWorkflowResumesAGateAfterRestart(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	dbPath := filepath.Join(t.TempDir(), "c.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, cancel := workflowFixtureOn(t, st, tr, fakeAgent{}, workflow.Definition{
		Name:  "gate",
		Steps: []workflow.Step{{Name: "approve", Gate: "Merge it?"}},
	})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow gate ship it")
	q := tr.waitFor(t, th, "Merge it?")
	if q.Prompt == nil {
		t.Fatalf("gate prompt = %+v", q)
	}
	cancel() // SIGTERM with the gate open
	runs, err := st.ListWorkflows(context.Background())
	if err != nil || len(runs) != 1 || runs[0].Status != workflow.RunWaiting {
		t.Fatalf("rows after shutdown = %+v err=%v", runs, err)
	}

	st2, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	tr2 := &fakeTransport{name: "slack", ready: make(chan struct{})}
	workflowFixtureOn(t, st2, tr2, fakeAgent{})
	tr2.waitFor(t, th, "dispatch is back — the workflow still needs an answer")
	q2 := tr2.waitFor(t, th, "Merge it?")
	if q2.Prompt == nil {
		t.Fatalf("re-asked gate prompt = %+v", q2)
	}
	tr2.decide(th, q2.Prompt.ID, "Yes")
	tr2.waitFor(t, th, "🏁 workflow *gate*")
}

// TestWorkflowGradesAnInterruptedStepBeforeResending: a step whose turn a
// restart cut short is judged on the records that made it to the log — the
// turn did not finish, the step failed, and only the human's retry asks for
// it again.
func TestWorkflowGradesAnInterruptedStepBeforeResending(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	dbPath := filepath.Join(t.TempDir(), "c.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, cancel := workflowFixtureOn(t, st, tr, fakeAgent{}, workflow.Definition{
		Name:  "plain",
		Steps: []workflow.Step{{Name: "work", Agent: "coder", Prompt: "{{.Ask}}"}},
	})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow plain slow work")
	tr.waitFor(t, th, "▶️ 1/1 *work*")
	tr.waitFor(t, th, "started with agent *coder*") // the turn is really running
	cancel()                                        // SIGTERM mid-turn; the turn never finished

	st2, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	tr2 := &fakeTransport{name: "slack", ready: make(chan struct{})}
	workflowFixtureOn(t, st2, tr2, fakeAgent{})
	// The restart graded the window: no result ever landed in it, so the
	// step failed — it was not silently asked for again.
	q := tr2.waitFor(t, th, "Retry")
	if q.Prompt == nil {
		t.Fatalf("failed-step question = %+v", q)
	}
	tr2.decide(th, q.Prompt.ID, "Retry")
	tr2.waitFor(t, th, "▶️ 1/1 *work*") // the retry's own turn
	tr2.waitFor(t, th, "🏁 workflow *plain*")
}

// TestWorkflowTemplateSeesEarlierSteps: {{.Steps.<name>.Report}} carries the
// first step's report into the second step's prompt.
func TestWorkflowTemplateSeesEarlierSteps(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	_, _, _ = workflowFixture(t, tr, fakeAgent{}, workflow.Definition{
		Name: "report",
		Steps: []workflow.Step{
			{Name: "first", Agent: "coder", Prompt: "{{.Ask}}", Expect: workflow.ExpectReport},
			{Name: "second", Prompt: "The first step said: {{.Steps.first.Report}}"},
		},
	})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow report ship it")
	// Step two is a warm follow-up; its prompt is echoed back by the fake.
	echo := tr.waitFor(t, th, "echo:")
	if !strings.Contains(echo.Text, "The first step said: opened") {
		t.Errorf("step two did not see step one's report:\n%s", echo.Text)
	}
	tr.waitFor(t, th, "🏁 workflow *report*")
}

// TestWorkflowModelOverrideIsRestored pins the pin: a step that names a
// model gets it for its turn, and the thread's next turn asks for what was
// there before.
func TestWorkflowModelOverrideIsRestored(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	_, st, _ := workflowFixture(t, tr, fakeAgent{}, workflow.Definition{
		Name: "models",
		Steps: []workflow.Step{
			{Name: "pin", Agent: "coder", Model: "opus", Prompt: "{{.Ask}}"},
		},
	})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow models slow work")
	tr.waitFor(t, th, "▶️ 1/1 *pin*")
	tr.waitFor(t, th, "started with agent *coder*") // the pin is on a task that exists
	// The turn is stopped mid-flight; whatever the pin was by then, the
	// run's defer puts back what it replaced.
	tr.say(th, "cancel")
	tr.waitFor(t, th, "⏹️ workflow *models*")
	ts, err := st.LatestTaskForThread(context.Background(), th)
	if err != nil {
		t.Fatal(err)
	}
	if ts.ModelPin != "" {
		t.Errorf("model pin outlived its step: %q", ts.ModelPin)
	}
}

// TestWorkflowStepRunsTheAgentItNames: a step that names an agent gets that
// agent on the workflow's own thread too, not just in a thread of its own.
// A follow-up resumes whatever definition started the thread's session, so
// the named one has to be started in its place — and the progress line says
// which agent is being asked, which was a lie for as long as the step's
// `agent` was quietly ignored here.
func TestWorkflowStepRunsTheAgentItNames(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	_, st, _ := workflowFixture(t, tr, fakeAgent{}, workflow.Definition{
		Name: "handover",
		Steps: []workflow.Step{
			{Name: "one", Agent: "coder", Prompt: "{{.Ask}}"},
			{Name: "two", Agent: "other", Prompt: "carry on"},
		},
	})
	if err := st.PutDefinition(context.Background(), agent.Definition{Name: "other", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow handover ship it")
	tr.waitFor(t, th, "▶️ 2/2 *two* — asking *other*")
	tr.waitFor(t, th, "started with agent *other*")
	task, err := st.LatestTaskForThread(context.Background(), th)
	if err != nil {
		t.Fatal(err)
	}
	if task.Definition.Name != "other" {
		t.Errorf("step two ran on definition %q, not the one it named", task.Definition.Name)
	}
}

// TestStatusDuringARunReadsAPublishedCopy: `status` arrives on the inbox
// goroutine while the run is writing its state between steps, so what it
// renders is the copy the run published, never the state itself — whose
// Steps array the next step writes into. It is the race detector that
// makes this one fail.
func TestStatusDuringARunReadsAPublishedCopy(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	workflowFixture(t, tr, fakeAgent{}, workflow.Definition{
		Name: "several",
		Steps: []workflow.Step{
			{Name: "a", Agent: "coder", Prompt: "{{.Ask}}"},
			{Name: "b", Prompt: "again"},
			{Name: "c", Prompt: "again"},
			{Name: "d", Prompt: "again"},
		},
	})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "workflow several ship it")
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.waitFor(t, th, "🏁 workflow *several*")
	}()
	for {
		select {
		case <-done:
			tr.waitFor(t, th, "🧗 workflow *several*") // `status` answered with the run
			return
		default:
			tr.say(th, "status")
			time.Sleep(2 * time.Millisecond)
		}
	}
}
