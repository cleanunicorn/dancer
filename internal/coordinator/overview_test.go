package coordinator

import (
	"context"
	"encoding/json"
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

// TestStatusCarriesTheWork: `status` answers with the pull request the
// thread opened and the issue it is for, read back out of the log — the
// task itself is long over and its container gone.
func TestStatusCarriesTheWork(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	th := transport.ThreadID("C-dev/1.0")
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTask(ctx, store.TaskState{ID: "t-1", Transport: "slack", Thread: th,
		Definition: agent.Definition{Name: "coder"}, Session: "s-1", Status: store.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	appendInbound(t, st, th, "run coder please fix #47")
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u1",
		ToolInput: map[string]any{"command": "git switch -c fix-47"}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u2",
		ToolInput: map[string]any{"command": `gh pr create --body "Closes #47"`}})
	logAgent(t, st, th, agent.Event{Type: agent.EventToolResult, ToolID: "u2",
		Text: "https://github.com/cleanunicorn/dispatch/pull/51"})

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	go c.Run(ctx)
	<-tr.ready

	tr.say(th, "status")
	reply := tr.waitFor(t, th, "task `t-1`")
	for _, want := range []string{
		"status *idle*",
		"🔀 #51 https://github.com/cleanunicorn/dispatch/pull/51 · for #47",
		"🌿 `fix-47`",
	} {
		if !strings.Contains(reply.Text, want) {
			t.Errorf("status reply is missing %q:\n%s", want, reply.Text)
		}
	}
}

// TestStatusOfAThreadWithoutCode: nothing is added when the thread never
// went near a repository.
func TestStatusOfAThreadWithoutCode(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	th := transport.ThreadID("C-dev/2.0")
	if err := st.PutTask(ctx, store.TaskState{ID: "t-2", Transport: "slack", Thread: th,
		Definition: agent.Definition{Name: "coder"}, Status: store.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	appendInbound(t, st, th, "run coder summarise the meeting notes")

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	go c.Run(ctx)
	<-tr.ready

	tr.say(th, "status")
	reply := tr.waitFor(t, th, "task `t-2`")
	// Asserted on the overview's own marks, not on the answer having one
	// line: `status` grows a line of its own whenever a decider has ruled
	// on the thread, and that line is not an overview.
	for _, mark := range []string{"🔀", "🎯", "🌿", "💬"} {
		if strings.Contains(reply.Text, mark) {
			t.Errorf("an overview (%s) was added to a thread that has no work to show:\n%s", mark, reply.Text)
		}
	}
}

// TestClosingLineCarriesTheWork: the closing line of a turn — the line a
// human reads to decide whether to go and look — carries the pull request
// the turn opened. This is the path most humans meet; `status` is the one
// they ask for.
func TestClosingLineCarriesTheWork(t *testing.T) {
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
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/3.0")
	tr.say(th, "run coder ship the fix")
	done := tr.waitFor(t, th, "✅ done")
	for _, want := range []string{
		"🔀 #51 https://github.com/cleanunicorn/dispatch/pull/51 · for #47",
		"🌿 `fix-47`",
	} {
		if !strings.Contains(done.Text, want) {
			t.Errorf("closing line is missing %q:\n%s", want, done.Text)
		}
	}
}

// TestFailedTurnCarriesTheWork: a turn that opened a pull request and then
// failed still says where the work is. That is the case where someone most
// has to go and look: there is half-finished work out there with their
// name on it, and the error alone does not say where.
func TestFailedTurnCarriesTheWork(t *testing.T) {
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
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/4.0")
	tr.say(th, "run coder botch it")
	failed := tr.waitFor(t, th, "❌")
	for _, want := range []string{
		"the build fell over",
		"🔀 #60 https://github.com/cleanunicorn/dispatch/pull/60",
		"🌿 `half-done`",
	} {
		if !strings.Contains(failed.Text, want) {
			t.Errorf("the failure line is missing %q:\n%s", want, failed.Text)
		}
	}
}

// logAgent records an agent event on a thread the way taskSink does.
func logAgent(t *testing.T, st store.Store, th transport.ThreadID, ev agent.Event) {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(context.Background(), store.Record{At: time.Now(), Task: "t-1", Thread: th, Kind: "agent", Payload: b}); err != nil {
		t.Fatal(err)
	}
}
