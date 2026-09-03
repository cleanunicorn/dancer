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
	"github.com/cleanunicorn/dispatch/internal/executor"
	execlocal "github.com/cleanunicorn/dispatch/internal/executor/local"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/store/sqlite"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/surface/chat"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// finishFixture is a thread that has done a piece of work: it opened
// pull request #51 on a branch, and its task is over — which is the state
// a human types `review` or `merge` in.
func finishFixture(t *testing.T, tr transport.Transport, ag fakeAgent) (*Coordinator, store.Store, transport.ThreadID, context.CancelFunc) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	th := transport.ThreadID("C-dev/1.0")
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTask(ctx, store.TaskState{ID: "t-1", Transport: "slack", Thread: th,
		Definition: agent.Definition{Name: "coder", Kind: "fake",
			Environment: environment.Spec{Kind: environment.KindLocal, Workdir: t.TempDir()}},
		Session: "s-1", Status: store.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	appendInbound(t, st, th, "run coder please fix #47")
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u1",
		ToolInput: map[string]any{"command": "git push -u origin fix-47"}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u2",
		ToolInput: map[string]any{"command": `gh pr create --body "Closes #47"`}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolResult, ToolID: "u2",
		Text: "https://github.com/cleanunicorn/dispatch/pull/51"})

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": ag}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	c.DrainTimeout = time.Second
	go c.Run(ctx)
	return c, st, th, cancel
}

// TestReviewOpensAThreadBesideIt: `review` reads the pull request out of
// the thread's own log, opens a thread next to it in the same channel and
// starts the same agent there with a review prompt — the two messages a
// human sends by hand at the end of every piece of work.
func TestReviewOpensAThreadBesideIt(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	_, st, th, cancel := finishFixture(t, tr, fakeAgent{})
	defer cancel()
	<-tr.ready

	tr.say(th, "review")
	tr.waitFor(t, th, "🔍 reviewing")

	// The new thread's root message is the prompt itself, so the review
	// thread reads as though someone had typed it.
	root := tr.waitFor(t, "C-dev/9.9", "Review pull request")
	for _, want := range []string{
		"https://github.com/cleanunicorn/dispatch/pull/51",
		"Do not push, commit or merge",
	} {
		if !strings.Contains(root.Text, want) {
			t.Errorf("review prompt is missing %q:\n%s", want, root.Text)
		}
	}
	tr.waitFor(t, "C-dev/9.9", "task `")

	// It runs the same agent as the thread it came from, on its own task.
	tasks, err := st.ListTasks(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var review *store.TaskState
	for i, task := range tasks {
		if task.Thread == "C-dev/9.9" {
			review = &tasks[i]
		}
	}
	if review == nil {
		t.Fatal("no task on the review thread")
	}
	if review.Definition.Name != "coder" {
		t.Errorf("review agent = %q, want coder", review.Definition.Name)
	}
	if !strings.Contains(review.Prompt, "pull/51") {
		t.Errorf("review prompt = %q", review.Prompt)
	}
}

// TestReviewWithoutAPullRequest says what to do instead, and names the
// branch when the thread got that far.
func TestReviewWithoutAPullRequest(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	th := transport.ThreadID("C-dev/2.0")
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u1",
		ToolInput: map[string]any{"command": "git push -u origin fix-47"}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolResult, ToolID: "u1",
		Text: "remote: https://github.com/cleanunicorn/dispatch"})
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	go c.Run(ctx)
	<-tr.ready

	tr.say(th, "review")
	reply := tr.waitFor(t, th, "nothing to review")
	if !strings.Contains(reply.Text, "fix-47") {
		t.Errorf("reply does not name the branch:\n%s", reply.Text)
	}
}

// TestMergeAsksTheAgentAndClosesOnTheLog: dispatch runs none of it. The
// agent commits, pushes, resolves and runs `gh pr merge` itself; dispatch
// reads the log back and closes the thread on gh's own confirmation.
func TestMergeAsksTheAgentAndClosesOnTheLog(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	_, st, th, cancel := finishFixture(t, tr, fakeAgent{})
	defer cancel()
	<-tr.ready

	tr.say(th, "merge")
	tr.waitFor(t, th, "🚢 asking the agent to merge")

	// Every step is the agent's, and the prompt says what not to do:
	// getting past a check is somebody's decision, not an obstacle.
	// (fakeAgent's resume echoes the prompt it was given.)
	prompt := tr.waitFor(t, th, "echo:")
	for _, want := range []string{
		"commit it and push",
		"mergeStateStatus",
		"resolve the conflicts and push",
		"gh pr merge https://github.com/cleanunicorn/dispatch/pull/51 --squash --delete-branch",
		"Do not try to get past a failing check",
	} {
		if !strings.Contains(prompt.Text, want) {
			t.Errorf("merge prompt is missing %q:\n%s", want, prompt.Text)
		}
	}

	tr.waitFor(t, th, "thread closed")
	waitReactions(t, &tr.fakeTransport, th, closedReaction)
	closed, err := st.ClosedThreads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0] != th {
		t.Fatalf("closed threads = %v, want [%s]", closed, th)
	}
}

// TestMergeAsksForTheMethodItWasGiven: the word after `merge` is gh's own
// flag and travels to the agent as one.
func TestMergeAsksForTheMethodItWasGiven(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	_, _, th, cancel := finishFixture(t, tr, fakeAgent{})
	defer cancel()
	<-tr.ready

	tr.say(th, "merge rebase")
	tr.waitFor(t, th, "(rebase)")
	if prompt := tr.waitFor(t, th, "echo:"); !strings.Contains(prompt.Text, "--rebase --delete-branch") {
		t.Errorf("merge prompt does not carry the method:\n%s", prompt.Text)
	}
}

// TestMergeLeavesTheThreadOpenWhenTheLogDoesNotSayMerged is the whole
// reason the close is read out of the log rather than taken from the
// agent's word: the turn ended, the agent reported, GitHub refused, and
// a thread closed on a refusal is a thread nobody comes back to.
func TestMergeLeavesTheThreadOpenWhenTheLogDoesNotSayMerged(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	_, st, th, cancel := finishFixture(t, tr, fakeAgent{merge: "refused"})
	defer cancel()
	<-tr.ready

	tr.say(th, "merge")
	tr.waitFor(t, th, "the log does not show")
	closed, err := st.ClosedThreads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 0 {
		t.Fatalf("thread was closed without a merge in the log: %v", closed)
	}
}

// TestMergeOfAnAlreadyMergedPullRequest asks nobody anything: the log
// already says it happened.
func TestMergeOfAnAlreadyMergedPullRequest(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	_, st, th, cancel := finishFixture(t, tr, fakeAgent{})
	defer cancel()
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "m-0",
		ToolInput: map[string]any{"command": "gh pr merge 51 --squash"}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolResult, ToolID: "m-0",
		Text: "✓ Squashed and merged pull request cleanunicorn/dispatch#51"})
	<-tr.ready

	tr.say(th, "merge")
	tr.waitFor(t, th, "is already merged")
}

func TestMergeWithoutAPullRequest(t *testing.T) {
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	go c.Run(ctx)
	<-tr.ready

	tr.say("C-dev/3.0", "merge")
	tr.waitFor(t, "C-dev/3.0", "nothing to merge")
}

// TestMergeWaitsForARunningTurn: the merging turn would queue behind
// what is running, and its report would be read before it had happened.
func TestMergeWaitsForARunningTurn(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	_, _, th, cancel := finishFixture(t, tr, fakeAgent{})
	defer cancel()
	<-tr.ready

	tr.say(th, "run coder do it")
	tr.waitFor(t, th, "wants to run") // live, waiting on a permission
	tr.say(th, "merge")
	tr.waitFor(t, th, "a turn is running on this thread")
}

// TestMergeRightAfterATurnEnds: a finished turn leaves its process warm
// for idle_timeout and the thread bound to it the whole time. That is not
// "a task is running" — it is exactly the moment someone reads the closing
// line and says `merge`, and refusing it there made the word useless for
// the ten minutes it is most wanted.
func TestMergeRightAfterATurnEnds(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	c, _, th, cancel := finishFixture(t, tr, fakeAgent{})
	defer cancel()
	<-tr.ready

	// A turn that runs to its end and leaves the process alive.
	tr.say(th, "run coder ship it")
	tr.waitFor(t, th, "✅ done")
	// The process is still warm and the thread still bound to it — which
	// is what `merge` used to be refused for.
	if !boundTo(c, th) {
		t.Fatal("the thread let go of its task before idle_timeout")
	}

	tr.say(th, "merge")
	tr.waitFor(t, th, "🚢 asking the agent to merge")
	tr.waitFor(t, th, "thread closed")
}

// TestMergeThatMergesNothingLeavesAClosedThreadClosed: `merge` reopens
// the thread it is about to run a turn in, and only then. Every other way
// out of it is a refusal, and a refusal that reopened the thread would
// take dispatch off mute in a thread nobody asked to restart — `status`
// has always put the tombstone back, and these have to as well.
func TestMergeThatMergesNothingLeavesAClosedThreadClosed(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	c, st, th, cancel := finishFixture(t, tr, fakeAgent{})
	defer cancel()
	<-tr.ready

	tr.say(th, "close")
	tr.waitFor(t, th, "thread closed")

	// #51 is already merged, so `merge` has nothing to do here.
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "m-0",
		ToolInput: map[string]any{"command": "gh pr merge 51 --squash"}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolResult, ToolID: "m-0",
		Text: "✓ Squashed and merged pull request cleanunicorn/dispatch#51"})

	tr.say(th, "merge")
	tr.waitFor(t, th, "is already merged")
	if !c.threadClosed(th) {
		t.Error("a `merge` that merged nothing reopened the thread")
	}
	closed, err := st.ClosedThreads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0] != th {
		t.Fatalf("closed threads = %v, want [%s]", closed, th)
	}
}

// TestMergeReopensTheThreadItActsIn: the other half of the same rule — a
// `merge` that does run a turn must take the thread off mute first, or
// execute's defer tombstones it and the agent's turn goes to nobody.
func TestMergeReopensTheThreadItActsIn(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	_, st, th, cancel := finishFixture(t, tr, fakeAgent{})
	defer cancel()
	<-tr.ready

	tr.say(th, "close")
	tr.waitFor(t, th, "thread closed")

	tr.say(th, "merge")
	tr.waitFor(t, th, "♻️ thread reopened")
	tr.waitFor(t, th, "🚢 asking the agent to merge")
	tr.waitForN(t, th, "thread closed", 2) // the `close` above, then the merge's own
	closed, err := st.ClosedThreads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0] != th {
		t.Fatalf("closed threads = %v, want [%s]", closed, th)
	}
}

// TestWaitForTurnIgnoresAnEndFromBeforeItAsked pins the other half of
// "wait for *its own* turn". Naming the task is not enough on the warm
// path, where the turn before the merge and the merge itself are the same
// task: the end that matters is the one announced after the prompt went
// out, so runMerge empties the waiter before sending and anything left
// over cannot satisfy it.
func TestWaitForTurnIgnoresAnEndFromBeforeItAsked(t *testing.T) {
	c := &Coordinator{turnEnds: map[transport.ThreadID][]chan turnEnd{}}
	th, task := transport.ThreadID("C-dev/1.0"), executor.TaskID("t-1")

	ends := c.awaitTurnEnd(th)
	defer c.dropTurnWaiter(th, ends)
	// The turn whose closing line the human read a second ago: the same
	// task, and below the floor the merge is about to write.
	c.turnEnded(th, turnEnd{Task: task, Seq: 41})
	const since = 42

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan turnWait, 1)
	go func() { got <- c.waitForTurn(ctx, task, since, ends, time.Minute) }()
	select {
	case w := <-got:
		t.Fatalf("waitForTurn returned %v on an end announced before its floor", w)
	case <-time.After(50 * time.Millisecond):
	}

	// The turn it actually asked for: past the floor.
	c.turnEnded(th, turnEnd{Task: task, Seq: 57})
	select {
	case w := <-got:
		if w != turnDone {
			t.Errorf("waitForTurn = %v, want turnDone", w)
		}
	case <-time.After(2 * time.Second):
		t.Error("waitForTurn never saw its own turn end")
	}
	cancel()
}

// TestWaitForTurnSettlesOnAProcessThatIsGone: a turn that never reached
// the agent writes no record to outrank the floor, so the only thing that
// can release its waiter is the task's process being gone.
func TestWaitForTurnSettlesOnAProcessThatIsGone(t *testing.T) {
	c := &Coordinator{turnEnds: map[transport.ThreadID][]chan turnEnd{}}
	th, task := transport.ThreadID("C-dev/1.1"), executor.TaskID("t-2")
	ends := c.awaitTurnEnd(th)
	defer c.dropTurnWaiter(th, ends)

	got := make(chan turnWait, 1)
	go func() { got <- c.waitForTurn(context.Background(), task, 42, ends, time.Minute) }()
	// Another task's process going away is not ours.
	c.turnEnded(th, turnEnd{Task: "t-other", Seq: 7, Done: true})
	select {
	case w := <-got:
		t.Fatalf("waitForTurn returned %v for another task's process", w)
	case <-time.After(50 * time.Millisecond):
	}
	c.turnEnded(th, turnEnd{Task: task, Seq: 7, Done: true})
	select {
	case w := <-got:
		if w != turnDone {
			t.Errorf("waitForTurn = %v, want turnDone", w)
		}
	case <-time.After(2 * time.Second):
		t.Error("waitForTurn never settled on a process that is gone")
	}
}

// TestWaitForTurnGivesUp: a turn that outlasts the caller's patience.
func TestWaitForTurnGivesUp(t *testing.T) {
	c := &Coordinator{turnEnds: map[transport.ThreadID][]chan turnEnd{}}
	th := transport.ThreadID("C-dev/1.2")
	ends := c.awaitTurnEnd(th)
	defer c.dropTurnWaiter(th, ends)
	if w := c.waitForTurn(context.Background(), "t-3", 42, ends, 10*time.Millisecond); w != turnSlow {
		t.Errorf("waitForTurn = %v, want turnSlow", w)
	}
}

// boundTo says the thread still holds a task, warm or running.
func boundTo(c *Coordinator, th transport.ThreadID) bool {
	_, ok := c.lookup(th)
	return ok
}

// TestMergeOfAPullRequestWithNoURL: the log knew a number and never a
// URL. "#51" on a command line is the start of a shell comment, so the
// prompt has to name the bare number.
func TestMergeOfAPullRequestWithNoURL(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	th := transport.ThreadID("C-dev/5.0")
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTask(ctx, store.TaskState{ID: "t-5", Transport: "slack", Thread: th,
		Definition: agent.Definition{Name: "coder", Kind: "fake",
			Environment: environment.Spec{Kind: environment.KindLocal, Workdir: t.TempDir()}},
		Session: "s-5", Status: store.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	// `gh pr view 51` acts on a number; no URL and no remote ever named.
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u1",
		ToolInput: map[string]any{"command": "gh pr view 51"}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolResult, ToolID: "u1", Text: "title:\tfix the status line"})
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	go c.Run(ctx)
	<-tr.ready

	tr.say(th, "merge")
	prompt := tr.waitFor(t, th, "echo:")
	if !strings.Contains(prompt.Text, "gh pr merge 51 --squash --delete-branch") {
		t.Errorf("merge prompt does not name the bare number:\n%s", prompt.Text)
	}
	if strings.Contains(prompt.Text, "merge #51 ") {
		t.Errorf("merge prompt hands the shell a comment:\n%s", prompt.Text)
	}
	// And the merge is read back: gh qualifies its confirmation
	// ("cleanunicorn/dispatch#51") on a thread that never named a
	// repository, and a thread that cannot recognise its own merge stays
	// open on a pull request that is merged.
	tr.waitFor(t, th, "thread closed")
	closed, err := st.ClosedThreads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0] != th {
		t.Fatalf("closed threads = %v, want [%s]", closed, th)
	}
}

// TestFailedBareMergeAsksTheThreadNothing: a bare word is one message, and
// its own line is the whole of what a failure says. The retry/skip/stop
// question a named run puts to the thread would be one nothing here can
// clear — a quiet run holds no slot in c.workflows, so `cancel` cannot find
// it, it has no timeout, and c.askText would swallow the thread's next
// message instead of letting it reach the agent.
func TestFailedBareMergeAsksTheThreadNothing(t *testing.T) {
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	c, _, th, cancel := finishFixture(t, tr, fakeAgent{merge: "refused"})
	defer cancel()
	<-tr.ready

	tr.say(th, "merge")
	tr.waitFor(t, th, "the log does not show")

	// The run is over by the time it has said that, but give the ask it
	// used to post the time it would have taken to arrive.
	time.Sleep(200 * time.Millisecond)
	c.mu.Lock()
	base, asking := c.askText[th]
	c.mu.Unlock()
	if asking {
		t.Errorf("a bare `merge` left a question open on the thread: %s", base)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, o := range tr.out {
		if o.Thread == th && o.Prompt != nil {
			t.Errorf("a bare `merge` prompted the thread: %q", o.Text)
		}
	}
}
