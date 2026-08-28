package gh

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/environment/docker"
)

// TestLiveDockerLend lends this host's GitHub login and identity into a
// container provisioned from ubuntu:24.04 and proves they took: `gh auth
// status` there authenticates against the real API, git commits as the
// host does, and git is pointed at gh for github.com so a push would use
// the same account. Run with DISPATCH_LIVE=1 and a docker daemon; the first
// run builds the image (~90s).
func TestLiveDockerLend(t *testing.T) {
	if os.Getenv("DISPATCH_LIVE") == "" {
		t.Skip("set DISPATCH_LIVE=1 to talk to the real GitHub")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if _, err := HostLogin(ctx); err != nil {
		t.Skip("no host GitHub login to lend")
	}

	spec := environment.Spec{
		Kind: environment.KindDocker, Image: "ubuntu:24.04", Workdir: t.TempDir(),
		Provision: &environment.Provision{Agents: []string{"claude"}},
	}
	env, err := docker.Factory{}.New(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer env.Stop(context.Background())

	Lend(ctx, env, spec.Env)

	out, err := environment.Run(ctx, env, nil, "sh", "-c", "gh auth status 2>&1")
	if err != nil {
		t.Fatalf("gh auth status: %v\n%s", err, out)
	}
	t.Logf("gh auth status:\n%s", out)
	if !strings.Contains(out, "Logged in to") {
		t.Errorf("the lent login did not authenticate:\n%s", out)
	}

	who, err := environment.Run(ctx, env, nil, "sh", "-c", "git config --global --get user.name; git config --global --get user.email")
	if err != nil {
		t.Fatalf("git identity: %v\n%s", err, who)
	}
	t.Logf("committing as:\n%s", who)
	id, err := HostIdentity(ctx)
	if err != nil {
		t.Fatalf("host identity: %v", err)
	}
	if !strings.Contains(who, id.Email) {
		t.Errorf("container commits as %q, want the host's %q", who, id.String())
	}

	helper, err := environment.Run(ctx, env, nil, "sh", "-c", "git config --get-regexp 'credential.*helper' 2>&1")
	if err != nil {
		t.Fatalf("git credential helper: %v\n%s", err, helper)
	}
	if !strings.Contains(helper, "gh auth git-credential") {
		t.Errorf("git was not pointed at gh:\n%s", helper)
	}

	// A second lend must be a no-op, not a rewrite: the container's copy
	// carries the host's mtime, so nothing there looks stale.
	before, _ := environment.Run(ctx, env, nil, "sh", "-c", "stat -c %Y \"$HOME/.config/gh/hosts.yml\"")
	time.Sleep(time.Second)
	Lend(ctx, env, spec.Env)
	after, _ := environment.Run(ctx, env, nil, "sh", "-c", "stat -c %Y \"$HOME/.config/gh/hosts.yml\"")
	if before != after {
		t.Errorf("second lend rewrote the file: %s -> %s", before, after)
	}
}
