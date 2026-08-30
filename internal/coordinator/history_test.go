package coordinator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/executor"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/store/sqlite"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// TestHistoryToolsAndFacts: History.Messages hands an observer the
// agent's tool calls as Tool entries, each paired with its result —
// done, failed, refused, or still open — in the order of the log, and
// Threads carries what is working on the thread.
func TestHistoryToolsAndFacts(t *testing.T) {
	th := transport.ThreadID("C-dev/41.0")
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	c := New(st, nil, nil, nil, nil)

	at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	log := func(kind string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, store.Record{At: at, Task: "t-1", Thread: th, Kind: kind, Payload: b}); err != nil {
			t.Fatal(err)
		}
		at = at.Add(1500 * time.Millisecond)
	}
	log("inbound", transport.Inbound{Transport: "slack", Thread: th, UserID: "U1", UserName: "ana", Text: "run coder fix it"})
	log("agent", agent.Event{Type: agent.EventInit, Session: "sess-1", Model: "claude-haiku-4-5"})
	log("agent", agent.Event{Type: agent.EventText, Partial: true, Text: "I'll"}) // not a message
	log("agent", agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u1", ToolInput: map[string]any{"command": "go test ./...", "description": "tests"}})
	log("agent", agent.Event{Type: agent.EventToolResult, ToolID: "u1"})
	log("agent", agent.Event{Type: agent.EventToolUse, Tool: "Edit", ToolID: "u2", ToolInput: map[string]any{"file_path": "/repo/a.go", "old_string": "x"}})
	log("agent", agent.Event{Type: agent.EventToolResult, ToolID: "u2", Tool: "error", Text: "no match"})
	log("agent", agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u3", ParentID: "sub-1", ToolInput: map[string]any{"command": "rm -rf /"}})
	log("agent", agent.Event{Type: agent.EventToolDenied, Tool: "Bash", ToolID: "u3", Text: "policy"})
	log("outbound", transport.Outbound{Thread: th, Text: "Fixed.", Markdown: true})
	log("agent", agent.Event{Type: agent.EventToolUse, Tool: "Grep", ToolID: "u4", ToolInput: map[string]any{"pattern": "TODO"}})
	log("agent", agent.Event{Type: agent.EventToolResult, ToolID: "u9"}) // a call older than the window: dropped

	msgs, err := c.Messages(ctx, th, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range msgs {
		switch {
		case e.Tool != nil:
			kinds = append(kinds, "tool:"+e.Tool.Name)
		case e.Message.From != nil:
			kinds = append(kinds, "from")
		default:
			kinds = append(kinds, "out")
		}
	}
	want := []string{"from", "tool:Bash", "tool:Edit", "tool:Bash", "out", "tool:Grep"}
	if len(kinds) != len(want) {
		t.Fatalf("entries %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("entries %v, want %v", kinds, want)
		}
	}
	if tc := msgs[1].Tool; !tc.Done || tc.Error || tc.Input != "go test ./..." || tc.Duration != 1500*time.Millisecond || tc.Sub {
		t.Errorf("bash call: %+v", tc)
	}
	if tc := msgs[2].Tool; !tc.Done || !tc.Error || tc.Denied || tc.Input != "/repo/a.go" {
		t.Errorf("edit call: %+v", tc)
	}
	if tc := msgs[3].Tool; !tc.Done || !tc.Error || !tc.Denied || !tc.Sub {
		t.Errorf("denied call: %+v", tc)
	}
	if tc := msgs[5].Tool; tc.Done || tc.Duration != 0 || tc.Input != "TODO" {
		t.Errorf("open call: %+v", tc)
	}

	task := store.TaskState{ID: "t-1", Transport: "slack", Thread: th, Requester: "U1", Session: "sess-1", Model: "claude-haiku-4-5",
		Status: store.StatusIdle, Definition: agent.Definition{Name: "coder", Model: "haiku", Environment: environment.Spec{Kind: environment.KindDocker}}}
	if err := st.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	infos, err := c.Threads(ctx)
	if err != nil || len(infos) != 1 {
		t.Fatalf("threads: %+v err=%v", infos, err)
	}
	if in := infos[0]; in.Agent != "coder" || in.Model != "claude-haiku-4-5" || in.Environment != "docker" || in.Session != "sess-1" || in.Title != "run coder fix it" {
		t.Errorf("thread info: %+v", in)
	}
	// Until the agent reports, the model is the one asked for.
	task.ID, task.Model, task.Thread = "t-2", "", "C-dev/42.0"
	if err := st.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	infos, _ = c.Threads(ctx)
	for _, in := range infos {
		if in.ID == "C-dev/42.0" && in.Model != "haiku" {
			t.Errorf("configured model: %+v", in)
		}
	}
	if err := st.PutDefinition(ctx, agent.Definition{Name: "reviewer", Model: "sonnet", Environment: environment.Spec{Kind: environment.KindLocal}}); err != nil {
		t.Fatal(err)
	}
	agents, err := c.Agents(ctx)
	if err != nil || len(agents) != 1 || agents[0].Name != "reviewer" || agents[0].Model != "sonnet" || agents[0].Environment != "local" {
		t.Errorf("agents: %+v err=%v", agents, err)
	}
	_ = executor.TaskID("")
}
