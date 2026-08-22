package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/cleanunicorn/dancer/internal/agent"
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
