package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	envlocal "github.com/cleanunicorn/dispatch/internal/environment/local"
	"github.com/cleanunicorn/dispatch/internal/executor"
)

// echoAgent answers every prompt and follow-up with a result that
// repeats it, so a test can see exactly what the agent was told.
type echoAgent struct{}

func (echoAgent) Kind() agent.Kind { return "echo" }
func (echoAgent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	r := &fakeRun{events: make(chan agent.Event, 16), decided: make(chan agent.PermissionDecision, 1), done: make(chan struct{})}
	go func() {
		r.emit(agent.Event{Type: agent.EventInit, Session: "s1"})
		r.emit(agent.Event{Type: agent.EventResult, Text: "prompt:" + prompt, Session: "s1"})
	}()
	return r, nil
}
func (a echoAgent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	return a.Start(ctx, env, def, prompt)
}

// Attachments are copied into the task's inbox before the agent starts,
// and again on a follow-up, and the message the agent gets lists where.
func TestAttachmentsStagedIntoTheEnvironment(t *testing.T) {
	ex := New(map[agent.Kind]agent.Agent{"echo": echoAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 300*time.Millisecond)
	ex.InboxDir = t.TempDir()
	sink := &recSink{}
	task := executor.Task{
		ID:         "task7",
		Definition: agent.Definition{Kind: "echo", Environment: environment.Spec{Workdir: t.TempDir()}},
		Prompt:     "what is in this screenshot?",
		Files:      []agent.File{{Name: "Screenshot 2026-08-23 at 10.00.png", Data: []byte("png1")}},
	}
	errCh := make(chan error, 1)
	go func() { errCh <- ex.Run(context.Background(), task, sink) }()

	first := waitText(t, sink, "prompt:")
	inbox := filepath.Join(ex.InboxDir, "task7")
	want := "what is in this screenshot?\n\nFiles attached to this message, copied from the chat into this environment:\n- " + inbox + "/Screenshot_2026-08-23_at_10.00.png"
	if first != "prompt:"+want {
		t.Errorf("prompt:\n%s\nwant:\n%s", first, want)
	}
	if b, err := os.ReadFile(filepath.Join(inbox, "Screenshot_2026-08-23_at_10.00.png")); err != nil || string(b) != "png1" {
		t.Errorf("staged file: %q, %v", b, err)
	}

	// A follow-up with no text and three files that collide: two named
	// image.png and one really named image-2.png. Each gets a name of its
	// own — the rename skips past the name already taken — and the message
	// is just the list.
	files := []agent.File{
		{Name: "image.png", Data: []byte("a")},
		{Name: "image-2.png", Data: []byte("b")},
		{Name: "image.png", Data: []byte("c")},
	}
	if err := ex.Send(context.Background(), "task7", "", files); err != nil {
		t.Fatal(err)
	}
	follow := waitText(t, sink, "follow:")
	want = "follow:Files attached to this message, copied from the chat into this environment:\n- " + inbox + "/image.png\n- " + inbox + "/image-2.png\n- " + inbox + "/image-3.png"
	if follow != want {
		t.Errorf("follow-up:\n%s\nwant:\n%s", follow, want)
	}
	for name, data := range map[string]string{"image.png": "a", "image-2.png": "b", "image-3.png": "c"} {
		if b, err := os.ReadFile(filepath.Join(inbox, name)); err != nil || string(b) != data {
			t.Errorf("%s: %q, %v", name, b, err)
		}
	}

	// No files: the text goes through untouched.
	if err := ex.Send(context.Background(), "task7", "thanks", nil); err != nil {
		t.Fatal(err)
	}
	if got := waitText(t, sink, "follow:thanks"); got != "follow:thanks" {
		t.Errorf("plain follow-up: %q", got)
	}
	if err := ex.Cancel(context.Background(), "task7"); err != nil {
		t.Fatal(err)
	}
	<-errCh
}

// waitText returns the first text/result event starting with prefix.
func waitText(t *testing.T, sink *recSink, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range sink.texts() {
			if strings.HasPrefix(s, prefix) {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no text starting with %q; got %q", prefix, sink.texts())
	return ""
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"shot.png":                        "shot.png",
		"Screenshot 2026.png":             "Screenshot_2026.png",
		"../../etc/passwd":                "passwd",
		"C:\\Users\\me\\notes.md":         "notes.md",
		"":                                "file",
		"..":                              "file",
		"résumé.pdf":                      "r_sum_.pdf",
		strings.Repeat("a", 120) + ".txt": strings.Repeat("a", 96) + ".txt",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}
