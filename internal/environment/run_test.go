// The tests live in environment_test rather than environment because the
// thing Check needs is an Environment to run in, and the only one that is
// always available is the local folder — a package that imports this one.
package environment_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/environment/local"
)

// localEnv is a started local environment rooted at dir.
func localEnv(t *testing.T, dir string, vars map[string]string) environment.Environment {
	t.Helper()
	env, err := local.Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: dir, Env: vars})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return env
}

// A check that passes says so with exit 0, and its output comes back
// whether or not anything went wrong.
func TestCheckReportsASuccessfulCommand(t *testing.T) {
	env := localEnv(t, t.TempDir(), nil)
	code, out, err := environment.Check(context.Background(), env, "echo ok")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want %q", out, "ok")
	}
}

// A failing check is the answer, not a failure to get one: the non-zero
// exit code is returned as data with err nil, which is the whole of what
// Check is for. Reporting it as an error the way Run does would make every
// failing `check = "make test"` indistinguishable from a broken one.
func TestCheckReportsAFailureAsDataNotAnError(t *testing.T) {
	env := localEnv(t, t.TempDir(), nil)
	code, out, err := environment.Check(context.Background(), env, "echo failing; exit 2")
	if err != nil {
		t.Fatalf("err = %v, want nil: a non-zero exit is not an error", err)
	}
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if out != "failing" {
		t.Fatalf("output = %q, want %q", out, "failing")
	}
}

// What a check prints on stderr is as much evidence as what it prints on
// stdout — a test runner writes its failures there — so both streams are
// captured.
func TestCheckCapturesStderrWithStdout(t *testing.T) {
	env := localEnv(t, t.TempDir(), nil)
	code, out, err := environment.Check(context.Background(), env, "echo to-stdout; echo to-stderr >&2")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	// Which of two simultaneous writes lands first is not promised; that
	// both are there is.
	for _, want := range []string{"to-stdout", "to-stderr"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, missing %q", out, want)
		}
	}
}

// The command is a shell line a human wrote in config, handed to `sh -c`
// whole: `&&`, `||` and pipes are operators, not arguments to a program
// named "make test && make lint".
func TestCheckRunsTheCommandThroughAShell(t *testing.T) {
	env := localEnv(t, t.TempDir(), nil)
	code, out, err := environment.Check(context.Background(), env, "echo one && echo two | tr a-z A-Z")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if code != 0 || out != "one\nTWO" {
		t.Fatalf("code=%d output=%q, want 0 and %q", code, out, "one\nTWO")
	}
	// And the shell's own short-circuiting decides the exit code: the
	// second half of "make test && make lint" never runs when the first
	// half fails.
	code, out, err = environment.Check(context.Background(), env, "false && echo unreachable")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if code == 0 || out != "" {
		t.Fatalf("code=%d output=%q, want a non-zero code and no output", code, out)
	}
}

// The output is trimmed, so a check's trailing newline never turns up in
// the message a human reads.
func TestCheckTrimsTheOutput(t *testing.T) {
	env := localEnv(t, t.TempDir(), nil)
	_, out, err := environment.Check(context.Background(), env, "printf '\\n\\n  make: nothing to be done  \\n\\n'")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != "make: nothing to be done" {
		t.Fatalf("output = %q", out)
	}
	// A command that printed only whitespace printed nothing.
	if _, out, _ := environment.Check(context.Background(), env, "echo"); out != "" {
		t.Fatalf("output = %q, want empty", out)
	}
}

// A check runs inside the environment it is handed, which for a workflow
// step is the directory the agent worked in and the variables it worked
// with — not dispatch's own process.
func TestCheckRunsInsideTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("built here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := localEnv(t, dir, map[string]string{"DISPATCH_CHECK": "yes"})
	code, out, err := environment.Check(context.Background(), env, "cat marker.txt; echo $DISPATCH_CHECK")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if code != 0 || out != "built here\nyes" {
		t.Fatalf("code=%d output=%q", code, out)
	}
}

// TestRunReadsBothStreamsBeforeItWaits: Run's error carries the tail of
// stderr, and for a long time nothing waited for the goroutine writing it —
// the failing path read that buffer while the copy was still filling it.
// The race detector is what fails this one; without it, it is a test that a
// failing command still says why.
func TestRunReadsBothStreamsBeforeItWaits(t *testing.T) {
	env := localEnv(t, t.TempDir(), nil)
	_, err := environment.Run(context.Background(), env, nil, "sh", "-c", "echo failing >&2; exit 2")
	if err == nil {
		t.Fatal("a non-zero exit came back as success")
	}
	if !strings.Contains(err.Error(), "exit 2") || !strings.Contains(err.Error(), "failing") {
		t.Errorf("err = %v, want the exit code and what stderr said", err)
	}
}

// A command that prints more than a pipe holds is drained before the exit
// is waited for, so none of it is lost: Wait closes the pipes, and whatever
// the child wrote that nobody had read yet would go with them. `check =
// "make test"` printing a failure log is the case that would notice.
func TestRunAndCheckKeepOutputBiggerThanAPipe(t *testing.T) {
	env := localEnv(t, t.TempDir(), nil)
	const lines = 20000 // ~230 KiB, several times a pipe buffer
	script := "for i in $(seq 1 " + strconv.Itoa(lines) + "); do echo line-$i-padding-padding-padding; done"

	out, err := environment.Run(context.Background(), env, nil, "sh", "-c", script)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Count(out, "\n") + 1; got != lines {
		t.Errorf("Run kept %d lines of %d", got, lines)
	}
	code, cout, err := environment.Check(context.Background(), env, script+"; exit 3")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if code != 3 {
		t.Errorf("code = %d, want 3", code)
	}
	if got := strings.Count(cout, "\n") + 1; got != lines {
		t.Errorf("Check kept %d lines of %d", got, lines)
	}
}
