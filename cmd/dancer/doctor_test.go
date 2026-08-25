package main

import (
	"strings"
	"testing"
)

// TestCheckGitHub: a host with no GitHub login is a note, not a failure —
// plenty of agents never touch GitHub — and a host with one says where
// what it lends comes from.
func TestCheckGitHub(t *testing.T) {
	t.Run("nothing to lend", func(t *testing.T) {
		isolateGitHub(t)
		c := checkGitHub()
		if !c.ok || !c.note {
			t.Fatalf("ok=%v note=%v, want a passing note", c.ok, c.note)
		}
		if !strings.Contains(c.info, "gh auth login") {
			t.Errorf("info = %q, want the fix in it", c.info)
		}
	})

	t.Run("token in the environment", func(t *testing.T) {
		isolateGitHub(t)
		t.Setenv("GH_TOKEN", "gho_doctor")
		c := checkGitHub()
		if !c.ok || c.note {
			t.Fatalf("ok=%v note=%v, want a plain pass", c.ok, c.note)
		}
		if !strings.Contains(c.info, "GH_TOKEN") {
			t.Errorf("info = %q, want the source named", c.info)
		}
		if strings.Contains(c.info, "gho_doctor") {
			t.Errorf("the token itself is in the output: %q", c.info)
		}
	})
}

// isolateGitHub hides every login this machine has: an empty gh config
// dir, no gh on PATH, no token in the environment.
func isolateGitHub(t *testing.T) {
	t.Helper()
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST"} {
		t.Setenv(k, "")
	}
}
