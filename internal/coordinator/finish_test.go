package coordinator

import (
	"context"
	"os"
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

// finishFixture is a thread that has done a piece of work: it opened
// pull request #51 on a branch, and its task is over — which is the state
// a human types `review` or `ship` in.
func finishFixture(t *testing.T, tr transport.Transport) (*Coordinator, store.Store, transport.ThreadID, context.CancelFunc) {
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
		Definition: agent.Definition{Name: "coder", Kind: "fake"}, Session: "s-1", Status: store.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	appendInbound(t, st, th, "run coder please fix #47")
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u1",
		ToolInput: map[string]any{"command": "git push -u origin fix-47"}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u2",
		ToolInput: map[string]any{"command": `gh pr create --body "Closes #47"`}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolResult, ToolID: "u2",
		Text: "https://github.com/cleanunicorn/dispatch/pull/51"})

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
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
	_, st, th, cancel := finishFixture(t, tr)
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

// TestShipMergesThenCloses: the thread is closed on gh's exit code and
// nothing else, and gh's own words are posted with it.
func TestShipMergesThenCloses(t *testing.T) {
	fakeGH(t, 0, "Merged pull request cleanunicorn/dispatch#51")
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	_, st, th, cancel := finishFixture(t, tr)
	defer cancel()
	<-tr.ready

	tr.say(th, "ship")
	tr.waitFor(t, th, "🚢 merging")
	tr.waitFor(t, th, "✅ merged")
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

// TestShipLeavesTheThreadOpenWhenTheMergeFails is the reason dispatch runs
// the merge itself: an agent asked to merge would report the refusal in
// prose, and a thread closed on prose is a thread nobody comes back to.
func TestShipLeavesTheThreadOpenWhenTheMergeFails(t *testing.T) {
	fakeGH(t, 1, "Pull request #51 is not mergeable: 1 required check is still pending")
	tr := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	_, st, th, cancel := finishFixture(t, tr)
	defer cancel()
	<-tr.ready

	tr.say(th, "ship")
	reply := tr.waitFor(t, th, "merge failed")
	if !strings.Contains(reply.Text, "still pending") {
		t.Errorf("reply does not carry gh's reason:\n%s", reply.Text)
	}
	closed, err := st.ClosedThreads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 0 {
		t.Fatalf("thread was closed on a failed merge: %v", closed)
	}
}

func TestShipWithoutAPullRequest(t *testing.T) {
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

	tr.say("C-dev/3.0", "ship")
	tr.waitFor(t, "C-dev/3.0", "nothing to ship")
}

// fakeGH puts a `gh` on PATH that prints say and exits with code, so a
// coordinator test can exercise the whole `ship` path without merging
// anything on GitHub.
func fakeGH(t *testing.T, code int, say string) {
	t.Helper()
	dir := t.TempDir()
	// echo, not cat: PATH is this directory alone, so the script may
	// only use the shell's own builtins.
	script := "#!/bin/sh\necho '" + say + "'\nexit " + map[int]string{0: "0", 1: "1"}[code] + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}
