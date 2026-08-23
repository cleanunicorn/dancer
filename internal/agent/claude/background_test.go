package claude

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
)

// A captured session (claude 2.1.240, --input-format stream-json): the
// model launches a sub-agent, its turn ends with a result while the
// sub-agent still runs, and when the sub-agent finishes the CLI starts a
// second turn — init, text, result. Only the second result ends the turn.
func TestBackgroundFixture(t *testing.T) {
	f, err := os.Open("testdata/background.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var bg background
	var results []bool // settled at each result line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		p, err := translate(sc.Bytes(), time.Now())
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		bg.observe(p)
		for _, ev := range p.Events {
			if ev.Type == agent.EventResult {
				results = append(results, bg.settled())
			}
		}
	}
	if len(results) != 2 || results[0] || !results[1] {
		t.Fatalf("settled at each result = %v, want [false true]", results)
	}
}

const (
	agentCall   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Agent","input":{"prompt":"ping"}}]},"parent_tool_use_id":null,"session_id":"s1"}`
	agentStart  = `{"type":"system","subtype":"task_started","task_id":"a1","tool_use_id":"toolu_1","is_backgrounded":true,"task_type":"local_agent","session_id":"s1"}`
	agentLaunch = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"Async agent launched successfully."}]},"parent_tool_use_id":null,"session_id":"s1"}`
	agentDone   = `{"type":"system","subtype":"task_notification","task_id":"a1","tool_use_id":"toolu_1","status":"completed","summary":"pong","session_id":"s1"}`
	waitingLine = `{"type":"result","subtype":"success","session_id":"s1","result":"waiting for the agent","total_cost_usd":0.01}`
	pongLine    = `{"type":"result","subtype":"success","session_id":"s1","result":"pong","total_cost_usd":0.02}`
)

// The result that ends the model's turn while a sub-agent runs is held
// back; the one after the CLI's follow-up turn is the turn's end.
func TestResultWaitsForSubAgent(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	next(t, r) // init
	f.say(agentCall)
	next(t, r) // tool_use
	f.say(agentStart)
	f.say(agentLaunch)
	next(t, r) // tool_result
	f.say(waitingLine)
	if req := f.wrote(`"get_usage"`, 200*time.Millisecond); req != "" {
		t.Fatalf("asked for usage on a held result: %s", req)
	}
	// The sub-agent speaks with parent_tool_use_id set: not a main-session request.
	f.say(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"pong"}]},"parent_tool_use_id":"toolu_1","session_id":"s1"}`)
	if ev := next(t, r); ev.Type != agent.EventText || ev.ParentID != "toolu_1" {
		t.Fatalf("sub-agent text = %+v", ev)
	}
	f.say(agentDone)
	f.say(initLine)
	next(t, r) // the follow-up turn's init
	f.say(pongLine)
	ev := next(t, r)
	if ev.Type != agent.EventResult || ev.Text != "pong" || ev.Billing != agent.BillingSubscription {
		t.Fatalf("result = %+v, want the follow-up turn's", ev)
	}
	if req := f.wrote(`"get_usage"`, 2*time.Second); !strings.Contains(req, `"request_id":"usage-1"`) {
		t.Fatalf("get_usage after the real result = %q", req)
	}
	f.exit()
	if ev, ok := <-r.events; ok {
		t.Fatalf("extra event after exit: %+v", ev)
	}
}

// A sub-agent that finishes while the model is still calling tools is
// reported inside the same turn: the next tool_result is the model's next
// request, so the result that follows is the end.
func TestSubAgentReportedInTurn(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	next(t, r)
	f.say(agentCall)
	next(t, r)
	f.say(agentStart)
	f.say(agentLaunch)
	next(t, r)
	f.say(agentDone)
	f.say(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"sleep 1"}}]},"parent_tool_use_id":null,"session_id":"s1"}`)
	next(t, r)
	// The CLI also reports foreground tool calls as tasks; they hold nothing.
	f.say(`{"type":"system","subtype":"task_started","task_id":"b1","tool_use_id":"toolu_2","is_backgrounded":false,"task_type":"local_bash","session_id":"s1"}`)
	f.say(`{"type":"system","subtype":"task_notification","task_id":"b1","tool_use_id":"toolu_2","status":"completed","session_id":"s1"}`)
	f.say(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_2","type":"tool_result","content":""}]},"parent_tool_use_id":null,"session_id":"s1"}`)
	next(t, r)
	f.say(pongLine)
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Text != "pong" {
		t.Fatalf("result = %+v", ev)
	}
	f.exit()
}

// A sub-agent's own background work is the sub-agent's to wait for.
func TestSubAgentOwnedTaskHoldsNothing(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	next(t, r)
	f.say(`{"type":"system","subtype":"task_started","task_id":"b2","owned_by_subagent":true,"tool_use_id":"toolu_9","is_backgrounded":true,"task_type":"local_bash","session_id":"s1"}`)
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult {
		t.Fatalf("result = %+v", ev)
	}
	f.exit()
}

// If the process ends cleanly on a held result, nothing more is coming:
// that result was the end after all.
func TestHeldResultDeliveredOnExit(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	next(t, r)
	f.say(agentCall)
	next(t, r)
	f.say(agentStart)
	f.say(agentLaunch)
	next(t, r)
	f.say(waitingLine)
	f.exit()
	if ev := next(t, r); ev.Type != agent.EventResult || ev.Text != "waiting for the agent" || ev.Billing != agent.BillingSubscription {
		t.Fatalf("result on exit = %+v", ev)
	}
	if _, ok := <-r.events; ok {
		t.Fatal("events not closed")
	}
}

// An error result is never held: the process may be about to go away
// and the human must hear why.
func TestErrorResultNotHeld(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	next(t, r)
	f.say(agentStart)
	f.say(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","result":"limit reached"}`)
	if ev := next(t, r); ev.Type != agent.EventError {
		t.Fatalf("error = %+v", ev)
	}
	f.exit()
}
