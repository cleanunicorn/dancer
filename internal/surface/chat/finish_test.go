package chat

import (
	"context"
	"reflect"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// TestHandleFinishCommands: `review` and `ship` are dispatch's words only
// when they are the whole message. "review the auth code" and "ship this
// behind a flag" are prompts a human would reasonably type, and a bare
// word that ate them would be worse than not having one.
func TestHandleFinishCommands(t *testing.T) {
	s := New("chat", "slack", false)
	th := transport.ThreadID("C1/1.0")
	cases := []struct {
		text string
		want surface.Intent
	}{
		{"review", surface.ReviewPR{Thread: th}},
		{"Review", surface.ReviewPR{Thread: th}},
		{"  review  ", surface.ReviewPR{Thread: th}},
		{"ship", surface.Ship{Thread: th}},
		{"ship it", surface.Ship{Thread: th, Method: "it"}},
		{"ship squash", surface.Ship{Thread: th, Method: "squash"}},
		{"ship rebase", surface.Ship{Thread: th, Method: "rebase"}},
		{"SHIP MERGE", surface.Ship{Thread: th, Method: "MERGE"}},
		// Prompts that begin with the same word pass through untouched.
		{"review the auth code", surface.FollowUp{Thread: th, Text: "review the auth code"}},
		{"review #51 for me", surface.FollowUp{Thread: th, Text: "review #51 for me"}},
		{"ship this behind a flag", surface.FollowUp{Thread: th, Text: "ship this behind a flag"}},
	}
	for _, tc := range cases {
		got, ok := s.Handle(context.Background(), transport.Inbound{Transport: "slack", Thread: th, Text: tc.text})
		if !ok || len(got) != 1 {
			t.Fatalf("%q: handled=%v intents=%d", tc.text, ok, len(got))
		}
		if !reflect.DeepEqual(got[0], tc.want) {
			t.Errorf("%q: got %#v, want %#v", tc.text, got[0], tc.want)
		}
	}
}

func TestFinishCommandsCarryTheUser(t *testing.T) {
	s := New("chat", "slack", false)
	in := transport.Inbound{Transport: "slack", Thread: "C1/1.0", UserID: "U42", Text: "review"}
	if got, _ := s.Handle(context.Background(), in); got[0].(surface.ReviewPR).User != "U42" {
		t.Errorf("review: user = %q", got[0].(surface.ReviewPR).User)
	}
	in.Text = "ship"
	if got, _ := s.Handle(context.Background(), in); got[0].(surface.Ship).User != "U42" {
		t.Errorf("ship: user = %q", got[0].(surface.Ship).User)
	}
}
