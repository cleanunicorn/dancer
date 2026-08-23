package claude

import (
	"bufio"
	"io"
	"strings"
	"sync"
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

	mu    sync.Mutex
	lines []string // everything dancer wrote to stdin
	got   chan string
}

func newFakeProc() *fakeProc {
	f := &fakeProc{exited: make(chan struct{}), got: make(chan string, 16)}
	f.stdinR, f.stdinW = io.Pipe()
	f.stdoutR, f.stdoutW = io.Pipe()
	go func() {
		sc := bufio.NewScanner(f.stdinR)
		for sc.Scan() {
			f.mu.Lock()
			f.lines = append(f.lines, sc.Text())
			f.mu.Unlock()
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

// wrote returns the next stdin line containing sub, or "" after a while.
func (f *fakeProc) wrote(sub string) string {
	for {
		select {
		case l := <-f.got:
			if strings.Contains(l, sub) {
				return l
			}
		case <-time.After(2 * time.Second):
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

// A subscription result waits for the usage the CLI reports after it.
func TestResultCarriesUsage(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	if ev := next(t, r); ev.Type != agent.EventInit || ev.Billing != agent.BillingSubscription {
		t.Fatalf("init = %+v", ev)
	}
	f.say(resultLine)
	req := f.wrote(`"get_usage"`)
	if !strings.Contains(req, `"request_id":"usage-1"`) {
		t.Fatalf("get_usage request = %q", req)
	}
	select {
	case ev := <-r.events:
		t.Fatalf("result %+v came before its usage", ev)
	case <-time.After(50 * time.Millisecond):
	}
	f.say(usageLine)
	ev := next(t, r)
	if ev.Type != agent.EventResult || ev.Usage == nil || len(ev.Usage.Windows) != 3 || ev.Usage.Windows[0].Used != 3 || ev.Cost != 0.01 {
		t.Fatalf("result = %+v usage = %+v", ev, ev.Usage)
	}
	// The next turn asks again, under a new id, and a late or failed
	// answer lets the result through without usage.
	f.say(resultLine)
	if req := f.wrote(`"get_usage"`); !strings.Contains(req, `"request_id":"usage-2"`) {
		t.Fatalf("second request = %q", req)
	}
	f.say(`{"type":"control_response","response":{"subtype":"error","request_id":"usage-2","error":"get_usage is not supported"}}`)
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Usage != nil {
		t.Fatalf("result after error = %+v", ev)
	}
	f.exit()
	if _, ok := <-r.events; ok {
		t.Fatal("events not closed")
	}
}

// When the CLI never answers, the result goes out after usageTimeout.
func TestResultWithoutUsageAfterTimeout(t *testing.T) {
	old := usageTimeout
	usageTimeout = 50 * time.Millisecond
	defer func() { usageTimeout = old }()
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	next(t, r)
	start := time.Now()
	f.say(resultLine)
	if f.wrote(`"get_usage"`) == "" {
		t.Fatal("no get_usage request")
	}
	ev := next(t, r)
	if ev.Type != agent.EventResult || ev.Usage != nil || ev.Billing != agent.BillingSubscription {
		t.Fatalf("result = %+v", ev)
	}
	if time.Since(start) < usageTimeout {
		t.Fatal("result did not wait for the usage")
	}
	f.exit()
}

// An API-key session is metered: no question, no wait.
func TestAPIKeyResultIsImmediate(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(`{"type":"system","subtype":"init","session_id":"s1","apiKeySource":"ANTHROPIC_API_KEY","model":"m"}`)
	if ev := next(t, r); ev.Billing != agent.BillingAPIKey {
		t.Fatalf("init = %+v", ev)
	}
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Usage != nil || ev.Billing != agent.BillingAPIKey {
		t.Fatalf("result = %+v", ev)
	}
	f.exit()
	for range r.events {
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.lines {
		if strings.Contains(l, "get_usage") {
			t.Fatalf("asked for usage on an API key: %s", l)
		}
	}
}
