package terminal

import (
	"bytes"
	"context"
	"testing"

	"github.com/cleanunicorn/dancer/internal/transport"
)

// TestKeyedStatus: on a terminal the status line is redrawn in place and
// closed before ordinary output; on a pipe every update is a line of its
// own and a removal prints nothing.
func TestKeyedStatus(t *testing.T) {
	send := func(c *Transport, msgs ...transport.Outbound) string {
		for _, m := range msgs {
			m.Thread = Thread
			if err := c.Send(context.Background(), m); err != nil {
				t.Fatal(err)
			}
		}
		return c.Out.(*bytes.Buffer).String()
	}
	seq := []transport.Outbound{
		{Key: "status", Text: "⏳ starting · 0s"},
		{Key: "status", Text: "⏳ thinking · 4s"},
		{Text: "hello"},
		{Key: "status", Text: "⏳ thinking · 5s"},
		{Key: "status"},
		{Text: "✅ done"},
	}
	if got, want := send(&Transport{Out: &bytes.Buffer{}, Redraw: true}, seq...), "⏳ starting · 0s\r\033[K⏳ thinking · 4s\nhello\n⏳ thinking · 5s\r\033[K✅ done\n"; got != want {
		t.Errorf("redraw:\n got %q\nwant %q", got, want)
	}
	if got, want := send(&Transport{Out: &bytes.Buffer{}}, seq...), "⏳ starting · 0s\n⏳ thinking · 4s\nhello\n⏳ thinking · 5s\n✅ done\n"; got != want {
		t.Errorf("pipe:\n got %q\nwant %q", got, want)
	}
}
