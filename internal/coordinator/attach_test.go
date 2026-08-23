package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// A file sent with a message lands in the agent's environment and the
// agent is told where — on a follow-up to a live task and, with the
// channel's default agent, on a message that is nothing but the file.
func TestAttachmentsReachTheAgent(t *testing.T) {
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
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 500*time.Millisecond)
	ex.InboxDir = t.TempDir()
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/1.0")
	tr.say(th, "run coder do the thing")
	prompt := tr.waitFor(t, th, "wants to run")
	tr.decide(th, prompt.Prompt.ID, "allow")
	tr.waitFor(t, th, "✅ done")

	// Follow-up with a file: the live process gets the text plus the path.
	tr.inbox <- transport.Inbound{Transport: "slack", Thread: th, UserID: "u1", Text: "what is this?", Files: []transport.File{{Name: "shot.png", Data: []byte("png")}}}
	echo := tr.waitFor(t, th, "echo:what is this?")
	if !strings.Contains(echo.Text, "Files attached to this message") || !strings.Contains(echo.Text, "/shot.png") {
		t.Errorf("agent was told: %q", echo.Text)
	}
	staged, _ := filepath.Glob(filepath.Join(ex.InboxDir, "*", "shot.png"))
	if len(staged) != 1 {
		t.Fatalf("staged files: %v", staged)
	}
	if b, _ := os.ReadFile(staged[0]); string(b) != "png" {
		t.Errorf("staged content: %q", b)
	}

	// A file alone on a fresh thread starts a task with the default agent.
	th2 := transport.ThreadID("C-dev/2.0")
	tr.inbox <- transport.Inbound{Transport: "slack", Thread: th2, UserID: "u1", Files: []transport.File{{Name: "log.txt", Data: []byte("boom")}}}
	tr.waitFor(t, th2, "started with agent *coder*")
	tr.waitFor(t, th2, "wants to run")
	if staged, _ := filepath.Glob(filepath.Join(ex.InboxDir, "*", "log.txt")); len(staged) != 1 {
		t.Errorf("second task's file: %v", staged)
	}

	// A bare `run` cannot take files: the picker asks for text later.
	th3 := transport.ThreadID("C-dev/3.0")
	tr.inbox <- transport.Inbound{Transport: "slack", Thread: th3, UserID: "u1", Text: "run", Files: []transport.File{{Name: "x.png"}}}
	tr.waitFor(t, th3, "attachments are dropped")
}
