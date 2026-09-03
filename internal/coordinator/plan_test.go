package coordinator

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/store/sqlite"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// planFixture is workflowFixture with a planner configured before the
// coordinator starts, which is the only moment a test may set one.
func planFixture(t *testing.T, tr *fakeTransport, tune func(*Coordinator), defs ...workflow.Definition) *Coordinator {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	c, _, _ := workflowFixtureTuned(t, st, tr, fakeAgent{}, func(c *Coordinator) {
		c.PlannerAgent = "coder"
		if tune != nil {
			tune(c)
		}
	}, defs...)
	return c
}

// Without a planner configured `plan` is not dispatch's word, so a message
// that starts with it is a message: it reaches the agent whole, the way
// "review the auth code" always has. Swallowing it for a config hint would
// have taken the sentence away from every user who never asked for
// planning.
func TestPlanWithoutAPlannerReachesTheAgent(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	_, st, _ := workflowFixture(t, tr, fakeAgent{})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "plan the migration before you touch anything")
	tr.waitFor(t, th, "started with agent *coder*")
	task, err := st.LatestTaskForThread(context.Background(), th)
	if err != nil {
		t.Fatal(err)
	}
	if task.Prompt != "plan the migration before you touch anything" {
		t.Errorf("the agent was asked %q, not what the human typed", task.Prompt)
	}
	// And the feature is still discoverable where somebody would look for
	// it, which is the list of what dispatch can run.
	tr.say(th, "workflows")
	tr.waitFor(t, th, "set `planner_agent` in `[server]`")
}

// The whole path: a message, a plan, the steps shown, a button, and the
// first step running as a real turn.
func TestPlanIsShownAndRunOnlyWhenApproved(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	planFixture(t, tr, nil)
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "plan ship it and then let me approve")
	tr.waitFor(t, th, "🧭 working out the steps")
	shown := tr.waitFor(t, th, "here is the plan")
	// The plan is described, not quoted: what each step is and what will
	// make it count as done.
	for _, want := range []string{"*implement*", "with coder", "a pull request was opened", "*approve*", "asks a human"} {
		if !strings.Contains(shown.Text, want) {
			t.Errorf("the plan does not mention %q:\n%s", want, shown.Text)
		}
	}
	if shown.Prompt == nil {
		t.Fatalf("the plan was posted without a button: %+v", shown)
	}
	tr.decide(th, shown.Prompt.ID, "Run")

	tr.waitFor(t, th, "🧗 planned workflow started")
	tr.waitFor(t, th, "▶️ 1/2 *implement* — asking *coder*")
	// The step passed on the pull request its own turn opened, and the
	// gate the plan asked for is put on that pull request.
	tr.waitFor(t, th, "✋ 2/2 *approve*")
	// The gate's own template rendered against the pull request the
	// previous step opened — not the "{{.PR}}" the plan was shown as.
	tr.waitFor(t, th, "Merge https://github.com/cleanunicorn/dispatch/pull/51?")
}

// Saying no starts nothing, and says how to keep the plan.
func TestPlanDeclinedStartsNothing(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := planFixture(t, tr, nil)
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "plan ship it")
	shown := tr.waitFor(t, th, "here is the plan")
	tr.decide(th, shown.Prompt.ID, "No")
	tr.waitFor(t, th, "not run")
	if wf := c.workflowOf(th); wf != nil {
		t.Errorf("a declined plan started a workflow: %+v", wf)
	}
}

// A plan that parses but names an agent nobody defined is refused by the
// same gate a config workflow goes through — before anything is started,
// and while there is still a human reading.
func TestPlanRefusedByValidationStartsNothing(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := planFixture(t, tr, nil)
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "plan nonsense please")
	out := tr.waitFor(t, th, "does not hold up")
	if !strings.Contains(out.Text, `no agent called "ghost"`) {
		t.Errorf("the refusal does not say why: %q", out.Text)
	}
	if wf := c.workflowOf(th); wf != nil {
		t.Errorf("a refused plan started a workflow: %+v", wf)
	}
}

// A planner that answered with prose and no JSON leaves the thread with a
// message and nothing started — the same as every other planner failure.
func TestPlanWithNoJSONStartsNothing(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := planFixture(t, tr, nil)
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "plan garbage please")
	tr.waitFor(t, th, "no plan:")
	if wf := c.workflowOf(th); wf != nil {
		t.Errorf("a plan that was never parsed started a workflow: %+v", wf)
	}
}

// `workflow save <name>` writes the thread's last plan back, so it can be
// started by name afterwards. Without a config file to write to it says so
// rather than pretending.
func TestSavePlanAsAWorkflow(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	var mu sync.Mutex
	var saved workflow.Definition
	c := planFixture(t, tr, func(c *Coordinator) {
		c.SaveWorkflow = func(_ context.Context, d workflow.Definition) error {
			mu.Lock()
			defer mu.Unlock()
			saved = d
			return nil
		}
	})
	th := transport.ThreadID("C-dev/1.0")

	tr.say(th, "plan ship it")
	shown := tr.waitFor(t, th, "here is the plan")
	tr.decide(th, shown.Prompt.ID, "No")
	tr.waitFor(t, th, "not run")

	tr.say(th, "workflow save shipit")
	tr.waitFor(t, th, "saved as workflow *shipit*")
	mu.Lock()
	got := saved
	mu.Unlock()
	if got.Name != "shipit" || len(got.Steps) != 2 {
		t.Fatalf("saved = %+v", got)
	}
	// It is startable by name straight away, not only after a restart.
	if _, ok := c.workflowDefinition("shipit"); !ok {
		t.Error("the saved workflow is not in the coordinator's list")
	}
	tr.say(th, "workflow save shipit")
	tr.waitFor(t, th, "already a workflow called")
}

// A plan is refused while the thread is busy, on the same grounds a named
// workflow is: it would be a second workflow on a thread that already has
// one, and the refusal must leave the first one exactly as it was.
func TestPlanIsRefusedOnABusyThread(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := planFixture(t, tr, nil, featureFlow())
	th := transport.ThreadID("C-dev/1.0")

	// "slow" is a turn that never ends on its own, so the workflow is
	// still on its first step when the plan arrives.
	tr.say(th, "workflow feature slow please")
	tr.waitFor(t, th, "▶️ 1/2 *implement* — asking *coder*")
	tr.say(th, "plan something else")
	out := tr.waitFor(t, th, "thread")
	if strings.Contains(out.Text, "working out the steps") {
		t.Fatalf("a plan was started on a thread already running a workflow: %q", out.Text)
	}
	if wf := c.workflowOf(th); wf == nil || wf.Def.Name != "feature" {
		t.Errorf("the running workflow did not survive the refusal: %+v", wf)
	}
}
