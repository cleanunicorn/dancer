package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckGitHub: a host missing either half of what dancer lends is a
// note, not a failure — plenty of agents never touch GitHub — and a host
// with both says where each came from.
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
		if !strings.Contains(c.info, "no git identity") {
			t.Errorf("info = %q, want the missing identity named", c.info)
		}
	})

	t.Run("token and identity in the environment", func(t *testing.T) {
		isolateGitHub(t)
		t.Setenv("GH_TOKEN", "gho_doctor")
		t.Setenv("GIT_AUTHOR_NAME", "Ada Lovelace")
		t.Setenv("GIT_AUTHOR_EMAIL", "ada@example.com")
		c := checkGitHub()
		if !c.ok || c.note {
			t.Fatalf("ok=%v note=%v, want a plain pass", c.ok, c.note)
		}
		if !strings.Contains(c.info, "GH_TOKEN") {
			t.Errorf("info = %q, want the source named", c.info)
		}
		if !strings.Contains(c.info, "Ada Lovelace <ada@example.com>") {
			t.Errorf("info = %q, want the committer named", c.info)
		}
		if strings.Contains(c.info, "gho_doctor") {
			t.Errorf("the token itself is in the output: %q", c.info)
		}
	})

	// A login with nobody to commit as is worth saying out loud: the agent
	// gets as far as `git commit` and stops there.
	t.Run("login without an identity", func(t *testing.T) {
		isolateGitHub(t)
		t.Setenv("GH_TOKEN", "gho_doctor")
		c := checkGitHub()
		if !c.ok || !c.note {
			t.Fatalf("ok=%v note=%v, want a passing note", c.ok, c.note)
		}
	})
}

// isolateGitHub hides everything this machine could lend: an empty gh
// config dir, no gh or git on PATH, no token and no identity in the
// environment, and a git config of its own.
func isolateGitHub(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", dir)
	t.Setenv("PATH", dir)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(dir, "gitconfig-system"))
	for _, k := range []string{
		"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST",
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		t.Setenv(k, "")
	}
}
