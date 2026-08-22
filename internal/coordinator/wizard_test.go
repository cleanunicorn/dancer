package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
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
	tr.say(th, "agent add")
	tr.waitFor(t, th, "Name for the new agent")
	tr.say(th, "cancel")
	tr.waitFor(t, th, "agent add cancelled")

	workdir := t.TempDir()
	tr.say(th, "agent add")
	waitForNth(t, tr, th, "Name for the new agent", 2)
	// Name: invalid, taken, then fine. Typed replies answer the question.
	tr.say(th, "bad name")
	tr.waitFor(t, th, "is not a valid name")
	tr.say(th, "coder")
	tr.waitFor(t, th, "already exists")
	tr.say(th, "reviewer")
	model := tr.waitFor(t, th, "Which model")
	if model.Prompt == nil || !strings.HasPrefix(model.Prompt.ID, "chat:") || len(model.Prompt.Options) != 4 {
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
	if flows, _ := st.ListFlows(ctx); len(flows) != 0 {
		t.Fatalf("flow left behind: %+v", flows)
	}
}

func TestAddAgentFlowSurvivesRestart(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	th := transport.ThreadID("C-dev/11.0")

	// First life: answer the name, then dancer restarts.
	ctx1, cancel1 := context.WithCancel(context.Background())
	tr1 := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c1 := New(st, ex, []transport.Transport{tr1}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	done1 := make(chan struct{})
	go func() { c1.Run(ctx1); close(done1) }()
	<-tr1.ready
	tr1.say(th, "agent add")
	tr1.waitFor(t, th, "Name for the new agent")
	tr1.say(th, "reviewer")
	tr1.waitFor(t, th, "Which model")
	cancel1()
	<-done1
	tr1.waitFor(t, th, "dancer is restarting")
	if flows, _ := st.ListFlows(context.Background()); len(flows) != 1 || len(flows[0].Answers) != 1 || flows[0].Answers[0] != "reviewer" {
		t.Fatalf("flows after shutdown = %+v", flows)
	}

	// Second life: the name is not asked again; the model question comes back.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	tr2 := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c2 := New(st, ex, []transport.Transport{tr2}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	go c2.Run(ctx2)
	<-tr2.ready
	tr2.waitFor(t, th, "dancer is back")
	tr2.waitFor(t, th, "Which model")
	tr2.mu.Lock()
	for _, o := range tr2.out {
		if strings.Contains(o.Text, "Name for the new agent") {
			t.Fatalf("name asked again after restart: %+v", o)
		}
	}
	tr2.mu.Unlock()
	tr2.say(th, "haiku")
	tr2.waitFor(t, th, "Where does it run")
	tr2.say(th, "local")
	tr2.waitFor(t, th, "Absolute path")
	tr2.say(th, "none")
	tr2.waitFor(t, th, "Permission mode?")
	tr2.say(th, "manual")
	tr2.waitFor(t, th, "Pre-approved tools")
	tr2.say(th, "None")
	tr2.waitFor(t, th, "Extra instructions")
	tr2.say(th, "skip")
	tr2.waitFor(t, th, "Save this agent?")
	tr2.say(th, "save")
	tr2.waitFor(t, th, "agent *reviewer* saved")
	def, err := st.GetDefinition(ctx2, "reviewer")
	if err != nil || def.Model != "haiku" || len(def.AllowedTools) != 0 || def.Environment.Workdir != "" {
		t.Fatalf("definition = %+v err=%v", def, err)
	}
	if flows, _ := st.ListFlows(ctx2); len(flows) != 0 {
		t.Fatalf("flow left behind: %+v", flows)
	}
}

func TestEditAndDeleteAgent(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workdir := t.TempDir()
	for _, d := range []agent.Definition{
		{Name: "coder", Kind: "fake", Model: "sonnet", PermissionMode: agent.PermissionManual, Environment: environment.Spec{Kind: environment.KindLocal, Workdir: workdir, Env: map[string]string{"FOO": "1"}}},
		{Name: "reviewer", Kind: "fake", Model: "opus", PermissionMode: agent.PermissionManual, AllowedTools: []string{"Read"}, Environment: environment.Spec{Kind: environment.KindLocal}},
		{Name: "tester", Kind: "fake", Model: "haiku", Environment: environment.Spec{Kind: environment.KindLocal}},
	} {
		if err := st.PutDefinition(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	c.ChannelAgents = map[string]string{"slack/C-review": "reviewer"}
	var mu sync.Mutex
	var updated []agent.Definition
	var deleted []string
	c.UpdateDefinition = func(_ context.Context, d agent.Definition) error {
		mu.Lock()
		defer mu.Unlock()
		updated = append(updated, d)
		return nil
	}
	c.DeleteDefinition = func(_ context.Context, name string) error {
		mu.Lock()
		defer mu.Unlock()
		deleted = append(deleted, name)
		return nil
	}
	go c.Run(ctx)
	<-tr.ready

	// Edit: unknown name is refused; the menu shows current values; Save
	// without a change is a no-op.
	th := transport.ThreadID("C-dev/20.0")
	tr.say(th, "agent edit nosuch")
	tr.waitFor(t, th, "unknown agent")
	tr.say(th, "agent edit reviewer")
	menu := tr.waitFor(t, th, "What do you want to change?")
	if menu.Prompt == nil || len(menu.Prompt.Options) != 7 || menu.Prompt.Options[0].Description != "opus" || menu.Prompt.Options[3].Description != "Read" ||
		!strings.Contains(menu.Text, "• model: opus") {
		t.Fatalf("edit menu = %+v\n%s", menu.Prompt, menu.Text)
	}
	tr.say(th, "save")
	tr.waitFor(t, th, "nothing changed on *reviewer*")

	// Change the model (button), the tools (typed), then save.
	tr.say(th, "agent edit reviewer")
	waitForNth(t, tr, th, "What do you want to change?", 2)
	tr.say(th, "nope")
	tr.waitFor(t, th, "pick one of the listed fields")
	tr.say(th, "Model")
	tr.waitFor(t, th, "Which model?")
	tr.say(th, "haiku")
	menu2 := waitForNthOut(t, tr, th, "What do you want to change?", 4) // 3rd was the re-ask after "nope"
	if menu2.Prompt.Options[0].Description != "haiku" {
		t.Fatalf("menu after model change = %+v", menu2.Prompt.Options)
	}
	tr.say(th, "tools")
	tr.waitFor(t, th, "Pre-approved tools")
	tr.say(th, "Read, Edit, Bash(go test:*)")
	waitForNth(t, tr, th, "What do you want to change?", 5)
	tr.say(th, "Save")
	tr.waitFor(t, th, "agent *reviewer* updated")
	def, err := st.GetDefinition(ctx, "reviewer")
	if err != nil || def.Model != "haiku" || strings.Join(def.AllowedTools, ",") != "Read,Edit,Bash(go test:*)" || def.PermissionMode != agent.PermissionManual || def.Kind != "fake" {
		t.Fatalf("edited definition = %+v err=%v", def, err)
	}
	mu.Lock()
	if len(updated) != 1 || updated[0].Name != "reviewer" || updated[0].Model != "haiku" {
		t.Fatalf("UpdateDefinition calls = %+v", updated)
	}
	mu.Unlock()

	// Edit via the picker, change the environment, cancel: nothing persisted.
	th2 := transport.ThreadID("C-dev/21.0")
	tr.say(th2, "agent edit")
	pick := tr.waitFor(t, th2, "Which agent do you want to edit?")
	if pick.Prompt == nil || len(pick.Prompt.Options) != 3 {
		t.Fatalf("picker = %+v", pick.Prompt)
	}
	tr.decide(th2, pick.Prompt.ID, "coder")
	if m := tr.waitFor(t, th2, "What do you want to change?"); !strings.Contains(m.Text, "· env FOO") {
		t.Fatalf("env not shown:\n%s", m.Text)
	}
	tr.say(th2, "Environment")
	tr.waitFor(t, th2, "Where does it run?")
	tr.say(th2, "docker")
	tr.waitFor(t, th2, "Docker image?")
	tr.say(th2, "ghcr.io/x/claude")
	tr.waitFor(t, th2, "How long should the container live?")
	tr.say(th2, "thread")
	tr.waitFor(t, th2, "Host directory to mount")
	tr.say(th2, "none")
	m := waitForNthOut(t, tr, th2, "What do you want to change?", 2)
	if !strings.Contains(m.Text, "docker ghcr.io/x/claude · provisioned · container per thread · directory dancer manages") || strings.Contains(m.Text, "env FOO") {
		t.Fatalf("menu after environment change (env must not carry over to another kind):\n%s", m.Text)
	}
	tr.say(th2, "Cancel")
	tr.waitFor(t, th2, "agent edit cancelled")
	if def, _ := st.GetDefinition(ctx, "coder"); def.Environment.Kind != environment.KindLocal || def.Environment.Workdir != workdir {
		t.Fatalf("cancelled edit persisted: %+v", def)
	}

	// Delete: defaults are protected; confirmation; the picker works too.
	th3 := transport.ThreadID("C-dev/22.0")
	tr.say(th3, "agent delete coder")
	tr.waitFor(t, th3, "global default agent")
	tr.say(th3, "agent delete reviewer")
	tr.waitFor(t, th3, "default agent on slack/C-review")
	tr.say(th3, "agent delete tester")
	q := tr.waitFor(t, th3, "Delete agent *tester*")
	if q.Prompt == nil || len(q.Prompt.Options) != 2 || q.Prompt.Options[0].Label != "Delete" {
		t.Fatalf("confirm prompt = %+v", q.Prompt)
	}
	tr.say(th3, "no")
	tr.waitFor(t, th3, "agent delete cancelled")
	if _, err := st.GetDefinition(ctx, "tester"); err != nil {
		t.Fatalf("tester deleted without confirmation: %v", err)
	}
	tr.say(th3, "agent delete")
	p := tr.waitFor(t, th3, "Which agent do you want to delete?")
	tr.decide(th3, p.Prompt.ID, "tester")
	q2 := waitForNthOut(t, tr, th3, "Delete agent *tester*", 2)
	tr.decide(th3, q2.Prompt.ID, "Delete")
	tr.waitFor(t, th3, "agent *tester* deleted")
	if _, err := st.GetDefinition(ctx, "tester"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tester still stored: %v", err)
	}
	mu.Lock()
	if len(deleted) != 1 || deleted[0] != "tester" {
		t.Fatalf("DeleteDefinition calls = %v", deleted)
	}
	mu.Unlock()
	tr.say(th3, "agents")
	if o := tr.waitFor(t, th3, "*reviewer*"); strings.Contains(o.Text, "tester") {
		t.Fatalf("deleted agent still listed: %s", o.Text)
	}
	if flows, _ := st.ListFlows(ctx); len(flows) != 0 {
		t.Fatalf("flow left behind: %+v", flows)
	}
}

// waitForNthOut waits for the nth outbound on th containing sub and returns it.
func waitForNthOut(t *testing.T, tr *fakeTransport, th transport.ThreadID, sub string, n int) transport.Outbound {
	t.Helper()
	waitForNth(t, tr, th, sub, n)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	seen := 0
	for _, o := range tr.out {
		if o.Thread == th && strings.Contains(o.Text, sub) {
			seen++
			if seen == n {
				return o
			}
		}
	}
	return transport.Outbound{}
}
