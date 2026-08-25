package docker

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/environment"
)

func TestImageTagIsStableAndSpecific(t *testing.T) {
	base := environment.Provision{Agents: []string{"claude"}, Packages: []string{"jq", "make"}}
	want := imageTag("ubuntu:24.04", base, 1000, 1000)

	if got := imageTag("ubuntu:24.04", base, 1000, 1000); got != want {
		t.Fatalf("same input gave %s and %s", want, got)
	}
	// Order of the sets must not matter: config is a set, not a sequence.
	reordered := environment.Provision{Agents: []string{"claude"}, Packages: []string{"make", "jq"}}
	if got := imageTag("ubuntu:24.04", reordered, 1000, 1000); got != want {
		t.Fatalf("reordered packages changed the tag: %s != %s", got, want)
	}

	for name, other := range map[string]struct {
		image    string
		p        environment.Provision
		uid, gid int
	}{
		"different image":    {"debian:12", base, 1000, 1000},
		"different uid":      {"ubuntu:24.04", base, 1001, 1000},
		"different gid":      {"ubuntu:24.04", base, 1000, 1001},
		"different agent":    {"ubuntu:24.04", environment.Provision{Agents: []string{"codex"}, Packages: base.Packages}, 1000, 1000},
		"different packages": {"ubuntu:24.04", environment.Provision{Agents: base.Agents, Packages: []string{"jq"}}, 1000, 1000},
		"added setup":        {"ubuntu:24.04", environment.Provision{Agents: base.Agents, Packages: base.Packages, Setup: []string{"echo hi"}}, 1000, 1000},
	} {
		if got := imageTag(other.image, other.p, other.uid, other.gid); got == want {
			t.Errorf("%s reused the tag %s", name, got)
		}
	}

	if !strings.HasPrefix(want, "dispatch-env:") {
		t.Fatalf("tag %q is not in dispatch's namespace", want)
	}
}

// TestSetupOrderMatters guards the one part of the spec that is a sequence:
// two setup commands in the other order are a different image.
func TestSetupOrderMatters(t *testing.T) {
	a := environment.Provision{Setup: []string{"echo one", "echo two"}}
	b := environment.Provision{Setup: []string{"echo two", "echo one"}}
	if imageTag("x", a, 1, 1) == imageTag("x", b, 1, 1) {
		t.Fatal("setup order did not change the tag")
	}
}

func TestProvisionScriptIsValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for name, p := range map[string]environment.Provision{
		"agents only": {Agents: []string{"claude"}},
		"both agents": {Agents: []string{"claude", "codex"}},
		"all agents":  {Agents: []string{"claude", "codex", "opencode"}},
		"everything":  {Agents: []string{"claude"}, Packages: []string{"jq", "a-b_c"}, Setup: []string{"echo 'hi there'", "true"}},
		"nothing":     {},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(provisionScript(p, 1000, 1000))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("script is not valid sh: %v\n%s", err, out)
			}
		})
	}
}

func TestProvisionScriptContents(t *testing.T) {
	s := provisionScript(environment.Provision{
		Agents:   []string{"claude"},
		Packages: []string{"postgresql-client"},
		Setup:    []string{"pip install ruff"},
	}, 1000, 1001)

	for _, want := range []string{
		"DISPATCH_UID=1000",
		"DISPATCH_GID=1001",
		"DISPATCH_HOME=" + ProvisionedHome,
		"npm install -g @anthropic-ai/claude-code",
		"'postgresql-client'",
		"pip install ruff",
		"safe.directory",
		"/etc/sudoers.d/dispatch",
		homeSkeleton,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script is missing %q", want)
		}
	}
	// An agent that is already installed must not be reinstalled: the whole
	// point of provisioning a purpose-built image is that it is a no-op.
	if !strings.Contains(s, "if command -v claude >/dev/null 2>&1;") {
		t.Error("claude install is not guarded by a presence check")
	}
	if strings.Contains(s, "@openai/codex") || strings.Contains(s, "opencode-ai") {
		t.Error("installed an agent without being asked")
	}
	// Every kind dispatch knows has an install line, guarded the same way.
	for _, a := range []string{"claude", "codex", "opencode"} {
		s := provisionScript(environment.Provision{Agents: []string{a}}, 1000, 1000)
		if !strings.Contains(s, "if command -v "+agentBinary[a]+" >/dev/null 2>&1;") || !strings.Contains(s, agentInstall[a]) {
			t.Errorf("%s: install line missing or unguarded", a)
		}
	}
	// The sudoers rule must name the uid, not a user name: on an image that
	// already had a user at that uid we keep its name, whatever it is.
	if !strings.Contains(s, `printf '#%s ALL=(ALL) NOPASSWD:ALL\n' "$DISPATCH_UID"`) {
		t.Errorf("sudoers rule is not keyed to the uid:\n%s", s)
	}
}

// TestProvisionScriptInstallsGitHubCLI: `gh` is part of every provisioned
// image, whatever was asked for, because dispatch lends it a login at run
// time (internal/gh) and an agent working on a repo needs it.
func TestProvisionScriptInstallsGitHubCLI(t *testing.T) {
	for name, p := range map[string]environment.Provision{
		"agents only": {Agents: []string{"claude"}},
		"nothing":     {},
	} {
		t.Run(name, func(t *testing.T) {
			s := provisionScript(p, 1000, 1000)
			for _, want := range []string{
				`if command -v gh >/dev/null 2>&1; then`,
				"gh_from_release()",
				"github-cli",
				"https://github.com/cli/cli/releases/latest",
			} {
				if !strings.Contains(s, want) {
					t.Errorf("script is missing %q", want)
				}
			}
			// A missing gh is a worse container, not a failed build, and
			// neither is a package manager with no tar to offer it.
			if !strings.Contains(s, `|| say "github cli unavailable, skipping"`) {
				t.Error("a failed gh install would fail the whole build")
			}
			if !strings.Contains(s, `pm_install tar || say "tar unavailable, skipping"`) {
				t.Error("a missing tar package would fail the whole build")
			}
			// The tarball goes on PATH next to the operator's GitHub token;
			// it is checked against the release's own checksums.
			for _, want := range []string{"_checksums.txt", "sha256sum", `"$gh_want" != "$gh_got"`} {
				if !strings.Contains(s, want) {
					t.Errorf("the release tarball is not verified: missing %q", want)
				}
			}
		})
	}
}

// TestProvisionScriptQuotesPackages keeps a package name from turning into a
// shell command.
func TestProvisionScriptQuotesPackages(t *testing.T) {
	s := provisionScript(environment.Provision{Packages: []string{"jq; touch /pwned"}}, 1, 1)
	if !strings.Contains(s, `'jq; touch /pwned'`) {
		t.Fatalf("package was not quoted:\n%s", s)
	}
}

func TestEmptyProvision(t *testing.T) {
	var nilp *environment.Provision
	if !nilp.Empty() {
		t.Error("nil provision should be empty")
	}
	if !(&environment.Provision{}).Empty() {
		t.Error("zero provision should be empty")
	}
	if (&environment.Provision{Agents: []string{"claude"}}).Empty() {
		t.Error("provision with an agent is not empty")
	}
}
