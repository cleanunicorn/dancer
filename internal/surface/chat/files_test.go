package chat

import (
	"context"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// Attachments ride along on prompts and follow-ups, even without text;
// commands drop them.
func TestHandleCarriesFiles(t *testing.T) {
	s := New("chat", "slack", false)
	th := transport.ThreadID("C1/1.0")
	files := []transport.File{{Name: "shot.png", Data: []byte("png")}}
	handle := func(text string) surface.Intent {
		t.Helper()
		got, ok := s.Handle(context.Background(), transport.Inbound{Transport: "slack", Thread: th, UserID: "U1", Text: text, Files: files})
		if !ok || len(got) != 1 {
			t.Fatalf("Handle(%q) = %v, %v", text, got, ok)
		}
		return got[0]
	}
	if it, ok := handle("run coder what is this?").(surface.RunTask); !ok || len(it.Files) != 1 || it.Prompt != "what is this?" {
		t.Errorf("run: %+v", it)
	}
	if it, ok := handle("and this?").(surface.FollowUp); !ok || len(it.Files) != 1 || it.Text != "and this?" {
		t.Errorf("follow-up: %+v", it)
	}
	if it, ok := handle("").(surface.FollowUp); !ok || len(it.Files) != 1 || it.Text != "" {
		t.Errorf("files alone: %+v", it)
	}
	if _, ok := handle("status").(surface.Status); !ok {
		t.Errorf("status with a file is still status")
	}
	// Nothing at all is still nothing.
	if got, ok := s.Handle(context.Background(), transport.Inbound{Transport: "slack", Thread: th, Text: " "}); !ok || got != nil {
		t.Errorf("empty: %v, %v", got, ok)
	}
}
