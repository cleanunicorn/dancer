package local

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	envlocal "github.com/cleanunicorn/dancer/internal/environment/local"
	"github.com/cleanunicorn/dancer/internal/executor"
)

// fakeAgent emits a scripted sequence: init, needs_permission, then echoes
// the decision as text and a result. Follow-ups produce another result.
type fakeAgent struct{}

func (fakeAgent) Kind() agent.Kind { return "fake" }
func (fakeAgent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	r := &fakeRun{events: make(chan agent.Event, 16), decided: make(chan agent.PermissionDecision, 1), done: make(chan struct{})}
	go r.script(prompt)
	return r, nil
}
func (f fakeAgent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	return f.Start(ctx, env, def, prompt)
}

type fakeRun struct {
	events  chan agent.Event
	decided chan agent.PermissionDecision
	done    chan struct{}
	once    sync.Once
}

func (r *fakeRun) script(prompt string) {
	r.events <- agent.Event{Type: agent.EventInit, Session: "s1"}
	r.events <- agent.Event{Type: agent.EventNeedsPermission, Tool: "Bash", ToolID: "t1", ToolInput: map[string]any{"command": "ls"}}
	d := <-r.decided
	r.events <- agent.Event{Type: agent.EventText, Text: "allowed=" + boolStr(d.Allow)}
	r.events <- agent.Event{Type: agent.EventResult, Text: "ok", Session: "s1"}
	<-r.done
}

func (r *fakeRun) Events() <-chan agent.Event { return r.events }
func (r *fakeRun) Send(ctx context.Context, text string) error {
	r.events <- agent.Event{Type: agent.EventResult, Text: "follow:" + text}
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

type recSink struct {
	mu     sync.Mutex
	events []agent.Event
	allow  bool
}

func (s *recSink) OnEvent(ctx context.Context, id executor.TaskID, ev agent.Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}
func (s *recSink) AwaitDecision(ctx context.Context, id executor.TaskID, ev agent.Event) (agent.PermissionDecision, error) {
	return agent.PermissionDecision{ToolID: ev.ToolID, Allow: s.allow}, nil
}
func (s *recSink) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		if e.Type == agent.EventText || e.Type == agent.EventResult {
			out = append(out, e.Text)
		}
	}
	return out
}

func TestRunPermissionFollowUpIdle(t *testing.T) {
	ex := New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 300*time.Millisecond)
	sink := &recSink{allow: true}
	task := executor.Task{ID: "task1", Definition: agent.Definition{Kind: "fake", Environment: environment.Spec{Workdir: t.TempDir()}}, Prompt: "go"}

	errCh := make(chan error, 1)
	go func() { errCh <- ex.Run(context.Background(), task, sink) }()

	// Wait for the first result, then send a follow-up within the idle window.
	deadline := time.Now().Add(3 * time.Second)
	for len(sink.texts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ex.IsRunning("task1") {
		t.Fatal("task should be running during idle window")
	}
	if err := ex.Send(context.Background(), "task1", "more"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not end after idle timeout")
	}
	got := sink.texts()
	want := []string{"allowed=true", "ok", "follow:more"}
	if len(got) != len(want) {
		t.Fatalf("texts = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("texts = %v, want %v", got, want)
		}
	}
	if ex.IsRunning("task1") {
		t.Fatal("task still registered after exit")
	}
	if err := ex.Send(context.Background(), "task1", "x"); err != ErrNotRunning {
		t.Fatalf("send after exit = %v", err)
	}
}

func TestCancel(t *testing.T) {
	ex := New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	sink := &recSink{allow: false}
	task := executor.Task{ID: "task2", Definition: agent.Definition{Kind: "fake", Environment: environment.Spec{Workdir: t.TempDir()}}, Prompt: "go"}
	errCh := make(chan error, 1)
	go func() { errCh <- ex.Run(context.Background(), task, sink) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(sink.texts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := ex.Cancel(context.Background(), "task2"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("run err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not end run")
	}
	if got := sink.texts(); len(got) < 1 || got[0] != "allowed=false" {
		t.Fatalf("texts = %v", got)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
