package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/transport"
)

func TestFormatCost(t *testing.T) {
	cases := map[agent.Billing]string{
		agent.BillingSubscription: "≈$2.27 API-equiv",
		agent.BillingAPIKey:       "$2.269",
		agent.BillingUnknown:      "$2.269",
	}
	for b, want := range cases {
		if got := FormatCost(&agent.Event{Cost: 2.269, Billing: b}); got != want {
			t.Errorf("%q: got %q want %q", b, got, want)
		}
	}
}

func TestHelpListsClose(t *testing.T) {
	if !strings.Contains(help, "`close`") {
		t.Fatal("help does not mention close")
	}
}

func TestHandleCommands(t *testing.T) {
	s := New("chat", "slack", false)
	th := transport.ThreadID("C1/1.0")
	cases := []struct {
		text string
		want surface.Intent
	}{
		{"run coder fix the build", surface.RunTask{Thread: th, Agent: "coder", Prompt: "fix the build"}},
		{"run coder", surface.RunTask{Thread: th, Agent: "coder"}},
		{"run", surface.RunTask{Thread: th}},
		{"default", surface.SetDefault{Thread: th}},
		{"default coder", surface.SetDefault{Thread: th, Agent: "coder"}},
		{"fix the build", surface.FollowUp{Thread: th, Text: "fix the build"}},
		{"close", surface.CloseThread{Thread: th}},
		{"Close", surface.CloseThread{Thread: th}},
		{"cancel", surface.Cancel{Thread: th}},
		{"agent", surface.ListAgents{Thread: th}},
		{"agent list", surface.ListAgents{Thread: th}},
		{"agent add", surface.AddAgent{Thread: th}},
		{"agent edit", surface.EditAgent{Thread: th}},
		{"agent edit coder", surface.EditAgent{Thread: th, Agent: "coder"}},
		{"Agent Update coder", surface.EditAgent{Thread: th, Agent: "coder"}},
		{"agent delete coder", surface.DeleteAgent{Thread: th, Agent: "coder"}},
		{"agent rm", surface.DeleteAgent{Thread: th}},
		{"add agent config to the readme", surface.FollowUp{Thread: th, Text: "add agent config to the readme"}},
		{"delete the old files", surface.FollowUp{Thread: th, Text: "delete the old files"}},
		{"edit main.go", surface.FollowUp{Thread: th, Text: "edit main.go"}},
	}
	for _, c := range cases {
		got, ok := s.Handle(context.Background(), transport.Inbound{Transport: "slack", Thread: th, Text: c.text})
		if !ok || len(got) != 1 || got[0] != c.want {
			t.Errorf("%q → %+v (ok=%v), want %+v", c.text, got, ok, c.want)
		}
	}
	for _, text := range []string{"default a b", "agent add now", "agent edit coder to use opus", "agent delete coder please", "agent frobnicate", "agent list all", "add agent", "edit agent coder", "delete agent"} {
		got, _ := s.Handle(context.Background(), transport.Inbound{Transport: "slack", Thread: th, Text: text})
		if say, ok := got[0].(surface.Say); !ok || !strings.Contains(say.Text, "usage") {
			t.Errorf("%q → %+v", text, got)
		}
	}
}

func TestRenderInit(t *testing.T) {
	task := &store.TaskState{Definition: agent.Definition{Name: "coder", Kind: agent.KindClaude, Environment: environment.Spec{Kind: environment.KindDocker, Image: "dancer/dev", Workdir: "/cfg"}}}
	full := &agent.Event{Type: agent.EventInit, Model: "claude-haiku-4-5-20251001", Mode: agent.PermissionAcceptEdits,
		Version: "2.1.239", Billing: agent.BillingSubscription, Workdir: "/work"}
	const fullLine = "🤖 `claude-haiku-4-5-20251001` · acceptEdits · claude 2.1.239 · subscription · docker dancer/dev /work"
	cases := []struct {
		name    string
		verbose bool
		agent   *agent.Event
		want    string // "" = nothing posted
	}{
		{"full", true, full, fullLine},
		// A quiet surface posts it too: it is the answer to "what am I talking to".
		{"quiet", false, full, fullLine},
		// Older CLIs report less; the configured workdir stands in.
		{"sparse", true, &agent.Event{Type: agent.EventInit, Model: "m", Mode: agent.PermissionManual}, "🤖 `m` · manual · docker dancer/dev /cfg"},
		// An agent that reports nothing beyond a session id has nothing to say.
		{"bare", true, &agent.Event{Type: agent.EventInit, Session: "s"}, ""},
		// A sub-agent's init is not the session the human talks to.
		{"sub-agent", true, &agent.Event{Type: agent.EventInit, Model: "m", ParentID: "toolu_1"}, ""},
	}
	for _, c := range cases {
		s := New("chat", "slack", c.verbose)
		out := s.Render(surface.Event{Kind: surface.EventAgent, Thread: "C1/1.0", Task: task, Agent: c.agent})
		got := ""
		if len(out) > 1 {
			t.Fatalf("%s: got %d messages", c.name, len(out))
		} else if len(out) == 1 {
			got = out[0].Text
		}
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestRenderInitOncePerThread(t *testing.T) {
	s := New("chat", "slack", false)
	task := &store.TaskState{Definition: agent.Definition{Name: "coder", Kind: agent.KindClaude, Environment: environment.Spec{Kind: environment.KindLocal}}}
	init := func(thread transport.ThreadID, model string) surface.Event {
		return surface.Event{Kind: surface.EventAgent, Thread: thread, Task: task, Agent: &agent.Event{Type: agent.EventInit, Model: model, Mode: agent.PermissionManual}}
	}
	if got := s.Render(init("C1/1.0", "m1")); len(got) != 1 {
		t.Fatalf("first init: %d messages", len(got))
	}
	// An idle resume reports the same details: nothing new to say.
	if got := s.Render(init("C1/1.0", "m1")); got != nil {
		t.Errorf("repeat init posted again: %q", got[0].Text)
	}
	// A change is news; another thread has not seen it at all.
	if got := s.Render(init("C1/1.0", "m2")); len(got) != 1 {
		t.Errorf("changed init not posted")
	}
	if got := s.Render(init("C2/1.0", "m1")); len(got) != 1 {
		t.Errorf("other thread not posted")
	}
}
