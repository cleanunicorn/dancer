package claude

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
)

// fakeProc stands in for the claude process: the test writes its stdout
// and reads what dispatch sent to its stdin.
type fakeProc struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	exited  chan struct{}
	code    int         // what Wait reports once exited
	got     chan string // every line dispatch wrote to stdin
}

func newFakeProc() *fakeProc {
	f := &fakeProc{exited: make(chan struct{}), got: make(chan string, 16)}
	f.stdinR, f.stdinW = io.Pipe()
	f.stdoutR, f.stdoutW = io.Pipe()
	go func() {
		sc := bufio.NewScanner(f.stdinR)
		for sc.Scan() {
			f.got <- sc.Text()
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
		f.stdoutW.Close()
	}
}
func (f *fakeProc) say(line string) { io.WriteString(f.stdoutW, line+"\n") }

// wrote returns the next stdin line containing sub, or "" after wait.
func (f *fakeProc) wrote(sub string, wait time.Duration) string {
	deadline := time.After(wait)
	for {
		select {
		case l := <-f.got:
			if strings.Contains(l, sub) {
				return l
			}
		case <-deadline:
			return ""
		}
	}
}

func newTestRun(f *fakeProc) *run {
	r := &run{proc: f, events: make(chan agent.Event, 64), pending: map[string]pendingPerm{}, done: make(chan struct{})}
	go r.loop()
	return r
}

func next(t *testing.T, r *run) agent.Event {
	t.Helper()
	select {
	case ev := <-r.events:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return agent.Event{}
	}
}

const (
	initLine   = `{"type":"system","subtype":"init","session_id":"s1","apiKeySource":"none","model":"m"}`
	resultLine = `{"type":"result","subtype":"success","session_id":"s1","result":"hi","total_cost_usd":0.01}`
	errorLine  = `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","result":"limit reached"}`
)

// A subscription result goes out at once; the usage the CLI reports
// after it follows as its own event.
func TestUsageFollowsResult(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	if ev := next(t, r); ev.Type != agent.EventInit || ev.Billing != agent.BillingSubscription {
		t.Fatalf("init = %+v", ev)
	}
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Usage != nil || ev.Cost != 0.01 || ev.Billing != agent.BillingSubscription {
		t.Fatalf("result = %+v", ev)
	}
	if req := f.wrote(`"get_usage"`, 2*time.Second); !strings.Contains(req, `"request_id":"usage-1"`) {
		t.Fatalf("get_usage request = %q", req)
	}
	f.say(usageLine)
	ev := next(t, r)
	if ev.Type != agent.EventUsage || ev.Usage == nil || len(ev.Usage.Windows) != 3 || ev.Usage.Windows[0].Used != 3 || ev.Billing != agent.BillingSubscription {
		t.Fatalf("usage event = %+v usage = %+v", ev, ev.Usage)
	}
	// An error turn asks too: a spent window is the likeliest cause.
	f.say(errorLine)
	if ev := next(t, r); ev.Type != agent.EventError {
		t.Fatalf("error = %+v", ev)
	}
	if req := f.wrote(`"get_usage"`, 2*time.Second); !strings.Contains(req, `"request_id":"usage-2"`) {
		t.Fatalf("second request = %q", req)
	}
	// A CLI that cannot answer is not asked again.
	f.say(`{"type":"control_response","response":{"subtype":"error","request_id":"usage-2","error":"get_usage is not supported"}}`)
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult {
		t.Fatalf("third result = %+v", ev)
	}
	if req := f.wrote(`"get_usage"`, 200*time.Millisecond); req != "" {
		t.Fatalf("asked again after an error answer: %s", req)
	}
	f.exit()
	if _, ok := <-r.events; ok {
		t.Fatal("events not closed")
	}
}

// An API-key session is metered: no question asked.
func TestAPIKeyAsksNoUsage(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(`{"type":"system","subtype":"init","session_id":"s1","apiKeySource":"ANTHROPIC_API_KEY","model":"m"}`)
	if ev := next(t, r); ev.Billing != agent.BillingAPIKey {
		t.Fatalf("init = %+v", ev)
	}
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Billing != agent.BillingAPIKey {
		t.Fatalf("result = %+v", ev)
	}
	if req := f.wrote(`"get_usage"`, 200*time.Millisecond); req != "" {
		t.Fatalf("asked for usage on an API key: %s", req)
	}
	f.exit()
}

// The driver reads "/model <name>" as it passes so it can report the
// switch on the turn's result; everything else is just text.
func TestModelArg(t *testing.T) {
	cases := []struct{ text, want string }{
		{"/model opus", "opus"},
		{"  /model sonnet  ", "sonnet"},
		{"/model claude-opus-4-1-20250805", "claude-opus-4-1-20250805"},
		{"/model\topus", "opus"},
		{"/model", ""},                             // a question, not a switch
		{"/model ", ""},                            //
		{"/model opus and then fix the build", ""}, // not something the CLI switches on
		{"/clear", ""},
		{"/compact", ""},
		{"tell me about /model opus", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := modelArg(c.text); got != c.want {
			t.Errorf("modelArg(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// A "/model …" goes to the CLI untouched — the CLI is what runs it —
// and the turn's result reports what the session switched to, so the
// next --resume can ask for it again.
func TestModelSwitchReportedOnResult(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	next(t, r)

	if err := r.Send(context.Background(), "/model opus"); err != nil {
		t.Fatal(err)
	}
	if sent := f.wrote(`/model opus`, 2*time.Second); sent == "" {
		t.Fatal("the command did not reach the CLI as written")
	}
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Model != "opus" {
		t.Fatalf("result = %+v, want the switch to opus", ev)
	}
	// Spent: the next turn reports no switch.
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Model != "" {
		t.Fatalf("second result = %+v, want no model", ev)
	}

	// A turn that failed switched nothing, and does not leave the note
	// behind for the next one either.
	if err := r.Send(context.Background(), "/model sonnet"); err != nil {
		t.Fatal(err)
	}
	f.wrote(`/model sonnet`, 2*time.Second)
	f.say(errorLine)
	if ev := next(t, r); ev.Type != agent.EventError || ev.Model != "" {
		t.Fatalf("error = %+v, want no model", ev)
	}
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Model != "" {
		t.Fatalf("result after a failed switch = %+v, want no model", ev)
	}

	// Any other command is text like any other: nothing is noted.
	if err := r.Send(context.Background(), "/clear"); err != nil {
		t.Fatal(err)
	}
	f.wrote(`/clear`, 2*time.Second)
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Model != "" {
		t.Fatalf("result after /clear = %+v, want no model", ev)
	}
	f.exit()
}
