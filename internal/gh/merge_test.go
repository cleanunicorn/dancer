package gh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI puts a `gh` on PATH that writes its arguments to a file and
// exits with code, so a test can see exactly what dispatch asked GitHub
// for without asking GitHub for it.
func fakeCLI(t *testing.T, code int, say string) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"echo " + say + "\nexit " + itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return argsFile
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

func TestParseMethod(t *testing.T) {
	ok := map[string]Method{
		"":        MethodSquash,
		"  ":      MethodSquash,
		"it":      MethodSquash,
		"IT":      MethodSquash,
		"squash":  MethodSquash,
		"Squash":  MethodSquash,
		"merge":   MethodMerge,
		"rebase":  MethodRebase,
		" rebase": MethodRebase,
	}
	for in, want := range ok {
		if got, ok := ParseMethod(in); !ok || got != want {
			t.Errorf("ParseMethod(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	// Anything else is not a merge method — it is a sentence, and the
	// chat surface hands it to the agent instead.
	for _, in := range []string{"this behind a flag", "fast-forward", "yes please"} {
		if _, ok := ParseMethod(in); ok {
			t.Errorf("ParseMethod(%q) accepted", in)
		}
	}
}

// TestMergeAsksForTheMethodAndTheBranch pins the command line, which is
// the whole of what dispatch promises GitHub.
func TestMergeAsksForTheMethodAndTheBranch(t *testing.T) {
	argsFile := fakeCLI(t, 0, "'Merged pull request #51'")
	out, err := Merge(context.Background(), "https://github.com/o/r/pull/51", MethodSquash)
	if err != nil {
		t.Fatalf("Merge: %v (%s)", err, out)
	}
	if !strings.Contains(out, "Merged pull request #51") {
		t.Errorf("output = %q, want gh's own words", out)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pr", "merge", "https://github.com/o/r/pull/51", "--squash", "--delete-branch"}
	got := strings.Fields(string(args))
	if len(got) != len(want) {
		t.Fatalf("gh args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("gh args = %v, want %v", got, want)
		}
	}
}

func TestMergeDefaultsToSquash(t *testing.T) {
	argsFile := fakeCLI(t, 0, "merged")
	if _, err := Merge(context.Background(), "https://github.com/o/r/pull/51", ""); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--squash") {
		t.Errorf("gh args = %q, want --squash", args)
	}
}

// TestMergeFailsLoudly: gh's exit code is the whole contract — a `ship`
// closes the thread on it, so a failure has to come back as an error with
// gh's own reason attached.
func TestMergeFailsLoudly(t *testing.T) {
	fakeCLI(t, 1, "'Pull request #51 is not mergeable: the base branch policy prohibits the merge'")
	out, err := Merge(context.Background(), "https://github.com/o/r/pull/51", MethodMerge)
	if err == nil {
		t.Fatal("Merge succeeded on a gh that exited 1")
	}
	if !strings.Contains(out, "not mergeable") {
		t.Errorf("output = %q, want gh's reason", out)
	}
}

func TestMergeWithoutTheCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Merge(context.Background(), "https://github.com/o/r/pull/51", MethodSquash); err == nil {
		t.Fatal("Merge succeeded without a gh on PATH")
	}
}

func TestMergeWithoutAPullRequest(t *testing.T) {
	fakeCLI(t, 0, "merged")
	if _, err := Merge(context.Background(), "  ", MethodSquash); err == nil {
		t.Fatal("Merge succeeded without a pull request")
	}
}
