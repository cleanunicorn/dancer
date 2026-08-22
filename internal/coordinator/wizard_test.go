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

// waitForNth waits until sub has appeared n times on th.
func waitForNth(t *testing.T, tr *fakeTransport, th transport.ThreadID, sub string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		seen := 0
		tr.mu.Lock()
		for _, o := range tr.out {
			if o.Thread == th && strings.Contains(o.Text, sub) {
				seen++
			}
		}
		tr.mu.Unlock()
		if seen >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%q did not appear %d times on %s", sub, n, th)
}

func TestAddAgentFlow(t *testing.T) {
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
	c.DefaultDefinition = "coder"
	var saved []agent.Definition
	var savedMu sync.Mutex
	c.SaveDefinition = func(_ context.Context, d agent.Definition) error {
		savedMu.Lock()
		defer savedMu.Unlock()
		saved = append(saved, d)
		return nil
	}
	go c.Run(ctx)
	<-tr.ready

	// Cancel half-way: the thread is free again afterwards.
	th := transport.ThreadID("C-dev/9.0")
	tr.say(th, "add agent")
	tr.waitFor(t, th, "Name for the new agent")
	tr.say(th, "cancel")
	tr.waitFor(t, th, "add agent cancelled")

	workdir := t.TempDir()
	tr.say(th, "add agent")
	waitForNth(t, tr, th, "Name for the new agent", 2)
	// Name: invalid, taken, then fine. Typed replies answer the question.
	tr.say(th, "bad name")
	tr.waitFor(t, th, "is not a valid name")
	tr.say(th, "coder")
	tr.waitFor(t, th, "already exists")
	tr.say(th, "reviewer")
	model := tr.waitFor(t, th, "Which model")
	if model.Prompt == nil || !strings.HasPrefix(model.Prompt.ID, "chat:") || len(model.Prompt.Options) != 3 {
		t.Fatalf("model prompt = %+v", model.Prompt)
	}
	// A button click answers too.
	tr.decide(th, model.Prompt.ID, "opus")
	tr.waitFor(t, th, "Where does it run")
	// Starting a task on the thread is refused while the flow is open.
	tr.say(th, "run coder something")
	tr.waitFor(t, th, "finish or `cancel`")
	tr.say(th, "local")
	tr.waitFor(t, th, "Absolute path")
	tr.say(th, "relative/dir")
	tr.waitFor(t, th, "not an absolute path")
	tr.say(th, "`"+workdir+"`") // as Slack users must send paths
	tr.waitFor(t, th, "Permission mode?")
	tr.say(th, "acceptEdits")
	tr.waitFor(t, th, "Pre-approved tools")
	tr.say(th, "Edit + git")
	tr.waitFor(t, th, "Extra instructions")
	tr.say(th, "Review carefully.")
	summary := tr.waitFor(t, th, "Save this agent?")
	for _, want := range []string{"*reviewer*", "opus", "local", workdir, "acceptEdits", "Bash(git:*)", "Review carefully."} {
		if !strings.Contains(summary.Text, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary.Text)
		}
	}
	tr.say(th, "save")
	tr.waitFor(t, th, "agent *reviewer* saved")

	def, err := st.GetDefinition(ctx, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if def.Kind != agent.KindClaude || def.Model != "opus" || def.Environment.Kind != environment.KindLocal ||
		def.Environment.Workdir != workdir || def.PermissionMode != agent.PermissionAcceptEdits ||
		strings.Join(def.AllowedTools, ",") != "Read,Glob,Grep,Edit,Write,Bash(git:*)" || def.SystemPrompt != "Review carefully." {
		t.Fatalf("stored definition = %+v", def)
	}
	savedMu.Lock()
	if len(saved) != 1 || saved[0].Name != "reviewer" {
		t.Fatalf("SaveDefinition calls = %+v", saved)
	}
	savedMu.Unlock()

	// The new agent is listed right away.
	th2 := transport.ThreadID("C-dev/10.0")
	tr.say(th2, "agents")
	tr.waitFor(t, th2, "*reviewer*")
}
