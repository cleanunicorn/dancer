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

// TestCloseThread: `close` stops the task, marks the thread closed, drops
// it from the transport and puts a reaction on it; a restart leaves the
// thread alone; talking in it again reopens it.
func TestCloseThread(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "c.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	c.DrainTimeout = time.Second
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	<-tr.ready

	th := transport.ThreadID("C-dev/21.0")
	tr.say(th, "run coder do it")
	tr.waitFor(t, th, "wants to run") // live, waiting on a permission

	tr.say(th, "close")
	tr.waitFor(t, th, "thread closed")

	if !tr.forgot(th) {
		t.Fatal("transport was not told to forget the thread")
	}
	waitReactions(t, tr, th, closedReaction)
	closed, err := st.ClosedThreads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0] != th {
		t.Fatalf("closed threads = %v", closed)
	}

	// The task it was running is stopped.
	id := firstTask(t, st)
	deadline := time.Now().Add(3 * time.Second)
	for ex.IsRunning(id) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ex.IsRunning(id) {
		t.Fatal("task still running after close")
	}

	// Closing twice says so rather than closing again. Reaching the bot in
	// the thread at all lifts Slack's tombstone, so a command that does not
	// reopen the thread has to put it back.
	before := tr.forgetCount(th)
	tr.say(th, "close")
	tr.waitFor(t, th, "already closed")
	waitForget(t, tr, th, before+1)
	before = tr.forgetCount(th)
	tr.say(th, "status")
	tr.waitFor(t, th, "task `")
	waitForget(t, tr, th, before+1)

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not stop")
	}
	st.Close()

	// Restart: a closed thread is neither re-seeded nor auto-resumed.
	st2, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	tr2 := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c2 := New(st2, ex, []transport.Transport{tr2}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c2.WorkdirRoot = t.TempDir()
	c2.DefaultDefinition = "coder"
	c2.AutoResume = true
	go c2.Run(ctx2)
	<-tr2.ready

	time.Sleep(200 * time.Millisecond)
	if tr2.wasRemembered(th) {
		t.Fatal("closed thread was re-seeded on the transport")
	}
	tr2.mu.Lock()
	for _, o := range tr2.out {
		if o.Thread == th {
			tr2.mu.Unlock()
			t.Fatalf("closed thread was spoken to after a restart: %+v", o)
		}
	}
	tr2.mu.Unlock()
	if ts, _ := st2.GetTask(ctx2, id); ts.Status != store.StatusCancelled {
		t.Fatalf("task on a closed thread after restart = %+v", ts)
	}

	// Talking in the thread again reopens it and resumes the session.
	tr2.say(th, "actually, one more thing")
	tr2.waitFor(t, th, "thread reopened")
	tr2.waitFor(t, th, "echo:actually, one more thing")
	if closed, err := st2.ClosedThreads(ctx2); err != nil || len(closed) != 0 {
		t.Fatalf("closed threads after reopen = %v (%v)", closed, err)
	}
	// The ✅ the previous process left is taken back and the thread waits
	// on a human again once the turn is over.
	waitReactions(t, tr2, th, answeredReaction)
	if !tr2.unreacted(th, closedReaction) {
		t.Fatal("✅ was not taken off the reopened thread")
	}
}

// TestReopenRightAfterClose: the cancelled task must let go of the thread
// before close returns, or the next message is written into its dying
// process instead of resuming the session.
func TestReopenRightAfterClose(t *testing.T) {
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
	c.DefaultDefinition = "coder"
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/24.0")
	tr.say(th, "run coder do it")
	tr.waitFor(t, th, "wants to run") // live, mid-turn
	tr.say(th, "close")
	tr.waitFor(t, th, "thread closed")

	tr.say(th, "one more thing")
	tr.waitFor(t, th, "thread reopened")
	tr.waitFor(t, th, "resuming session")
	tr.waitFor(t, th, "echo:one more thing")
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, o := range tr.out {
		if strings.Contains(o.Text, "❌ send:") {
			t.Fatalf("follow-up was sent into the cancelled process: %+v", o)
		}
	}
}

// TestInterruptedTaskOnAClosedThreadIsLeftAlone: a restart mid-turn on a
// thread that was closed in the meantime neither resumes the task nor
// announces anything there — the thread stays quiet until someone speaks.
func TestInterruptedTaskOnAClosedThreadIsLeftAlone(t *testing.T) {
	def := agent.Definition{Name: "coder", Kind: "fake", Environment: environment.Spec{Kind: environment.KindLocal}}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := st.PutDefinition(ctx, def); err != nil {
		t.Fatal(err)
	}
	th := transport.ThreadID("C-dev/23.0")
	if err := st.PutTask(ctx, store.TaskState{ID: "t-closed", Transport: "slack", Thread: th, Definition: def,
		Status: store.StatusInterrupted, Session: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetThreadClosed(ctx, th, true); err != nil {
		t.Fatal(err)
	}

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.AutoResume = true
	go c.Run(ctx)
	<-tr.ready

	time.Sleep(200 * time.Millisecond)
	tr.mu.Lock()
	out := append([]transport.Outbound(nil), tr.out...)
	tr.mu.Unlock()
	if len(out) != 0 {
		t.Fatalf("closed thread was spoken to on restart: %+v", out)
	}
	if tr.wasRemembered(th) {
		t.Fatal("closed thread was re-seeded on the transport")
	}
	if ts, _ := st.GetTask(ctx, "t-closed"); ts.Status != store.StatusIdle {
		t.Fatalf("task on a closed thread after restart = %+v", ts)
	}
}

// TestCloseUnknownThread: closing a thread with nothing running still ends
// it, so a channel can be tidied up without starting a task first.
func TestCloseUnknownThread(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/22.0")
	tr.say(th, "close")
	tr.waitFor(t, th, "thread closed")
	if !tr.forgot(th) {
		t.Fatal("transport was not told to forget the thread")
	}
	if closed, _ := st.ClosedThreads(ctx); len(closed) != 1 {
		t.Fatalf("closed threads = %v", closed)
	}
}

// waitForget waits for the coordinator to have told the transport to forget
// th at least want times. The forget lands just after the reply goes out,
// so waitFor alone is not enough to observe it.
func waitForget(t *testing.T, tr *fakeTransport, th transport.ThreadID, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tr.forgetCount(th) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("thread %s forgotten %d times, want >= %d", th, tr.forgetCount(th), want)
}
