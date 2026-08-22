package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

	mu     sync.Mutex
	closed bool
}

// emit sends unless stopped; serialized with Stop so close never races a send.
func (r *fakeRun) emit(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.events <- ev
	}
}

func (r *fakeRun) script(prompt string) {
	r.emit(agent.Event{Type: agent.EventInit, Session: "s1"})
	r.emit(agent.Event{Type: agent.EventNeedsPermission, Tool: "Bash", ToolID: "t1", ToolInput: map[string]any{"command": "ls"}})
	select {
	case d := <-r.decided:
		r.emit(agent.Event{Type: agent.EventText, Text: "allowed=" + boolStr(d.Allow)})
		r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: "s1"})
	case <-r.done:
	}
}

func (r *fakeRun) Events() <-chan agent.Event { return r.events }
func (r *fakeRun) Send(ctx context.Context, text string) error {
	r.emit(agent.Event{Type: agent.EventResult, Text: "follow:" + text})
	return nil
}
func (r *fakeRun) Decide(ctx context.Context, d agent.PermissionDecision) error {
	r.decided <- d
	return nil
}
func (r *fakeRun) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.done)
		close(r.events)
	}
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

// slowToolAgent emits a tool_use, then the tool_result only after Stop is
// NOT called for drainDelay — simulating an in-flight command.
type slowToolAgent struct{ delay time.Duration }

func (slowToolAgent) Kind() agent.Kind { return "slow" }
func (a slowToolAgent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	r := &fakeRun{events: make(chan agent.Event, 16), decided: make(chan agent.PermissionDecision, 1), done: make(chan struct{})}
	go func() {
		r.emit(agent.Event{Type: agent.EventInit, Session: "s"})
		r.emit(agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "t1"})
		select {
		case <-time.After(a.delay):
			r.emit(agent.Event{Type: agent.EventToolResult, ToolID: "t1", Text: "finished"})
			r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: "s"})
		case <-r.done:
		}
	}()
	return r, nil
}
func (a slowToolAgent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	return a.Start(ctx, env, def, prompt)
}

func TestShutdownDrainsInflightTool(t *testing.T) {
	ex := New(map[agent.Kind]agent.Agent{"slow": slowToolAgent{delay: 300 * time.Millisecond}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	ex.DrainTimeout = 2 * time.Second
	sink := &recSink{}
	task := executor.Task{ID: "t3", Definition: agent.Definition{Kind: "slow", Environment: environment.Spec{Workdir: t.TempDir()}}, Prompt: "go"}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- ex.Run(ctx, task, sink) }()
	time.Sleep(100 * time.Millisecond) // tool_use is in flight
	cancel()                           // shutdown
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("run err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not end")
	}
	sink.mu.Lock()
	finished := false
	for _, e := range sink.events {
		if e.Type == agent.EventToolResult && e.Text == "finished" {
			finished = true
		}
	}
	sink.mu.Unlock()
	if !finished {
		t.Fatal("expected the in-flight tool call to finish before stop")
	}
}

func TestCancelDoesNotDrain(t *testing.T) {
	ex := New(map[agent.Kind]agent.Agent{"slow": slowToolAgent{delay: 2 * time.Second}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	ex.DrainTimeout = 5 * time.Second
	sink := &recSink{}
	task := executor.Task{ID: "t4", Definition: agent.Definition{Kind: "slow", Environment: environment.Spec{Workdir: t.TempDir()}}, Prompt: "go"}
	errCh := make(chan error, 1)
	go func() { errCh <- ex.Run(context.Background(), task, sink) }()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := ex.Cancel(context.Background(), "t4"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not end run")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("cancel took %v; should not drain", time.Since(start))
	}
	sink.mu.Lock()
	for _, e := range sink.events {
		if e.Type == agent.EventToolResult {
			t.Fatalf("tool result should not have arrived after immediate cancel: %+v", e)
		}
	}
	sink.mu.Unlock()
}

// procAgent runs a real shell command through the environment so the test
// covers process lifetime (exec.CommandContext) rather than a fake channel.
type procAgent struct{}

func (procAgent) Kind() agent.Kind { return "proc" }
func (procAgent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	p, err := env.Exec(ctx, "sh", "-c", prompt)
	if err != nil {
		return nil, err
	}
	r := &procRun{p: p, events: make(chan agent.Event, 16)}
	go func() {
		defer close(r.events)
		r.events <- agent.Event{Type: agent.EventInit, Session: "p"}
		r.events <- agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "t1"}
		out, _ := io.ReadAll(p.Stdout())
		code, _ := p.Wait()
		r.events <- agent.Event{Type: agent.EventToolResult, ToolID: "t1", Text: fmt.Sprintf("code=%d out=%s", code, strings.TrimSpace(string(out)))}
		r.events <- agent.Event{Type: agent.EventResult, Text: "ok", Session: "p"}
	}()
	return r, nil
}
func (a procAgent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	return a.Start(ctx, env, def, prompt)
}

type procRun struct {
	p      environment.Process
	events chan agent.Event
}

func (r *procRun) Events() <-chan agent.Event                                   { return r.events }
func (r *procRun) Send(ctx context.Context, text string) error                  { return nil }
func (r *procRun) Decide(ctx context.Context, d agent.PermissionDecision) error { return nil }
func (r *procRun) Stop() error                                                  { r.p.Stdin().Close(); return nil }

func TestShutdownDoesNotKillAgentProcess(t *testing.T) {
	ex := New(map[agent.Kind]agent.Agent{"proc": procAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	ex.DrainTimeout = 5 * time.Second
	sink := &recSink{}
	task := executor.Task{ID: "t5", Definition: agent.Definition{Kind: "proc", Environment: environment.Spec{Workdir: t.TempDir()}}, Prompt: "sleep 0.5; echo finished"}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- ex.Run(ctx, task, sink) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not end")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, e := range sink.events {
		if e.Type == agent.EventToolResult {
			if e.Text != "code=0 out=finished" {
				t.Fatalf("process was killed by cancellation: %s", e.Text)
			}
			return
		}
	}
	t.Fatalf("no tool result; events=%+v", sink.events)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestFilePathRE(t *testing.T) {
	text := "Screenshots: `/tmp/settings-top.png` and ![mid](out/mid.jpeg). Also /etc/passwd, shot.webp, and notafile.png."
	var got []string
	for _, m := range filePathRE.FindAllStringSubmatch(text, -1) {
		got = append(got, m[1])
	}
	want := []string{"/tmp/settings-top.png", "out/mid.jpeg", "shot.webp", "notafile.png"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestAttachFilesFromEnvironment(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "out"), 0o755)
	os.WriteFile(filepath.Join(dir, "out", "shot.png"), []byte("PNGDATA"), 0o644)
	abs := filepath.Join(dir, "report.pdf")
	os.WriteFile(abs, []byte("PDFDATA"), 0o644)

	ex := New(map[agent.Kind]agent.Agent{"proc": procAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	sink := &recSink{}
	// procAgent echoes the shell output as a tool result; its result text is
	// fixed, so attach through a text event via Send is not available — use
	// attachFiles directly against a real local environment.
	env, _ := envlocal.Factory{}.New(environment.Spec{Workdir: dir})
	sent := map[string]bool{}
	files := ex.attachFiles(context.Background(), env, "Done: out/shot.png and "+abs+" plus missing.png", sent)
	if len(files) != 2 || files[0].Name != "shot.png" || string(files[0].Data) != "PNGDATA" || files[1].Name != "report.pdf" {
		t.Fatalf("files = %+v", files)
	}
	// Same paths again are not re-sent.
	if again := ex.attachFiles(context.Background(), env, "see out/shot.png", sent); len(again) != 0 {
		t.Fatalf("re-sent %+v", again)
	}
	_ = sink
}
