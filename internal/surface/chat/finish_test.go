package chat

import (
	"context"
	"reflect"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// TestHandleFinishCommands: `review` and `merge` are dispatch's words only
// when they are the whole message. "review the auth code" and "merge main
// into this branch" are prompts a human would reasonably type, and a bare
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
		{"merge", surface.MergePR{Thread: th}},
		{"merge it", surface.MergePR{Thread: th, Method: "it"}},
		{"merge squash", surface.MergePR{Thread: th, Method: "squash"}},
		{"merge rebase", surface.MergePR{Thread: th, Method: "rebase"}},
		{"MERGE MERGE", surface.MergePR{Thread: th, Method: "MERGE"}},
		// Prompts that begin with the same word pass through untouched.
		{"review the auth code", surface.FollowUp{Thread: th, Text: "review the auth code"}},
		{"review #51 for me", surface.FollowUp{Thread: th, Text: "review #51 for me"}},
		{"merge main into this branch", surface.FollowUp{Thread: th, Text: "merge main into this branch"}},
		{"merge the two helpers", surface.FollowUp{Thread: th, Text: "merge the two helpers"}},
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
	in.Text = "merge"
	if got, _ := s.Handle(context.Background(), in); got[0].(surface.MergePR).User != "U42" {
		t.Errorf("merge: user = %q", got[0].(surface.MergePR).User)
	}
}
