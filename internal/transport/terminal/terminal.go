// Package terminal is a stdin/stdout transport. One constant thread. Used
// for local testing without Slack.
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

	mu     sync.Mutex
	prompt *transport.Prompt // last open prompt, answered by typing a choice
}

// New returns a terminal transport on os.Stdin/os.Stdout.
func New() *Transport { return &Transport{In: os.Stdin, Out: os.Stdout} }

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
	fmt.Fprintln(c.Out, msg.Text)
	if msg.Prompt != nil {
		c.prompt = msg.Prompt
		if len(msg.Prompt.Options) > 0 {
			fmt.Fprintf(c.Out, "[1-%d or text] > ", len(msg.Prompt.Options))
		} else {
			fmt.Fprintf(c.Out, "[%s] > ", strings.Join(msg.Prompt.Choices, "/"))
		}
	}
	return nil
}

// answer maps a typed line to a decision value: a choice name, an option
// number or label, or free text when the prompt allows it.
func answer(p *transport.Prompt, line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	if len(p.Options) == 0 {
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
