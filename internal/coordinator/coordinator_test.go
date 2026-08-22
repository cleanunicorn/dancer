package coordinator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	envlocal "github.com/cleanunicorn/dancer/internal/environment/local"
	"github.com/cleanunicorn/dancer/internal/executor"
	execlocal "github.com/cleanunicorn/dancer/internal/executor/local"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/store/sqlite"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/surface/chat"
	"github.com/cleanunicorn/dancer/internal/surface/feed"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// fakeTransport records outbound messages and lets the test inject inbound ones.
type fakeTransport struct {
	name  string
	inbox chan<- transport.Inbound
	ready chan struct{}

	mu  sync.Mutex
	out []transport.Outbound
}

func (f *fakeTransport) Name() string { return f.name }
func (f *fakeTransport) Run(ctx context.Context, inbox chan<- transport.Inbound) error {
	f.inbox = inbox
	close(f.ready)
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeTransport) Send(ctx context.Context, msg transport.Outbound) error {
	f.mu.Lock()
	f.out = append(f.out, msg)
	f.mu.Unlock()
	return nil
}
func (f *fakeTransport) say(th transport.ThreadID, text string) {
	f.inbox <- transport.Inbound{Transport: f.name, Thread: th, UserID: "u1", Text: text}
}
func (f *fakeTransport) decide(th transport.ThreadID, id, choice string) {
	f.inbox <- transport.Inbound{Transport: f.name, Thread: th, UserID: "u1", Decision: &transport.Decision{PromptID: id, Choice: choice}}
}
func (f *fakeTransport) waitFor(t *testing.T, th transport.ThreadID, sub string) transport.Outbound {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, o := range f.out {
			if o.Thread == th && strings.Contains(o.Text, sub) {
				f.mu.Unlock()
				return o
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("no outbound on %s containing %q; got %+v", th, sub, f.out)
	return transport.Outbound{}
}

// fakeAgent: asks permission for Bash, reports the decision, then a result.
type fakeAgent struct{}

func (fakeAgent) Kind() agent.Kind { return "fake" }
func (fakeAgent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	r := &fakeRun{events: make(chan agent.Event, 16), decided: make(chan agent.PermissionDecision, 1), done: make(chan struct{})}
	go func() {
		r.events <- agent.Event{Type: agent.EventInit, Session: "sess-1"}
		r.events <- agent.Event{Type: agent.EventNeedsPermission, Tool: "Bash", ToolID: "tool-1", ToolInput: map[string]any{"command": "ls"}}
		d := <-r.decided
		r.events <- agent.Event{Type: agent.EventText, Text: fmt.Sprintf("allowed=%v", d.Allow)}
		r.events <- agent.Event{Type: agent.EventResult, Text: "ok", Session: "sess-1", Cost: 0.01}
		<-r.done
	}()
	return r, nil
}
func (f fakeAgent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	r := &fakeRun{events: make(chan agent.Event, 16), decided: make(chan agent.PermissionDecision, 1), done: make(chan struct{})}
	go func() {
		r.events <- agent.Event{Type: agent.EventInit, Session: session}
		r.events <- agent.Event{Type: agent.EventText, Text: "echo:" + prompt}
		r.events <- agent.Event{Type: agent.EventResult, Text: "ok", Session: session}
		<-r.done
	}()
	return r, nil
}

type fakeRun struct {
	events  chan agent.Event
	decided chan agent.PermissionDecision
	once    sync.Once
	done    chan struct{}
}

func (r *fakeRun) Events() <-chan agent.Event { return r.events }
func (r *fakeRun) Send(ctx context.Context, text string) error {
	r.events <- agent.Event{Type: agent.EventText, Text: "echo:" + text}
	r.events <- agent.Event{Type: agent.EventResult, Text: "ok", Session: "sess-1"}
	return nil
}
func (r *fakeRun) Decide(ctx context.Context, d agent.PermissionDecision) error {
	r.decided <- d
	return nil
}
func (r *fakeRun) Stop() error {
	r.once.Do(func() { close(r.done); close(r.events) })
	return nil
}

func TestTwoSurfacesOneTransport(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def := agent.Definition{Name: "coder", Kind: "fake"}
	if err := st.PutDefinition(ctx, def); err != nil {
		t.Fatal(err)
	}

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	feedThread := transport.ThreadID("C-ops/")
	surfaces := []surface.Surface{
		feed.New("ops", "slack", feedThread, true),
		chat.New("chat", "slack", false),
	}
	c := New(st, ex, []transport.Transport{tr}, surfaces, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/1.0")
	tr.say(th, "run coder do the thing")
	tr.waitFor(t, th, "started with agent *coder*")
	tr.waitFor(t, feedThread, "started — agent *coder*")

	// Both surfaces render the permission prompt with their own prefix.
	chatPrompt := tr.waitFor(t, th, "wants to run")
	feedPrompt := tr.waitFor(t, feedThread, "wants to run")
	if chatPrompt.Prompt == nil || !strings.HasPrefix(chatPrompt.Prompt.ID, "chat:") {
		t.Fatalf("chat prompt = %+v", chatPrompt.Prompt)
	}
	if feedPrompt.Prompt == nil || !strings.HasPrefix(feedPrompt.Prompt.ID, "ops:") {
		t.Fatalf("feed prompt = %+v", feedPrompt.Prompt)
	}

	// Decide from the feed surface; the chat thread still gets the result.
	tr.decide(feedThread, feedPrompt.Prompt.ID, "allow")
	tr.waitFor(t, th, "allowed=true")
	tr.waitFor(t, th, "✅ done")
	tr.waitFor(t, feedThread, "done")

	// Follow-up while still live.
	tr.say(th, "more please")
	tr.waitFor(t, th, "echo:more please")

	// Status reply goes only to the chat thread.
	tr.say(th, "status")
	tr.waitFor(t, th, "status *")

	// Plain chatter in the feed thread is swallowed by the feed surface.
	tr.say(feedThread, "hello ops")
	time.Sleep(50 * time.Millisecond)
	tr.mu.Lock()
	for _, o := range tr.out {
		if o.Thread == feedThread && strings.Contains(o.Text, "no task") {
			t.Fatalf("feed chatter leaked to chat surface: %+v", o)
		}
	}
	tr.mu.Unlock()

	// After the idle timeout the process is gone; the task is idle with a
	// session and the next message resumes it.
	id := firstTask(t, st)
	deadline := time.Now().Add(3 * time.Second)
	for ex.IsRunning(id) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ex.IsRunning(id) {
		t.Fatal("task still running after idle timeout")
	}
	ts, _ := st.GetTask(ctx, id)
	if ts.Status != store.StatusIdle || ts.Session != "sess-1" {
		t.Fatalf("task after idle = %+v", ts)
	}
	tr.say(th, "again")
	tr.waitFor(t, th, "resuming session")
	tr.waitFor(t, th, "echo:again")
}

func firstTask(t *testing.T, st store.Store) executor.TaskID {
	tasks, err := st.ListTasks(context.Background(), "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("no tasks: %v", err)
	}
	return tasks[0].ID
}
