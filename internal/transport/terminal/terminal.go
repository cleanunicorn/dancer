// Package terminal is a stdin/stdout transport. One constant thread. Used
// for local testing without Slack.
//
// Keyed messages (Outbound.Key, the live status line of a running task)
// are redrawn in place on the last line when Out is a terminal, and
// printed as ordinary lines when it is a pipe, so logs and the e2e script
// still see every update.
package terminal

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/cleanunicorn/dancer/internal/transport"
)

// Thread is the single thread id used by the terminal.
const Thread transport.ThreadID = "terminal"

// Transport reads from In and writes to Out.
type Transport struct {
	In  io.Reader
	Out io.Writer
	// Redraw: keyed messages overwrite their own line instead of adding
	// one per update. New sets it when Out is a terminal.
	Redraw bool

	mu     sync.Mutex
	prompt *transport.Prompt // last open prompt, answered by typing a choice
	open   string            // key of the status line drawn on the current line, "" when the line is closed
}

// New returns a terminal transport on os.Stdin/os.Stdout.
func New() *Transport {
	t := &Transport{In: os.Stdin, Out: os.Stdout}
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		t.Redraw = true
	}
	return t
}

func (c *Transport) Name() string { return "terminal" }

func (c *Transport) Run(ctx context.Context, inbox chan<- transport.Inbound) error {
	fmt.Fprintln(c.Out, "dancer terminal — type `help` for commands")
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(c.In)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				<-ctx.Done()
				return ctx.Err()
			}
			in := transport.Inbound{Transport: "terminal", Thread: Thread, UserID: "local", Text: line}
			c.mu.Lock()
			if p := c.prompt; p != nil {
				if choice, ok := answer(p, line); ok {
					in.Decision = &transport.Decision{PromptID: p.ID, Choice: choice}
					in.Text = ""
					c.prompt = nil
				}
			}
			c.mu.Unlock()
			select {
			case inbox <- in:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (c *Transport) Send(ctx context.Context, msg transport.Outbound) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if msg.Key != "" {
		c.status(msg)
		return nil
	}
	c.closeLine()
	if msg.Text != "" {
		fmt.Fprintln(c.Out, msg.Text)
	}
	for _, f := range msg.Files {
		fmt.Fprintf(c.Out, "📎 %s (%d bytes)\n", f.Name, len(f.Data))
	}
	if msg.Prompt != nil {
		c.prompt = msg.Prompt
		switch {
		case len(msg.Prompt.Options) > 0:
			fmt.Fprintf(c.Out, "[1-%d or text] > ", len(msg.Prompt.Options))
		case len(msg.Prompt.Choices) > 0:
			fmt.Fprintf(c.Out, "[%s] > ", strings.Join(msg.Prompt.Choices, "/"))
		default:
			fmt.Fprint(c.Out, "> ")
		}
	}
	return nil
}

// status draws a keyed message. With Redraw it replaces whatever status
// line is on the current line (there is one thread, so one status at a
// time); without it, every non-empty update is its own line and a
// removal prints nothing.
func (c *Transport) status(msg transport.Outbound) {
	if !c.Redraw {
		if msg.Text != "" {
			fmt.Fprintln(c.Out, msg.Text)
		}
		return
	}
	if c.open != "" {
		fmt.Fprint(c.Out, "\r\033[K")
		c.open = ""
	}
	if msg.Text == "" {
		return
	}
	fmt.Fprint(c.Out, msg.Text)
	c.open = msg.Key
}

// closeLine ends a status line drawn in place so ordinary output starts
// on a fresh line.
func (c *Transport) closeLine() {
	if c.open != "" {
		fmt.Fprintln(c.Out)
		c.open = ""
	}
}

// answer maps a typed line to a decision value: a choice name, an option
// number or label, or free text when the prompt allows it.
func answer(p *transport.Prompt, line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	if len(p.Options) == 0 {
		if len(p.Choices) == 0 {
			return line, p.FreeText
		}
		l := strings.ToLower(line)
		for _, ch := range p.Choices {
			if ch == l {
				return ch, true
			}
		}
		return "", false
	}
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(p.Options) {
		return p.Options[n-1].Value, true
	}
	for _, o := range p.Options {
		if strings.EqualFold(o.Label, line) || strings.EqualFold(o.Value, line) {
			return o.Value, true
		}
	}
	return line, p.FreeText
}
