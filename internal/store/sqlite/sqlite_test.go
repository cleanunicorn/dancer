package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/store"
)

func TestStore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var _ store.Store = s

	seq1, err := s.Append(ctx, store.Record{Task: "t1", Thread: "th1", Kind: "inbound", Payload: []byte(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	seq2, _ := s.Append(ctx, store.Record{Task: "t1", Thread: "th1", Kind: "agent", Payload: []byte(`{"b":2}`)})
	if seq2 != seq1+1 {
		t.Fatalf("seq %d %d", seq1, seq2)
	}
	var got []store.Record
	if err := s.Replay(ctx, seq1, func(r store.Record) error { got = append(got, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "agent" || string(got[0].Payload) != `{"b":2}` {
		t.Fatalf("replay = %+v", got)
	}

	def := agent.Definition{Name: "coder", Kind: agent.KindClaude, Model: "haiku", AllowedTools: []string{"Read"}}
	if err := s.PutDefinition(ctx, def); err != nil {
		t.Fatal(err)
	}
	d2, err := s.GetDefinition(ctx, "coder")
	if err != nil || d2.Model != "haiku" || len(d2.AllowedTools) != 1 {
		t.Fatalf("get def = %+v err=%v", d2, err)
	}
	if _, err := s.GetDefinition(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	ts := store.TaskState{ID: "t1", Transport: "slack", Thread: "th1", Definition: def, Status: store.StatusRunning, LastSeq: seq2}
	if err := s.PutTask(ctx, ts); err != nil {
		t.Fatal(err)
	}
	ts.Session = "sess"
	ts.Status = store.StatusIdle
	if err := s.PutTask(ctx, ts); err != nil {
		t.Fatal(err)
	}
	back, err := s.GetTask(ctx, "t1")
	if err != nil || back.Session != "sess" || back.Status != store.StatusIdle || back.Definition.Name != "coder" || back.Transport != "slack" {
		t.Fatalf("get task = %+v err=%v", back, err)
	}
	latest, err := s.LatestTaskForThread(ctx, "th1")
	if err != nil || latest.ID != "t1" {
		t.Fatalf("latest = %+v err=%v", latest, err)
	}
	idle, _ := s.ListTasks(ctx, store.StatusIdle)
	if len(idle) != 1 {
		t.Fatalf("idle = %d", len(idle))
	}
	if err := s.DeleteDefinition(ctx, "coder"); err != nil {
		t.Fatal(err)
	}
	if defs, _ := s.ListDefinitions(ctx); len(defs) != 0 {
		t.Fatalf("defs = %d", len(defs))
	}

	flow := store.FlowState{Thread: "th9", Transport: "slack", Surface: "chat", Kind: "add_agent", Answers: []string{"reviewer"}}
	if err := s.PutFlow(ctx, flow); err != nil {
		t.Fatal(err)
	}
	flow.Answers = append(flow.Answers, "opus")
	if err := s.PutFlow(ctx, flow); err != nil {
		t.Fatal(err)
	}
	flows, err := s.ListFlows(ctx)
	if err != nil || len(flows) != 1 || flows[0].Surface != "chat" || len(flows[0].Answers) != 2 || flows[0].Answers[1] != "opus" {
		t.Fatalf("flows = %+v err=%v", flows, err)
	}
	if err := s.DeleteFlow(ctx, "th9"); err != nil {
		t.Fatal(err)
	}
	if flows, _ := s.ListFlows(ctx); len(flows) != 0 {
		t.Fatalf("flows after delete = %+v", flows)
	}
}
