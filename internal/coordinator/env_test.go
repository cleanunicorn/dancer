package coordinator

import (
	"path/filepath"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/environment"
)

// TestResolveEnvScopesWorkdirToTheReuseKey: a reused container has its bind
// mount fixed at creation, so every task sharing it must resolve to the same
// working directory. A per-task environment keeps a directory per task.
func TestResolveEnvScopesWorkdir(t *testing.T) {
	c := &Coordinator{WorkdirRoot: "/srv/work"}
	docker := environment.Spec{Kind: environment.KindDocker, Image: "ubuntu:24.04"}

	perTask := docker
	perTask.Reuse = environment.ReuseTask
	a := c.resolveEnv(perTask, "coder", "C1/17.5", "task-a")
	b := c.resolveEnv(perTask, "coder", "C1/17.5", "task-b")
	if a.Workdir == b.Workdir {
		t.Errorf("two tasks shared %s without asking to", a.Workdir)
	}
	if a.ReuseKey != "" {
		t.Errorf("per-task environment got reuse key %q", a.ReuseKey)
	}
	if a.Workdir != filepath.Join("/srv/work", "task-a") {
		t.Errorf("workdir = %q", a.Workdir)
	}

	perThread := docker
	perThread.Reuse = environment.ReuseThread
	c1 := c.resolveEnv(perThread, "coder", "C1/17.5", "task-a")
	c2 := c.resolveEnv(perThread, "coder", "C1/17.5", "task-b")
	if c1.ReuseKey != "C1/17.5" {
		t.Errorf("reuse key = %q, want the thread", c1.ReuseKey)
	}
	if c1.Workdir != c2.Workdir {
		t.Errorf("same thread got different workdirs: %s vs %s", c1.Workdir, c2.Workdir)
	}
	other := c.resolveEnv(perThread, "coder", "C1/99.9", "task-c")
	if other.Workdir == c1.Workdir {
		t.Error("two threads landed in the same workdir")
	}

	perDef := docker
	perDef.Reuse = environment.ReuseDefinition
	d1 := c.resolveEnv(perDef, "coder", "C1/17.5", "task-a")
	d2 := c.resolveEnv(perDef, "coder", "C2/42.0", "task-b")
	if d1.ReuseKey != "coder" || d2.ReuseKey != "coder" {
		t.Errorf("reuse key = %q/%q, want the agent name", d1.ReuseKey, d2.ReuseKey)
	}
	if d1.Workdir != d2.Workdir {
		t.Error("one definition-scoped environment used two workdirs")
	}
}

// A definition with a workdir of its own keeps it, whatever the scope.
func TestResolveEnvKeepsExplicitWorkdir(t *testing.T) {
	c := &Coordinator{WorkdirRoot: "/srv/work"}
	spec := environment.Spec{Kind: environment.KindLocal, Workdir: "/home/me/app"}
	if got := c.resolveEnv(spec, "coder", "C1/17.5", "task-a"); got.Workdir != "/home/me/app" {
		t.Fatalf("workdir = %q", got.Workdir)
	}
}

func TestDirNameIsPathSafe(t *testing.T) {
	cases := map[string]string{
		"C0123ABC/1700000000.123456": "C0123ABC-1700000000.123456",
		"coder":                      "coder",
		"../../etc":                  "..-..-etc",
		"/":                          "shared",
	}
	for in, want := range cases {
		got := dirName(in)
		if got != want {
			t.Errorf("dirName(%q) = %q, want %q", in, got, want)
		}
		if filepath.Base(got) != got {
			t.Errorf("dirName(%q) = %q escapes its parent", in, got)
		}
	}
}
