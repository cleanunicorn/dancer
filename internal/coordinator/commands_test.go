package coordinator

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	envlocal "github.com/cleanunicorn/dancer/internal/environment/local"
	execlocal "github.com/cleanunicorn/dancer/internal/executor/local"
	"github.com/cleanunicorn/dancer/internal/store/sqlite"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/surface/chat"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// modelAgent stands in for a CLI that reads its own commands out of the
// message: "/model X" makes it report X on a new init, as claude does,
// and the change lasts only as long as this run. It records the model
// every start and resume was asked for.
type modelAgent struct {
	mu     sync.Mutex
	asked  []string // def.Model per Start/Resume
	starts int
}

func (a *modelAgent) Kind() agent.Kind { return "fake" }

func (a *modelAgent) note(model string) {
	a.mu.Lock()
	a.asked = append(a.asked, model)
	a.mu.Unlock()
}

func (a *modelAgent) models() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asked...)
}

func (a *modelAgent) run(def agent.Definition, session, prompt string) agent.Run {
	a.note(def.Model)
	model := def.Model
	if model == "" {
		model = "default-model"
	}
	if session == "" {
		session = "sess-1"
	}
	r := newCmdRun()
	reply := func(text string) {
		if cmd, ok := strings.CutPrefix(text, "/model "); ok {
			// As the driver does it: the switch is the agent's own doing,
			// and the turn's result reports the model it now runs.
			model = cmd
			r.emit(agent.Event{Type: agent.EventText, Text: "Set model to " + model})
			r.emit(agent.Event{Type: agent.EventResult, Text: "Set model to " + model, Session: session, Model: model})
			return
		}
		r.emit(agent.Event{Type: agent.EventText, Text: "echo:" + text})
		r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: session})
	}
	go func() {
		r.emit(agent.Event{Type: agent.EventInit, Session: session, Model: model, Commands: []string{"clear", "model"}})
		reply(prompt)
		for {
			select {
			case text := <-r.sent:
				r.emit(agent.Event{Type: agent.EventInit, Session: session, Model: model})
				reply(text)
			case <-r.done:
				return
			}
		}
	}()
	return r
}

func (a *modelAgent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	a.mu.Lock()
	a.starts++
	a.mu.Unlock()
	return a.run(def, "", prompt), nil
}

func (a *modelAgent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	return a.run(def, session, prompt), nil
}

// setup builds a coordinator over ag with a chat surface on one thread.
func setupCommands(t *testing.T, ag agent.Agent, idle time.Duration) (*fakeTransport, transport.ThreadID) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	def := agent.Definition{Name: "coder", Kind: "fake", Model: "haiku"}
	if err := st.PutDefinition(ctx, def); err != nil {
		t.Fatal(err)
	}
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": ag},
		map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, idle)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	go c.Run(ctx)
	<-tr.ready
	return tr, transport.ThreadID("C-dev/1.0")
}

// A command is not dancer's to interpret: whatever it is, it reaches the
// agent as the human typed it.
func TestSlashCommandReachesAgentVerbatim(t *testing.T) {
	ag := &modelAgent{}
	tr, th := setupCommands(t, ag, time.Minute)
	tr.say(th, "run coder hello")
	tr.waitFor(t, th, "echo:hello")
	tr.say(th, "/whatever-the-agent-defines --flag")
	tr.waitFor(t, th, "echo:/whatever-the-agent-defines --flag")
}

// "/model X" only lives in the agent process, so the resume after an
// idle timeout must ask for X again — not the definition's model, which
// would silently undo the human's choice.
func TestModelCommandSurvivesResume(t *testing.T) {
	ag := &modelAgent{}
	tr, th := setupCommands(t, ag, 50*time.Millisecond)
	tr.say(th, "run coder hello")
	tr.waitFor(t, th, "echo:hello")
	tr.say(th, "/model opus")
	tr.waitFor(t, th, "Set model to opus")

	// Let the idle timeout end the process, then send a follow-up: the
	// coordinator resumes the session and must re-ask for opus.
	time.Sleep(300 * time.Millisecond)
	tr.say(th, "still there?")
	tr.waitFor(t, th, "echo:still there?")

	asked := ag.models()
	if len(asked) < 2 {
		t.Fatalf("agent was started %d time(s), want a start and a resume: %v", len(asked), asked)
	}
	if asked[0] != "haiku" {
		t.Errorf("start asked for %q, want the definition's haiku", asked[0])
	}
	if last := asked[len(asked)-1]; last != "opus" {
		t.Errorf("resume asked for %q, want opus — the /model choice was lost", last)
	}
}

// Without a "/model", nothing is pinned: every resume keeps asking for
// the definition's model, so an alias stays an alias.
func TestNoModelCommandLeavesDefinitionAlone(t *testing.T) {
	ag := &modelAgent{}
	tr, th := setupCommands(t, ag, 50*time.Millisecond)
	tr.say(th, "run coder hello")
	tr.waitFor(t, th, "echo:hello")
	time.Sleep(300 * time.Millisecond)
	tr.say(th, "again")
	tr.waitFor(t, th, "echo:again")
	for _, m := range ag.models() {
		if m != "haiku" {
			t.Fatalf("asked for %q, want the definition's haiku every time: %v", m, ag.models())
		}
	}
}

// "commands" lists what the agent said it accepts, and says so plainly
// when no agent has run on the thread yet.
func TestListCommands(t *testing.T) {
	ag := &modelAgent{}
	tr, th := setupCommands(t, ag, time.Minute)
	tr.say(th, "commands")
	tr.waitFor(t, th, "no agent has started on this thread yet")

	tr.say(th, "run coder hello")
	tr.waitFor(t, th, "echo:hello")
	tr.say(th, "commands")
	out := tr.waitFor(t, th, "commands *coder* answers")
	for _, want := range []string{"`/clear`", "`/model`"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("commands = %q, want it to list %s", out.Text, want)
		}
	}
}

// cmdRun is fakeRun with a mailbox: the agent goroutine reads what the
// human sent, so a command can change the run's state instead of being
// echoed back.
type cmdRun struct {
	events chan agent.Event
	sent   chan string
	done   chan struct{}

	mu     sync.Mutex
	closed bool
}

func newCmdRun() *cmdRun {
	return &cmdRun{events: make(chan agent.Event, 32), sent: make(chan string, 8), done: make(chan struct{})}
}

func (r *cmdRun) emit(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.events <- ev
}

func (r *cmdRun) Events() <-chan agent.Event { return r.events }

func (r *cmdRun) Send(ctx context.Context, text string) error {
	select {
	case r.sent <- text:
	case <-r.done:
	}
	return nil
}

func (r *cmdRun) Decide(ctx context.Context, d agent.PermissionDecision) error { return nil }

func (r *cmdRun) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.done)
		close(r.events)
	}
	return nil
}
