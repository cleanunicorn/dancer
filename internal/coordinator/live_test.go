package coordinator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/decider"
	"github.com/cleanunicorn/dancer/internal/environment"
	"github.com/cleanunicorn/dancer/internal/executor"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/store/sqlite"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// TestLiveResumeVerdicts asks the real decider about three interrupted
// tasks of different shapes, with the facts read out of a real event log.
// Run with DANCER_LIVE=1; three haiku calls, a few cents.
//
// Only the decisive shapes are pinned: a verdict is always one of the
// offered actions, work plainly under way continues, and work that has
// failed the same way three times across two resumes does not. The third
// shape — the agent said it was done — is left unpinned on purpose: both
// "abandon, it is finished" and "continue, but only to verify" are sane
// readings of it, and the model has given each on different runs. The test
// logs what it chose.
func TestLiveResumeVerdicts(t *testing.T) {
	if os.Getenv("DANCER_LIVE") == "" {
		t.Skip("set DANCER_LIVE=1 to run against the real claude CLI")
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	c := New(st, nil, nil, nil, nil)
	c.Decider = decider.Claude{Model: "haiku", Timeout: 60 * time.Second}
	c.DeciderUses = []string{kindResume}
	c.DeciderTimeout = 60 * time.Second

	def := agent.Definition{Name: "coder", Environment: environment.Spec{Kind: environment.KindLocal}}
	appendEvent := func(th transport.ThreadID, task executor.TaskID, kind string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, store.Record{At: time.Now(), Task: task, Thread: th, Kind: kind, Payload: b}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Cut off mid-work: a test run was in flight, files already changed.
	th1 := transport.ThreadID("C/1.0")
	appendEvent(th1, "", "inbound", transport.Inbound{Thread: th1, Text: "add retries to the HTTP client and run the tests"})
	appendEvent(th1, "live-1", "agent", agent.Event{Type: agent.EventText, Text: "Adding backoff to client.go, then I'll run the suite."})
	appendEvent(th1, "live-1", "agent", agent.Event{Type: agent.EventToolUse, Tool: "Edit", ToolID: "a1", ToolInput: map[string]any{"file_path": "/repo/client.go"}})
	appendEvent(th1, "live-1", "agent", agent.Event{Type: agent.EventToolResult, ToolID: "a1"})
	appendEvent(th1, "live-1", "agent", agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "a2", ToolInput: map[string]any{"command": "go test ./..."}})

	// 2. Same failure, twice already retried.
	th2 := transport.ThreadID("C/2.0")
	appendEvent(th2, "", "inbound", transport.Inbound{Thread: th2, Text: "make the docker build work"})
	for i := 0; i < 3; i++ {
		appendEvent(th2, "live-2", "agent", agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "b", ToolInput: map[string]any{"command": "docker build ."}})
		appendEvent(th2, "live-2", "agent", agent.Event{Type: agent.EventText, Text: "Build failed again: no space left on device."})
	}

	// 3. Work finished and reported before the stop.
	th3 := transport.ThreadID("C/3.0")
	appendEvent(th3, "", "inbound", transport.Inbound{Thread: th3, Text: "bump the go version in CI"})
	appendEvent(th3, "live-3", "agent", agent.Event{Type: agent.EventToolUse, Tool: "Edit", ToolID: "c1", ToolInput: map[string]any{"file_path": "/repo/.github/workflows/ci.yml"}})
	appendEvent(th3, "live-3", "agent", agent.Event{Type: agent.EventToolResult, ToolID: "c1"})
	appendEvent(th3, "live-3", "agent", agent.Event{Type: agent.EventResult, Text: "Bumped Go to 1.24 in ci.yml, committed and pushed. Nothing left to do."})

	cases := []struct {
		name string
		task store.TaskState
		// want is the action this shape has to produce; "" means the
		// judgement is genuinely open and only the options are checked.
		want string
	}{
		{"cut off mid-work", store.TaskState{ID: "live-1", Thread: th1, Definition: def, Session: "s1",
			Status: store.StatusInterrupted, Prompt: "add retries to the HTTP client and run the tests",
			UpdatedAt: time.Now().Add(-2 * time.Minute)}, actionContinue},
		{"failing the same way", store.TaskState{ID: "live-2", Thread: th2, Definition: def, Session: "s2",
			Status: store.StatusInterrupted, Prompt: "make the docker build work", Resumes: 2,
			UpdatedAt: time.Now().Add(-30 * time.Minute)}, "not " + actionContinue},
		{"already finished", store.TaskState{ID: "live-3", Thread: th3, Definition: def, Session: "s3",
			Status: store.StatusInterrupted, Prompt: "bump the go version in CI",
			UpdatedAt: time.Now().Add(-10 * time.Minute)}, ""},
	}
	options := []string{actionContinue, actionWait, actionAsk, actionAbandon}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := c.factsForResume(ctx, tc.task)
			v := c.decide(ctx, decider.Question{
				Kind: kindResume, Task: string(tc.task.ID), Thread: string(tc.task.Thread),
				Options: options,
				Facts:   facts,
				Static:  decider.Verdict{Action: actionContinue, Prompt: c.resumePrompt()},
			})
			t.Logf("facts: %+v", facts)
			t.Logf("verdict: %s by %s — %s (prompt: %s)", v.Action, v.By, v.Reason, v.Prompt)
			if !slices.Contains(options, v.Action) {
				t.Fatalf("verdict escaped the options: %+v", v)
			}
			if v.By != "claude" {
				t.Fatalf("the live decider did not answer: %+v", v)
			}
			switch tc.want {
			case "":
			case "not " + actionContinue:
				if v.Action == actionContinue {
					t.Errorf("action = %q, want anything but %q", v.Action, actionContinue)
				}
			default:
				if v.Action != tc.want {
					t.Errorf("action = %q, want %q", v.Action, tc.want)
				}
			}
			if v.Action == actionContinue && v.Prompt == "" {
				t.Error("a continue verdict should carry the turn to hand the agent")
			}
		})
	}
}
