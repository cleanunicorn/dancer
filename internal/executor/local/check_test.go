package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	envlocal "github.com/cleanunicorn/dispatch/internal/environment/local"
	"github.com/cleanunicorn/dispatch/internal/executor"
)

// A check can only be run while the task's process is alive: once the idle
// timeout has taken it and its container down there is nothing left to run
// the command in, and Check says that rather than reporting a pass.
func TestCheckWithoutALiveTask(t *testing.T) {
	ex := New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	code, out, err := ex.Check(context.Background(), "never-ran", "true")
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
	if code != 0 || out != "" {
		t.Fatalf("code=%d output=%q, want nothing alongside the error", code, out)
	}
}

// A live task's check runs in that task's own environment — the workdir the
// agent worked in — and reports the command's exit code and output, a
// failing one included.
func TestCheckRunsInTheTaskEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("built here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	sink := &recSink{allow: true}
	task := executor.Task{ID: "check1", Definition: agent.Definition{Kind: "fake", Environment: environment.Spec{Workdir: dir}}, Prompt: "go"}
	errCh := make(chan error, 1)
	go func() { errCh <- ex.Run(context.Background(), task, sink) }()

	deadline := time.Now().Add(3 * time.Second)
	for !ex.IsRunning("check1") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ex.IsRunning("check1") {
		t.Fatal("task never started")
	}

	code, out, err := ex.Check(context.Background(), "check1", "cat marker.txt")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if code != 0 || out != "built here" {
		t.Fatalf("code=%d output=%q; the check did not run in the task's workdir", code, out)
	}

	code, out, err = ex.Check(context.Background(), "check1", "echo FAIL: 1 test failed >&2; exit 7")
	if err != nil {
		t.Fatalf("err = %v, want nil: a failing check is an answer", err)
	}
	if code != 7 || out != "FAIL: 1 test failed" {
		t.Fatalf("code=%d output=%q, want 7 and the command's stderr", code, out)
	}

	// And the bound holds for a task that was alive and is not any more.
	if err := ex.Cancel(context.Background(), "check1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not end the run")
	}
	if _, _, err := ex.Check(context.Background(), "check1", "true"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("check after the task ended = %v, want ErrNotRunning", err)
	}
}
