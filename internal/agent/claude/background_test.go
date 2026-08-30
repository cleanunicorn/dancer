package claude

import (
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
)

// A captured session (claude 2.1.240, --input-format stream-json): the
// model launches a sub-agent, its turn ends with a result while the
// sub-agent still runs, and when the sub-agent finishes the CLI starts a
// second turn — init, text, result. Only the second result ends the turn.
func TestBackgroundFixture(t *testing.T) {
	var bg background
	var results []bool // settled at each result line
	scanFixture(t, "background.jsonl", func(p parsed) {
		bg.observe(p)
		for _, ev := range p.Events {
			if ev.Type == agent.EventResult {
				results = append(results, bg.settled())
			}
		}
	})
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

// launchAgent plays the start of a turn in which the model launches a
// sub-agent: init, the Agent call, task_started, the "launched" result of
// the call. The three events it produces are consumed.
func launchAgent(t *testing.T, f *fakeProc, r *run) {
	t.Helper()
	f.say(initLine)
	next(t, r) // init
	f.say(agentCall)
	next(t, r) // tool_use
	f.say(agentStart)
	f.say(agentLaunch)
	next(t, r) // tool_result
}

// The result that ends the model's turn while a sub-agent runs is held
// back; the one after the CLI's follow-up turn is the turn's end, and the
// first get_usage is asked for that one only.
func TestResultWaitsForSubAgent(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	launchAgent(t, f, r)
	f.say(waitingLine)
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
		t.Fatalf("get_usage after the real result = %q, want the first one asked", req)
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
	launchAgent(t, f, r)
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
	f.say(`{"type":"system","subtype":"task_started","task_id":"b2","owned_by_subagent":true,"tool_use_id":"toolu_9","is_backgrounded":true,"task_type":"local_agent","session_id":"s1"}`)
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult {
		t.Fatalf("result = %+v", ev)
	}
	f.exit()
}

// A shell command started with run_in_background may never end (a dev
// server, a watcher): its result is not held, whatever the CLI does when
// it does end.
func TestBackgroundCommandHoldsNothing(t *testing.T) {
	f := newFakeProc()
	r := newTestRun(f)
	f.say(initLine)
	next(t, r)
	f.say(`{"type":"system","subtype":"task_started","task_id":"b3","tool_use_id":"toolu_3","description":"npm run dev","is_backgrounded":true,"task_type":"local_bash","session_id":"s1"}`)
	f.say(resultLine)
	if ev := next(t, r); ev.Type != agent.EventResult {
		t.Fatalf("result = %+v", ev)
	}
	f.exit()
}

// If the process ends on a held result, nothing more is coming: that
// result was the end after all — on a later turn of the process as much
// as on its first, and whether or not the exit was clean (a cancel kills
// a CLI that is still driving its sub-agent).
func TestHeldResultDeliveredOnExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{{"clean", 0}, {"killed", 137}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeProc()
			f.code = tc.code
			r := newTestRun(f)
			f.say(initLine)
			next(t, r)
			f.say(resultLine)
			if ev := next(t, r); ev.Type != agent.EventResult || ev.Text != "hi" {
				t.Fatalf("first turn's result = %+v", ev)
			}
			launchAgent(t, f, r) // the follow-up turn
			f.say(waitingLine)
			f.exit()
			if ev := next(t, r); ev.Type != agent.EventResult || ev.Text != "waiting for the agent" || ev.Billing != agent.BillingSubscription {
				t.Fatalf("result on exit = %+v", ev)
			}
			if ev, ok := <-r.events; ok {
				t.Fatalf("extra event after the held result: %+v", ev)
			}
		})
	}
}

// A process that dies mid-turn without any result is still reported,
// even when an earlier turn of it had one.
func TestExitErrorPerTurn(t *testing.T) {
	f := newFakeProc()
	f.code = 1
	r := newTestRun(f)
	f.say(initLine)
	next(t, r)
	f.say(resultLine)
	next(t, r)
	f.say(initLine) // a follow-up turn that never finishes
	next(t, r)
	f.exit()
	if ev := next(t, r); ev.Type != agent.EventError || !strings.Contains(ev.Text, "exited with code 1") {
		t.Fatalf("on exit = %+v, want the exit error", ev)
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
	f.say(errorLine)
	if ev := next(t, r); ev.Type != agent.EventError {
		t.Fatalf("error = %+v", ev)
	}
	f.exit()
}
