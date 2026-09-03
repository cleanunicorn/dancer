package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// TestWorkflowRowSurvivesARestart: the workflows table is what a restart
// resumes a run from — one row per thread, replaced in place, JSON whole.
func TestWorkflowRowSurvivesARestart(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	st := workflow.Start(workflow.Definition{
		Name:  "feature",
		Steps: []workflow.Step{{Name: "implement", Prompt: "{{.Ask}}"}},
	}, transport.ThreadID("C-dev/1.0"), "slack", "chat", "ship it", "u1", time.Now())
	st.Step = 1
	st.Status = workflow.RunWaiting
	if err := s.PutWorkflow(ctx, *st); err != nil {
		t.Fatal(err)
	}
	st.Status = workflow.RunRunning
	if err := s.PutWorkflow(ctx, *st); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListWorkflows(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v err=%v", runs, err)
	}
	got := runs[0]
	if got.Thread != "C-dev/1.0" || got.Status != workflow.RunRunning || got.Ask != "ship it" ||
		len(got.Def.Steps) != 1 || got.Def.Steps[0].Name != "implement" {
		t.Fatalf("run = %+v", got)
	}
	if err := s.DeleteWorkflow(ctx, "C-dev/1.0"); err != nil {
		t.Fatal(err)
	}
	if runs, _ := s.ListWorkflows(ctx); len(runs) != 0 {
		t.Fatalf("runs after delete = %+v", runs)
	}
}
