package coordinator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/decider"
	"github.com/cleanunicorn/dispatch/internal/environment"
	envlocal "github.com/cleanunicorn/dispatch/internal/environment/local"
	"github.com/cleanunicorn/dispatch/internal/executor"
	execlocal "github.com/cleanunicorn/dispatch/internal/executor/local"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/store/sqlite"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/surface/chat"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// TestFactsReadTheThreadBack builds a thread's history in the log the way
// the coordinator does, then checks the question a decider would be asked.
func TestFactsReadTheThreadBack(t *testing.T) {
	th := transport.ThreadID("C-dev/40.0")
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	c := New(st, nil, nil, nil, nil)

	log := func(kind string, task string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, store.Record{At: time.Now(), Task: executor.TaskID(task), Thread: th, Kind: kind, Payload: b}); err != nil {
			t.Fatal(err)
		}
	}
	log("inbound", "", transport.Inbound{Transport: "slack", Thread: th, Text: "run coder fix the flaky retry test"})
	log("agent", "t-1", agent.Event{Type: agent.EventInit, Session: "sess-1"})
	log("agent", "t-1", agent.Event{Type: agent.EventText, Partial: true, Text: "I'll look at"}) // deltas are noise
	log("agent", "t-1", agent.Event{Type: agent.EventText, Text: "Looking at the retry helper first."})
	log("agent", "t-1", agent.Event{Type: agent.EventToolUse, Tool: "Edit", ToolID: "u1",
		ToolInput: map[string]any{"file_path": "/repo/retry.go"}})
	log("agent", "t-1", agent.Event{Type: agent.EventToolResult, ToolID: "u1"})
	log("agent", "t-1", agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u2",
		ToolInput: map[string]any{"command": "go test ./...\nwith a second line"}})
	// u2 never came back: that is the tool call the stop cut short.

	task := store.TaskState{ID: "t-1", Thread: th, Prompt: "fix the flaky retry test",
		Status: store.StatusInterrupted, Session: "sess-1", Resumes: 1,
		Definition: agent.Definition{Name: "coder", Environment: environment.Spec{Kind: environment.KindLocal}},
		UpdatedAt:  time.Now().Add(-90 * time.Second)}
	f := c.factsForResume(ctx, task)

	if f.LastHumanMessage != "run coder fix the flaky retry test" {
		t.Errorf("last human message = %q", f.LastHumanMessage)
	}
	if f.AgentLastWords != "Looking at the retry helper first." {
		t.Errorf("agent last words = %q", f.AgentLastWords)
	}
	if len(f.FilesTouched) != 1 || f.FilesTouched[0] != "/repo/retry.go" {
		t.Errorf("files touched = %v", f.FilesTouched)
	}
	if !strings.Contains(f.ToolInFlight, "Bash") || !strings.Contains(f.ToolInFlight, "go test") {
		t.Errorf("tool in flight = %q", f.ToolInFlight)
	}
	if strings.Contains(f.ToolInFlight, "\n") {
		t.Errorf("tool in flight kept a newline: %q", f.ToolInFlight)
	}
	if f.MinutesAgo != 1 || f.PreviousResumes != 1 || f.StatusAtStop != store.StatusInterrupted {
		t.Errorf("facts = %+v", f)
	}
	for _, want := range []string{"text Looking at the retry helper first.", "tool_use Edit /repo/retry.go"} {
		if !containsLine(f.RecentEvents, want) {
			t.Errorf("recent events %v missing %q", f.RecentEvents, want)
		}
	}
	for _, line := range f.RecentEvents {
		if strings.HasPrefix(line, "text I'll look at") {
			t.Errorf("streaming delta leaked into the facts: %v", f.RecentEvents)
		}
	}
}

// TestFactsAreThisTasksOwn: an earlier task on the same thread does not
// lend this one its last words, and the outbound copies dispatch posted
// do not push the human's message out of the window.
func TestFactsAreThisTasksOwn(t *testing.T) {
	th := transport.ThreadID("C-dev/41.0")
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	c := New(st, nil, nil, nil, nil)
	log := func(kind string, task string, v any) {
		b, _ := json.Marshal(v)
		if _, err := st.Append(ctx, store.Record{At: time.Now(), Task: executor.TaskID(task), Thread: th, Kind: kind, Payload: b}); err != nil {
			t.Fatal(err)
		}
	}
	// Task A ran to the end on this thread.
	log("inbound", "", transport.Inbound{Transport: "slack", Thread: th, Text: "run coder ship the feature"})
	log("agent", "t-a", agent.Event{Type: agent.EventToolUse, Tool: "Edit", ToolID: "a1", ToolInput: map[string]any{"file_path": "/repo/a.go"}})
	log("agent", "t-a", agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "a2", ToolInput: map[string]any{"command": "git push"}})
	log("agent", "t-a", agent.Event{Type: agent.EventResult, Text: "Committed and pushed. Nothing left to do."})
	// Task B was started and cut short after one tool call — but its turn
	// was verbose: every agent event also left an outbound copy.
	log("inbound", "", transport.Inbound{Transport: "slack", Thread: th, Text: "run coder now add the retries"})
	log("agent", "t-b", agent.Event{Type: agent.EventInit, Session: "sess-b"})
	for i := 0; i < factRecords*2; i++ {
		log("agent", "t-b", agent.Event{Type: agent.EventToolUse, Tool: "Read", ToolID: "b", ToolInput: map[string]any{"file_path": "/repo/b.go"}})
		log("agent", "t-b", agent.Event{Type: agent.EventToolResult, ToolID: "b"})
		log("outbound", "t-b", transport.Outbound{Thread: th, Text: "📖 Read /repo/b.go"})
		log("outbound", "t-b", transport.Outbound{Thread: th, Text: "…"})
	}
	log("agent", "t-b", agent.Event{Type: agent.EventToolUse, Tool: "Edit", ToolID: "b9", ToolInput: map[string]any{"file_path": "/repo/retry.go"}})

	f := c.factsForResume(ctx, store.TaskState{ID: "t-b", Thread: th, Session: "sess-b", Status: store.StatusInterrupted,
		Definition: agent.Definition{Name: "coder", Environment: environment.Spec{Kind: environment.KindLocal}}})
	if f.LastHumanMessage != "run coder now add the retries" {
		t.Errorf("last human message = %q (pushed out by outbound records?)", f.LastHumanMessage)
	}
	if f.AgentLastWords != "" {
		t.Errorf("agent last words = %q — those are task A's", f.AgentLastWords)
	}
	for _, p := range f.FilesTouched {
		if p == "/repo/a.go" {
			t.Errorf("files touched %v include task A's", f.FilesTouched)
		}
	}
	if !strings.Contains(f.ToolInFlight, "/repo/retry.go") {
		t.Errorf("tool in flight = %q", f.ToolInFlight)
	}
	if len(f.RecentEvents) > factEvents {
		t.Errorf("%d recent events, cap %d", len(f.RecentEvents), factEvents)
	}
}

// TestFactsAreBounded: a chatty (or hostile) agent cannot flood the question.
func TestFactsAreBounded(t *testing.T) {
	th := transport.ThreadID("C-dev/41.0")
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	c := New(st, nil, nil, nil, nil)
	for i := 0; i < 40; i++ {
		b, _ := json.Marshal(agent.Event{Type: agent.EventText, Text: strings.Repeat("noise ", 400)})
		if _, err := st.Append(ctx, store.Record{At: time.Now(), Task: "t-1", Thread: th, Kind: "agent", Payload: b}); err != nil {
			t.Fatal(err)
		}
	}
	f := c.factsForResume(ctx, store.TaskState{ID: "t-1", Thread: th})
	if len(f.RecentEvents) > factEvents {
		t.Fatalf("recent events = %d, cap %d", len(f.RecentEvents), factEvents)
	}
	for _, line := range f.RecentEvents {
		if len(line) > factLine+4 {
			t.Fatalf("event line is %d chars", len(line))
		}
	}
	if len(f.AgentLastWords) > factParagraph+4 {
		t.Fatalf("agent last words is %d chars", len(f.AgentLastWords))
	}
}

// TestAskPutsTheChoiceToTheThread: the "ask" verdict renders a question
// with buttons, and the answer decides what happens to the task.
func TestAskPutsTheChoiceToTheThread(t *testing.T) {
	for _, tc := range []struct {
		name, choice, want string
		status             string
	}{
		{"continue resumes it", "continue", "echo:" + defaultResumePrompt, store.StatusIdle},
		{"drop leaves it", "drop", "⏹️ dropped", store.StatusCancelled},
		// The terminal types its answers: the human's own words are the
		// turn to hand the agent, not a drop.
		{"own words resume it", "yes, keep going and run the tests", "echo:yes, keep going and run the tests", store.StatusIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &stubDecider{verdict: decider.Verdict{Action: actionAsk,
				Prompt: "The tests were still running when dispatch stopped. Finish the run?",
				Reason: "could go either way"}}
			tr, st, th := startWithDecider(t, d, []string{kindResume})

			q := tr.waitFor(t, th, "The tests were still running when dispatch stopped")
			if q.Prompt == nil || len(q.Prompt.Options) != 2 {
				t.Fatalf("question prompt = %+v", q.Prompt)
			}
			tr.decide(th, q.Prompt.ID, tc.choice)
			tr.waitFor(t, th, tc.want)

			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				got, _ := st.GetTask(context.Background(), "t-1")
				if got.Status == tc.status {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			got, _ := st.GetTask(context.Background(), "t-1")
			t.Fatalf("task status = %q, want %q", got.Status, tc.status)
		})
	}
}

// TestStaleResumeAnswerIsIgnored: the question stays clickable for hours.
// If the human replied instead and the task ran another turn, a later
// click must not write the pre-restart snapshot back over it.
func TestStaleResumeAnswerIsIgnored(t *testing.T) {
	d := &stubDecider{verdict: decider.Verdict{Action: actionAsk, Prompt: "Finish the run?"}}
	tr, st, th := startWithDecider(t, d, []string{kindResume})

	q := tr.waitFor(t, th, "Finish the run?")

	// The human types instead of clicking: the task resumes, finishes its
	// turn and lands with a newer session.
	tr.say(th, "yes please, and add a test")
	tr.waitFor(t, th, "echo:yes please, and add a test")
	deadline := time.Now().Add(3 * time.Second)
	var moved store.TaskState
	for time.Now().Before(deadline) {
		moved, _ = st.GetTask(context.Background(), "t-1")
		if moved.LastSeq > 0 && moved.Status == store.StatusIdle {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if moved.LastSeq == 0 {
		t.Fatalf("the follow-up turn did not land: %+v", moved)
	}

	// Now the stale click arrives.
	tr.decide(th, q.Prompt.ID, "drop")
	time.Sleep(300 * time.Millisecond)
	after, _ := st.GetTask(context.Background(), "t-1")
	if after.Status == store.StatusCancelled {
		t.Fatalf("a stale click cancelled a task that had moved on: %+v", after)
	}
	if after.LastSeq != moved.LastSeq || after.Session != moved.Session {
		t.Fatalf("a stale click clobbered newer state: %+v, was %+v", after, moved)
	}
}

// TestAbandonStopsOfferingTheTask: abandoned tasks are cancelled, say why,
// and are not picked up by the next restart either.
func TestAbandonStopsOfferingTheTask(t *testing.T) {
	d := &stubDecider{verdict: decider.Verdict{Action: actionAbandon, Reason: "the branch was merged an hour ago"}}
	tr, st, th := startWithDecider(t, d, []string{kindResume})

	tr.waitFor(t, th, "leaving this task: the branch was merged an hour ago")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := st.GetTask(context.Background(), "t-1")
		if got.Status == store.StatusCancelled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := st.GetTask(context.Background(), "t-1")
	if got.Status != store.StatusCancelled {
		t.Fatalf("task status = %q, want cancelled", got.Status)
	}

	// A second start must not offer it again: cancelled is not a status
	// recover() picks up.
	tr2 := &fakeTransport{name: "slack", ready: make(chan struct{})}
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	c2 := New(st, ex, []transport.Transport{tr2}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c2.AutoResume = true
	c2.Decider = d
	c2.DeciderUses = []string{kindResume}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c2.Run(ctx)
	<-tr2.ready
	time.Sleep(300 * time.Millisecond)
	tr2.mu.Lock()
	defer tr2.mu.Unlock()
	if len(tr2.out) != 0 {
		t.Fatalf("abandoned task came back: %+v", tr2.out)
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
