package gh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this host")
	}
}

func TestLendIdentityGivesTheHostsCommitter(t *testing.T) {
	requireGit(t)
	dir := hostConfig(t)
	hostIdentity(t, "Ada Lovelace", "ada@example.com")
	writeHosts(t, dir, hostsWithToken, time.Now())
	env, home, _ := newShEnv(t, "docker", nil)

	Lend(context.Background(), env, nil)

	got := containerIdentity(t, home)
	if !strings.Contains(got, "ada@example.com") || !strings.Contains(got, "Ada Lovelace") {
		t.Fatalf("container git config = %q, want the host's identity", got)
	}
}

// An identity already in the container was chosen by the operator or the
// agent; dancer does not get to overrule it.
func TestLendIdentityKeepsTheOneAlreadyThere(t *testing.T) {
	requireGit(t)
	hostConfig(t)
	hostIdentity(t, "Ada Lovelace", "ada@example.com")
	env, home, _ := newShEnv(t, "docker", nil)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[user]\n\tname = Bot\n\temail = bot@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	Lend(context.Background(), env, nil)

	if got := containerIdentity(t, home); !strings.Contains(got, "bot@example.com") || strings.Contains(got, "ada@") {
		t.Fatalf("container git config = %q, want it untouched", got)
	}
}

// A definition's setup commands run as root, so an identity they left
// behind lives in the system config rather than the agent user's. git can
// answer with it, so dancer leaves it be.
func TestLendIdentityKeepsASystemWideOne(t *testing.T) {
	requireGit(t)
	hostConfig(t)
	hostIdentity(t, "Ada Lovelace", "ada@example.com")
	env, home, _ := newShEnv(t, "docker", nil)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig-system"),
		[]byte("[user]\n\tname = Bot\n\temail = bot@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	Lend(context.Background(), env, nil)

	if got := containerIdentity(t, home); got != "" {
		t.Fatalf("container global git config = %q, want the system one left to answer", got)
	}
}

// A definition that names its own committer is answered with silence, the
// same way its own GH_TOKEN stops the login being lent.
func TestLendIdentitySkipsWhenTheDefinitionHasOne(t *testing.T) {
	requireGit(t)
	hostConfig(t)
	hostIdentity(t, "Ada Lovelace", "ada@example.com")
	env, home, _ := newShEnv(t, "docker", nil)

	Lend(context.Background(), env, map[string]string{"GIT_AUTHOR_EMAIL": "bot@example.com"})

	if got := containerIdentity(t, home); got != "" {
		t.Fatalf("container git config = %q, want none written", got)
	}
}

// The login can be the definition's own while the identity is still the
// host's: a container that cannot say who committed is stuck either way.
func TestLendIdentityWithTheDefinitionsOwnToken(t *testing.T) {
	requireGit(t)
	dir := hostConfig(t)
	hostIdentity(t, "Ada Lovelace", "ada@example.com")
	writeHosts(t, dir, hostsWithToken, time.Now())
	env, home, _ := newShEnv(t, "docker", nil)

	Lend(context.Background(), env, map[string]string{"GH_TOKEN": "gho_own"})

	if _, err := os.Stat(filepath.Join(home, ".config", "gh", "hosts.yml")); err == nil {
		t.Error("lent a login over the definition's own token")
	}
	if got := containerIdentity(t, home); !strings.Contains(got, "ada@example.com") {
		t.Errorf("container git config = %q, want the host's identity", got)
	}
}

func TestHostIdentitySources(t *testing.T) {
	requireGit(t)

	t.Run("git config", func(t *testing.T) {
		hostConfig(t)
		hostIdentity(t, "Ada Lovelace", "ada@example.com")
		id, err := HostIdentity(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if id.String() != "Ada Lovelace <ada@example.com>" {
			t.Fatalf("identity = %q", id.String())
		}
	})

	t.Run("environment", func(t *testing.T) {
		hostConfig(t)
		t.Setenv("GIT_AUTHOR_NAME", "Grace Hopper")
		t.Setenv("GIT_AUTHOR_EMAIL", "grace@example.com")
		id, err := HostIdentity(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if id.String() != "Grace Hopper <grace@example.com>" || id.Source != "GIT_AUTHOR_EMAIL" {
			t.Fatalf("identity = %q from %q", id.String(), id.Source)
		}
	})

	// A name without an email is not something git can commit with.
	t.Run("name only", func(t *testing.T) {
		hostConfig(t)
		hostIdentity(t, "Ada Lovelace", "")
		if id, err := HostIdentity(context.Background()); err == nil {
			t.Fatalf("identity = %q, want an error", id.String())
		}
	})

	t.Run("nothing", func(t *testing.T) {
		hostConfig(t)
		if id, err := HostIdentity(context.Background()); err == nil {
			t.Fatalf("identity = %q, want an error", id.String())
		}
	})
}
