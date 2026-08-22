package coordinator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	envlocal "github.com/cleanunicorn/dancer/internal/environment/local"
	execlocal "github.com/cleanunicorn/dancer/internal/executor/local"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/store/sqlite"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/surface/chat"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// TestAutoResumeAfterRestart: a task cut short by a restart continues on
// its own once dancer is back, with nobody typing in the thread.
func TestAutoResumeAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "c.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
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

	th := transport.ThreadID("C-dev/11.0")
	tr.say(th, "run coder do it")
	tr.waitFor(t, th, "wants to run") // live, waiting on a permission

	cancel() // SIGTERM
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not stop")
	}
	id := firstTask(t, st)
	ts, _ := st.GetTask(context.Background(), id)
	if ts.Status != store.StatusInterrupted || ts.Session == "" {
		t.Fatalf("task after shutdown = %+v", ts)
	}
	st.Close()

	// Restart with auto-resume: the session continues without any message.
	st2, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	tr2 := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c2 := New(st2, ex, []transport.Transport{tr2}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c2.DefaultDefinition = "coder"
	c2.AutoResume = true
	go c2.Run(ctx2)
	<-tr2.ready
	tr2.waitFor(t, th, "picking up this task")
	tr2.waitFor(t, th, "echo:"+defaultResumePrompt)

	// The turn ended by itself, so the restart counter is cleared again.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ts, _ = st2.GetTask(ctx2, id)
		if ts.Status == store.StatusIdle && ts.Resumes == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task after auto-resume = %+v", ts)
}

// TestAutoResumeGuards: a task that never reached a session runs again from
// its prompt; stale tasks and restart-loopers fall back to waiting.
func TestAutoResumeGuards(t *testing.T) {
	def := agent.Definition{Name: "coder", Kind: "fake", Environment: environment.Spec{Kind: environment.KindLocal}}
	seed := func(t *testing.T, task store.TaskState) store.Store {
		t.Helper()
		st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if err := st.PutDefinition(context.Background(), def); err != nil {
			t.Fatal(err)
		}
		if err := st.PutTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		return st
	}
	start := func(t *testing.T, st store.Store, tune func(*Coordinator)) *fakeTransport {
		t.Helper()
		ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
		tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
		c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
		c.WorkdirRoot = t.TempDir()
		c.AutoResume = true
		tune(c)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go c.Run(ctx)
		<-tr.ready
		return tr
	}

	t.Run("queued task runs again from its prompt", func(t *testing.T) {
		th := transport.ThreadID("C-dev/20.0")
		st := seed(t, store.TaskState{ID: "t-queued", Transport: "slack", Thread: th, Definition: def,
			Status: store.StatusQueued, Prompt: "do it"})
		tr := start(t, st, func(*Coordinator) {})
		tr.waitFor(t, th, "never started, running it again")
		tr.waitFor(t, th, "wants to run")
	})

	t.Run("task that keeps restarting waits for a reply", func(t *testing.T) {
		th := transport.ThreadID("C-dev/21.0")
		st := seed(t, store.TaskState{ID: "t-loop", Transport: "slack", Thread: th, Definition: def,
			Status: store.StatusInterrupted, Session: "sess-1", Resumes: 3})
		tr := start(t, st, func(*Coordinator) {})
		tr.waitFor(t, th, "reply in this thread to continue")
	})

	t.Run("stale task waits for a reply", func(t *testing.T) {
		th := transport.ThreadID("C-dev/22.0")
		st := seed(t, store.TaskState{ID: "t-stale", Transport: "slack", Thread: th, Definition: def,
			Status: store.StatusInterrupted, Session: "sess-1"})
		tr := start(t, st, func(c *Coordinator) { c.AutoResumeWithin = time.Nanosecond })
		tr.waitFor(t, th, "reply in this thread to continue")
	})

	t.Run("auto-resume off waits for a reply", func(t *testing.T) {
		th := transport.ThreadID("C-dev/23.0")
		st := seed(t, store.TaskState{ID: "t-off", Transport: "slack", Thread: th, Definition: def,
			Status: store.StatusInterrupted, Session: "sess-1"})
		tr := start(t, st, func(c *Coordinator) { c.AutoResume = false })
		tr.waitFor(t, th, "reply in this thread to continue")
	})
}
