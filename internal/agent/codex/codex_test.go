package codex

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	envlocal "github.com/cleanunicorn/dispatch/internal/environment/local"
)

type fakeProc struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	exited  chan struct{}
	got     chan string
	code    int
}

func newFakeProc() *fakeProc {
	f := &fakeProc{exited: make(chan struct{}), got: make(chan string, 32)}
	f.stdinR, f.stdinW = io.Pipe()
	f.stdoutR, f.stdoutW = io.Pipe()
	go func() {
		s := bufio.NewScanner(f.stdinR)
		for s.Scan() {
			f.got <- s.Text()
		}
	}()
	return f
}
func (f *fakeProc) Stdin() io.WriteCloser { return f.stdinW }
func (f *fakeProc) Stdout() io.Reader     { return f.stdoutR }
func (f *fakeProc) Stderr() io.Reader     { return strings.NewReader("") }
func (f *fakeProc) Wait() (int, error)    { <-f.exited; return f.code, nil }
func (f *fakeProc) Kill() error           { f.exit(); return nil }
func (f *fakeProc) exit() {
	select {
	case <-f.exited:
	default:
		close(f.exited)
		_ = f.stdoutW.Close()
	}
}
func (f *fakeProc) say(s string) { _, _ = io.WriteString(f.stdoutW, s+"\n") }
func (f *fakeProc) wrote(t *testing.T, sub string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case s := <-f.got:
			if strings.Contains(s, sub) {
				return s
			}
		case <-deadline:
			t.Fatalf("did not write %q", sub)
		}
	}
}

type fakeEnv struct{ p *fakeProc }

func (e fakeEnv) Kind() environment.Kind      { return environment.KindLocal }
func (e fakeEnv) Start(context.Context) error { return nil }
func (e fakeEnv) Exec(context.Context, string, ...string) (environment.Process, error) {
	return e.p, nil
}
func (e fakeEnv) CopyIn(context.Context, io.Reader, string) error        { return nil }
func (e fakeEnv) CopyOut(context.Context, string) (io.ReadCloser, error) { return nil, io.EOF }
func (e fakeEnv) Stop(context.Context) error                             { return nil }

func next(t *testing.T, r agent.Run) agent.Event {
	t.Helper()
	select {
	case e := <-r.Events():
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return agent.Event{}
	}
}

func TestAppServerRoundTrip(t *testing.T) {
	f := newFakeProc()
	r, err := (&Agent{Binary: "codex"}).Start(context.Background(), fakeEnv{f}, agent.Definition{Kind: agent.KindCodex, Model: "gpt-test", PermissionMode: agent.PermissionManual}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	f.wrote(t, `"method":"initialize"`)
	f.say(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	f.wrote(t, `"method":"initialized"`)
	start := f.wrote(t, `"method":"thread/start"`)
	if !strings.Contains(start, `"approvalPolicy":"untrusted"`) || !strings.Contains(start, `"sandbox":"read-only"`) {
		t.Fatalf("thread start = %s", start)
	}
	f.say(`{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-1","model":"gpt-test"}}}`)
	if e := next(t, r); e.Type != agent.EventInit || e.Session != "thr-1" || e.Model != "gpt-test" {
		t.Fatalf("init = %+v", e)
	}
	f.wrote(t, `"method":"turn/start"`)
	f.say(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-1","turn":{"id":"turn-1"}}}`)
	f.say(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"thr-1","itemId":"msg-1","delta":"hello"}}`)
	if e := next(t, r); e.Type != agent.EventText || !e.Partial || e.Text != "hello" {
		t.Fatalf("delta = %+v", e)
	}
	f.say(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thr-1","item":{"id":"cmd-1","type":"commandExecution","command":"git status"}}}`)
	if e := next(t, r); e.Type != agent.EventToolUse || e.Tool != agent.ToolBash || e.ToolInput["command"] != "git status" {
		t.Fatalf("tool use = %+v", e)
	}
	f.say(`{"jsonrpc":"2.0","id":9,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr-1","itemId":"cmd-1","command":"git status","reason":"inspect"}}`)
	if e := next(t, r); e.Type != agent.EventNeedsPermission || e.ToolID != "cmd-1" || e.Tool != agent.ToolBash {
		t.Fatalf("permission = %+v", e)
	}
	if err := r.Decide(context.Background(), agent.PermissionDecision{ToolID: "cmd-1", Allow: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.wrote(t, `"id":9`); !strings.Contains(got, `"decision":"accept"`) {
		t.Fatalf("approval = %s", got)
	}
	f.say(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-1","item":{"id":"cmd-1","type":"commandExecution","aggregatedOutput":"clean"}}}`)
	if e := next(t, r); e.Type != agent.EventToolResult || e.ToolID != "cmd-1" || e.Text != "clean" {
		t.Fatalf("tool result = %+v", e)
	}
	f.say(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-1","turn":{"id":"turn-1","status":"completed"}}}`)
	if e := next(t, r); e.Type != agent.EventResult {
		t.Fatalf("result = %+v", e)
	}
	f.exit()
}

func TestModes(t *testing.T) {
	for _, tt := range []struct {
		m                 agent.PermissionMode
		approval, sandbox string
	}{
		{agent.PermissionManual, "untrusted", "read-only"}, {agent.PermissionAcceptEdits, "on-request", "workspace-write"}, {agent.PermissionAuto, "on-request", "workspace-write"}, {agent.PermissionBypass, "never", "danger-full-access"},
	} {
		if a, s := mode(tt.m); a != tt.approval || s != tt.sandbox {
			t.Errorf("mode(%q) = %q, %q", tt.m, a, s)
		}
	}
}

// TestLiveAppServer is a cheap protocol smoke test against the installed
// Codex CLI. It is opt-in because it uses the caller's Codex account.
func TestLiveAppServer(t *testing.T) {
	if os.Getenv("DISPATCH_LIVE") != "1" {
		t.Skip("set DISPATCH_LIVE=1 to run against the real Codex CLI")
	}
	env, err := envlocal.Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := New().Start(context.Background(), env, agent.Definition{Kind: agent.KindCodex, PermissionMode: agent.PermissionManual}, "Reply with exactly CODEX_DRIVER_OK. Do not run tools.")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	deadline := time.After(90 * time.Second)
	var text string
	for {
		select {
		case ev, ok := <-r.Events():
			if !ok {
				t.Fatalf("Codex ended without a result; text=%q", text)
			}
			if ev.Type == agent.EventText {
				text = ev.Text
			}
			if ev.Type == agent.EventError {
				t.Fatalf("Codex error: %s", ev.Text)
			}
			if ev.Type == agent.EventResult {
				if text != "CODEX_DRIVER_OK" {
					t.Fatalf("reply = %q", text)
				}
				return
			}
		case <-deadline:
			t.Fatal("Codex app-server did not complete")
		}
	}
}
