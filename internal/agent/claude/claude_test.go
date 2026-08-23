package claude

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
)

// fakeProc stands in for the claude process: the test writes its stdout
// and reads what dancer sent to its stdin.
type fakeProc struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	exited  chan struct{}
	got     chan string // every line dancer wrote to stdin
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
func (f *fakeProc) Wait() (int, error)    { <-f.exited; return 0, nil }
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

const initLine = `{"type":"system","subtype":"init","session_id":"s1","apiKeySource":"none","model":"m"}`
const resultLine = `{"type":"result","subtype":"success","session_id":"s1","result":"hi","total_cost_usd":0.01}`

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
	f.say(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","result":"limit reached"}`)
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
