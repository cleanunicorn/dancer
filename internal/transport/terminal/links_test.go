package terminal

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/transport"
)

// TestLinksOnAPipe: without a terminal there is nothing to hyperlink into,
// so the label keeps its URL beside it — what the e2e script and a log
// read, and what the closing line said before links existed.
func TestLinksOnAPipe(t *testing.T) {
	var out bytes.Buffer
	c := &Transport{In: strings.NewReader(""), Out: &out}
	line := "🔀 " + transport.Link("https://github.com/o/r/pull/51", "#51")
	if err := c.Send(context.Background(), transport.Outbound{Thread: Thread, Text: line}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "🔀 #51 https://github.com/o/r/pull/51\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRelayedTextIsNotMarkup: what a human wrote is theirs, not
// dispatch's markup, so no link is read out of it — otherwise a typed
// "<https://evil|the docs>" would render as a hyperlink nobody wrote.
func TestRelayedTextIsNotMarkup(t *testing.T) {
	var out bytes.Buffer
	c := &Transport{In: strings.NewReader(""), Out: &out, Redraw: true}
	raw := "read <https://evil.example/x|the docs>"
	err := c.Send(context.Background(), transport.Outbound{
		Thread: Thread, Text: raw,
		From: &transport.Author{Name: "dana", Via: "slack"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "💬 dana via slack: "+raw+"\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestLinksOnATerminal: Redraw means Out is a terminal, so the URL goes
// into an OSC 8 hyperlink and only the label is on the line.
func TestLinksOnATerminal(t *testing.T) {
	var out bytes.Buffer
	c := &Transport{In: strings.NewReader(""), Out: &out, Redraw: true}
	line := "🌿 " + transport.Link("https://github.com/o/r/tree/fix", "fix")
	if err := c.Send(context.Background(), transport.Outbound{Thread: Thread, Text: line}); err != nil {
		t.Fatal(err)
	}
	want := "🌿 \033]8;;https://github.com/o/r/tree/fix\033\\fix\033]8;;\033\\\n"
	if got := out.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
