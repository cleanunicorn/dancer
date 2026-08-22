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

	mu         sync.Mutex
	out        []transport.Outbound
	remembered []transport.ThreadID
	forgotten  []transport.ThreadID
	reacted    map[transport.ThreadID][]string
}

func (f *fakeTransport) Name() string { return f.name }
func (f *fakeTransport) Remember(th transport.ThreadID) {
	f.mu.Lock()
	f.remembered = append(f.remembered, th)
	f.mu.Unlock()
}

// Forget implements transport.ThreadCloser.
func (f *fakeTransport) Forget(th transport.ThreadID) {
	f.mu.Lock()
	f.forgotten = append(f.forgotten, th)
	f.mu.Unlock()
}

// React implements transport.Reactor.
func (f *fakeTransport) React(ctx context.Context, th transport.ThreadID, emoji string) error {
	f.mu.Lock()
	if f.reacted == nil {
		f.reacted = map[transport.ThreadID][]string{}
	}
	f.reacted[th] = append(f.reacted[th], emoji)
	f.mu.Unlock()
	return nil
}

// Unreact implements transport.Reactor.
func (f *fakeTransport) Unreact(ctx context.Context, th transport.ThreadID, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.reacted[th]
	for i, e := range cur {
		if e == emoji {
			f.reacted[th] = append(cur[:i:i], cur[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeTransport) forgot(th transport.ThreadID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.forgotten {
		if t == th {
			return true
		}
	}
	return false
}

// forgetCount is how many times the coordinator told the transport to
// forget th; a mention in a closed thread must re-tombstone it.
func (f *fakeTransport) forgetCount(th transport.ThreadID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.forgotten {
		if t == th {
			n++
		}
	}
	return n
}

func (f *fakeTransport) wasRemembered(th transport.ThreadID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.remembered {
		if t == th {
			return true
		}
	}
	return false
}

// reactions is what is on the thread's root message right now.
func (f *fakeTransport) reactions(th transport.ThreadID) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reacted[th]...)
}

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
func (f *fakeTransport) say(th transport.ThreadID, text string) { f.sayAs(th, "u1", text) }
func (f *fakeTransport) sayAs(th transport.ThreadID, user, text string) {
	f.inbox <- transport.Inbound{Transport: f.name, Thread: th, UserID: user, Text: text}
}
func (f *fakeTransport) decide(th transport.ThreadID, id, choice string) {
	f.decideAs(th, "u1", id, choice)
}
func (f *fakeTransport) decideAs(th transport.ThreadID, user, id, choice string) {
	f.inbox <- transport.Inbound{Transport: f.name, Thread: th, UserID: user, Decision: &transport.Decision{PromptID: id, Choice: choice}}
}
func (f *fakeTransport) waitFor(t *testing.T, th transport.ThreadID, sub string) transport.Outbound {
	t.Helper()
	return f.waitForN(t, th, sub, 1)
}

// waitForN returns the n-th message on th containing sub.
func (f *fakeTransport) waitForN(t *testing.T, th transport.ThreadID, sub string, n int) transport.Outbound {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		seen := 0
		for _, o := range f.out {
			if o.Thread == th && strings.Contains(o.Text, sub) {
				if seen++; seen == n {
					f.mu.Unlock()
					return o
				}
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
	r := newFakeRun()
	if strings.HasPrefix(prompt, "ask") {
		go func() {
			r.emit(agent.Event{Type: agent.EventInit, Session: "sess-q"})
			r.emit(agent.Event{Type: agent.EventQuestion, Tool: "AskUserQuestion", ToolID: "q-1", Questions: []agent.Question{
				{Header: "Fruit", Text: "Apple or Banana?", Options: []agent.Option{{Label: "Apple"}, {Label: "Banana", Description: "long"}}},
				{Header: "Size", Text: "Big or small?", Options: []agent.Option{{Label: "Big"}, {Label: "Small"}}},
			}})
			select {
			case d := <-r.decided:
				r.emit(agent.Event{Type: agent.EventText, Text: fmt.Sprintf("answers=%s|%s", d.Answers["Apple or Banana?"], d.Answers["Big or small?"])})
				r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: "sess-q"})
			case <-r.done:
			}
		}()
		return r, nil
	}
	go func() {
		r.emit(agent.Event{Type: agent.EventInit, Session: "sess-1"})
		r.emit(agent.Event{Type: agent.EventNeedsPermission, Tool: "Bash", ToolID: "tool-1", ToolInput: map[string]any{"command": "ls"}})
		select {
		case d := <-r.decided:
			r.emit(agent.Event{Type: agent.EventText, Text: fmt.Sprintf("allowed=%v", d.Allow)})
			r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: "sess-1", Cost: 0.01})
		case <-r.done:
		}
	}()
	return r, nil
}
func (f fakeAgent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	r := newFakeRun()
	go func() {
		r.emit(agent.Event{Type: agent.EventInit, Session: session})
		r.emit(agent.Event{Type: agent.EventText, Text: "echo:" + prompt})
		r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: session})
	}()
	return r, nil
}

type fakeRun struct {
	events  chan agent.Event
	decided chan agent.PermissionDecision
	done    chan struct{}

	mu     sync.Mutex
	closed bool
}

func newFakeRun() *fakeRun {
	return &fakeRun{events: make(chan agent.Event, 16), decided: make(chan agent.PermissionDecision, 1), done: make(chan struct{})}
}

// emit sends unless the run was stopped; sends and Stop are serialized by mu.
func (r *fakeRun) emit(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.events <- ev
}

func (r *fakeRun) Events() <-chan agent.Event { return r.events }
func (r *fakeRun) Send(ctx context.Context, text string) error {
	r.emit(agent.Event{Type: agent.EventText, Text: "echo:" + text})
	r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: "sess-1"})
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

	// Plain text on a fresh thread starts a task with the default agent.
	th2 := transport.ThreadID("C-dev/2.0")
	tr.say(th2, "just do the thing")
	tr.waitFor(t, th2, "started with agent *coder*")

	// Questions: first answered with a button, second with a typed reply.
	th3 := transport.ThreadID("C-dev/3.0")
	tr.say(th3, "ask me things")
	q1 := tr.waitFor(t, th3, "Apple or Banana?")
	if q1.Prompt == nil || len(q1.Prompt.Options) != 2 || !q1.Prompt.FreeText {
		t.Fatalf("question prompt = %+v", q1.Prompt)
	}
	tr.decide(th3, q1.Prompt.ID, "Banana")
	tr.waitFor(t, th3, "Big or small?")
	tr.say(th3, "medium, actually")
	tr.waitFor(t, th3, "answers=Banana|medium, actually")
}

// TestMentionStaysWithRequester: the lines that need a human address the
// one who started the task, even when someone else answers its prompt or
// follows it up after idle.
func TestMentionStaysWithRequester(t *testing.T) {
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
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/1.0")
	tr.sayAs(th, "u1", "run coder do the thing")
	p := tr.waitFor(t, th, "wants to run")
	if p.Mention != "u1" {
		t.Errorf("prompt addressed to %q, want u1", p.Mention)
	}
	tr.decideAs(th, "u2", p.Prompt.ID, "allow")
	if o := tr.waitFor(t, th, "✅ done"); o.Mention != "u1" {
		t.Errorf("done line addressed to %q after u2 allowed, want u1", o.Mention)
	}

	// u2 follows up after the idle timeout: the resumed turn still reports to u1.
	id := firstTask(t, st)
	deadline := time.Now().Add(3 * time.Second)
	for ex.IsRunning(id) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	tr.sayAs(th, "u2", "again")
	tr.waitFor(t, th, "echo:again")
	if o := tr.waitForN(t, th, "✅ done", 2); o.Mention != "u1" {
		t.Errorf("resumed done line addressed to %q after u2 followed up, want u1", o.Mention)
	}
	if ts, err := st.GetTask(ctx, id); err != nil || ts.Requester != "u1" {
		t.Errorf("requester = %q err=%v, want u1", ts.Requester, err)
	}
}

func TestGracefulRestart(t *testing.T) {
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

	th := transport.ThreadID("C-dev/9.0")
	tr.say(th, "run coder do it")
	tr.waitFor(t, th, "wants to run") // task is live, waiting on a permission

	cancel() // SIGTERM
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not stop")
	}
	tr.waitFor(t, th, "dancer is restarting")
	id := firstTask(t, st)
	ts, _ := st.GetTask(context.Background(), id)
	if ts.Status != store.StatusInterrupted || ts.Session != "sess-1" || ts.Transport != "slack" {
		t.Fatalf("task after shutdown = %+v", ts)
	}
	st.Close()

	// Restart: recovered task gets a "back" notice and the next reply resumes it.
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
	go c2.Run(ctx2)
	<-tr2.ready
	tr2.waitFor(t, th, "dancer is back")
	tr2.mu.Lock()
	seeded := len(tr2.remembered) == 1 && tr2.remembered[0] == th
	tr2.mu.Unlock()
	if !seeded {
		t.Fatalf("transport not re-seeded with task thread: %v", tr2.remembered)
	}
	tr2.say(th, "carry on")
	tr2.waitFor(t, th, "resuming session")
	tr2.waitFor(t, th, "echo:carry on")
}

func firstTask(t *testing.T, st store.Store) executor.TaskID {
	tasks, err := st.ListTasks(context.Background(), "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("no tasks: %v", err)
	}
	return tasks[0].ID
}

func TestChannelDefaultsAndRunPicker(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, name := range []string{"coder", "reviewer"} {
		if err := st.PutDefinition(ctx, agent.Definition{Name: name, Kind: "fake"}); err != nil {
			t.Fatal(err)
		}
	}

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	c.ChannelAgents = map[string]string{"slack/C-review": "reviewer"}
	var saved []string
	var savedMu sync.Mutex
	c.SaveChannelAgent = func(_ context.Context, tr, ch, a string) error {
		savedMu.Lock()
		saved = append(saved, tr+"/"+ch+"="+a)
		savedMu.Unlock()
		return nil
	}
	go c.Run(ctx)
	<-tr.ready

	// Plain text follows the channel default, else the global one.
	tr.say("C-review/1.0", "look at this")
	tr.waitFor(t, "C-review/1.0", "started with agent *reviewer*")
	// ...and records who asked, like `run` does.
	if o := tr.waitFor(t, "C-review/1.0", "wants to run"); o.Mention != "u1" {
		t.Errorf("default-agent prompt addressed to %q, want u1", o.Mention)
	}
	if ts, err := st.LatestTaskForThread(ctx, "C-review/1.0"); err != nil || ts.Requester != "u1" {
		t.Errorf("default-agent requester = %q err=%v", ts.Requester, err)
	}
	tr.say("C-dev/1.0", "build this")
	tr.waitFor(t, "C-dev/1.0", "started with agent *coder*")

	// An unknown agent name falls back to the channel default too.
	tr.say("C-review/2.0", "run nosuch thing")
	tr.waitFor(t, "C-review/2.0", "started with agent *reviewer*")

	// `default` shows, `default <agent>` sets and persists.
	tr.say("C-dev/2.0", "default")
	tr.waitFor(t, "C-dev/2.0", "global default *coder*")
	tr.say("C-dev/2.0", "default nosuch")
	tr.waitFor(t, "C-dev/2.0", "unknown agent")
	tr.say("C-dev/2.0", "default reviewer")
	tr.waitFor(t, "C-dev/2.0", "now *reviewer*")
	savedMu.Lock()
	if len(saved) != 1 || saved[0] != "slack/C-dev=reviewer" {
		t.Fatalf("saved = %v", saved)
	}
	savedMu.Unlock()
	tr.say("C-dev/3.0", "next thing")
	tr.waitFor(t, "C-dev/3.0", "started with agent *reviewer*")
	tr.say("C-dev/4.0", "agents")
	if o := tr.waitFor(t, "C-dev/4.0", "*reviewer*"); !strings.Contains(o.Text, "*reviewer* — fake/, env , mode  · _default here_") || strings.Count(o.Text, "default here") != 1 {
		t.Fatalf("agents = %q", o.Text)
	}

	// Bare `run`: pick the agent from a list, then type the prompt.
	th := transport.ThreadID("C-dev/5.0")
	tr.say(th, "run")
	q := tr.waitFor(t, th, "Which agent?")
	if q.Prompt == nil || len(q.Prompt.Options) != 2 || !q.Prompt.FreeText || !strings.Contains(q.Prompt.Options[1].Description, "default here") {
		t.Fatalf("picker prompt = %+v", q.Prompt)
	}
	tr.decide(th, q.Prompt.ID, "coder")
	p := tr.waitFor(t, th, "What should *coder* do?")
	if p.Prompt == nil || len(p.Prompt.Options) != 0 {
		t.Fatalf("prompt question = %+v", p.Prompt)
	}
	tr.say(th, "do the thing")
	tr.waitFor(t, th, "started with agent *coder*")
	if o := tr.waitFor(t, th, "wants to run"); o.Mention != "u1" {
		t.Errorf("permission prompt addressed to %q, want the one who typed `run`", o.Mention)
	}
	if st, err := st.LatestTaskForThread(ctx, th); err != nil || st.Requester != "u1" {
		t.Errorf("requester = %q err=%v", st.Requester, err)
	}

	// `run <agent>` without a prompt asks for the prompt; a typed agent
	// name works for the picker too; `cancel` abandons it.
	th2 := transport.ThreadID("C-dev/6.0")
	tr.say(th2, "run reviewer")
	tr.waitFor(t, th2, "What should *reviewer* do?")
	tr.say(th2, "cancel")
	tr.waitFor(t, th2, "run cancelled")
	th3 := transport.ThreadID("C-dev/7.0")
	tr.say(th3, "run")
	tr.waitFor(t, th3, "Which agent?")
	tr.say(th3, "nosuch")
	tr.waitFor(t, th3, "no agent named")
	tr.say(th3, "reviewer")
	tr.waitFor(t, th3, "What should *reviewer* do?")
	tr.say(th3, "review it")
	tr.waitFor(t, th3, "started with agent *reviewer*")
}
