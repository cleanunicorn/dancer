package docker

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/cleanunicorn/dancer/internal/environment"
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

	if !strings.HasPrefix(want, "dancer-env:") {
		t.Fatalf("tag %q is not in dancer's namespace", want)
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
		"DANCER_UID=1000",
		"DANCER_GID=1001",
		"DANCER_HOME=" + ProvisionedHome,
		"npm install -g @anthropic-ai/claude-code",
		"'postgresql-client'",
		"pip install ruff",
		"safe.directory",
		"/etc/sudoers.d/dancer",
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
	if strings.Contains(s, "@openai/codex") {
		t.Error("installed codex without being asked")
	}
	// The sudoers rule must name the uid, not a user name: on an image that
	// already had a user at that uid we keep its name, whatever it is.
	if !strings.Contains(s, `printf '#%s ALL=(ALL) NOPASSWD:ALL\n' "$DANCER_UID"`) {
		t.Errorf("sudoers rule is not keyed to the uid:\n%s", s)
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
