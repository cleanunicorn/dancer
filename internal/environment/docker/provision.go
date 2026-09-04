package docker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dispatch/internal/environment"
)

// ProvisionedHome is $HOME inside an image dispatch provisioned. It is a real
// writable directory owned by the container user, so agent login, session
// history and settings (for example `~/.claude` and `~/.codex`) have somewhere
// to live.
const ProvisionedHome = "/home/dispatch"

// homeSkeleton is where provisioning stashes a copy of ProvisionedHome, so a
// persistent home volume mounted over it on a reused container can be seeded
// with whatever the image put there.
const homeSkeleton = "/opt/dispatch-home"

// provisionVersion changes whenever provisionScript does; it is part of the
// derived image tag, so an upgraded dispatch rebuilds instead of reusing an
// image built by the old script.
const provisionVersion = "4"

// agentInstall maps an agent kind to the command that installs its CLI.
var agentInstall = map[string]string{
	"claude":   "npm install -g @anthropic-ai/claude-code",
	"codex":    "npm install -g @openai/codex",
	"opencode": "npm install -g opencode-ai",
}

// agentBinary maps an agent kind to the binary it must put on PATH.
var agentBinary = map[string]string{
	"claude":   "claude",
	"codex":    "codex",
	"opencode": "opencode",
}

// build serialises provisioning per derived tag: two tasks starting at once
// on a cold image must not both build it.
var build struct {
	sync.Mutex
	locks map[string]*sync.Mutex
}

// ready caches "this base image already has everything asked for", so the
// probe container runs once per dispatch process, not once per task.
var ready sync.Map // string -> bool

func buildLock(tag string) *sync.Mutex {
	build.Lock()
	defer build.Unlock()
	if build.locks == nil {
		build.locks = map[string]*sync.Mutex{}
	}
	m, ok := build.locks[tag]
	if !ok {
		m = &sync.Mutex{}
		build.locks[tag] = m
	}
	return m
}

// imageTag is the name of the image provisioning produces. It hashes
// everything that changes the result, so a config change rebuilds and an
// unchanged config reuses.
func imageTag(base string, p environment.Provision, uid, gid int) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%s\n%s\n%d:%d\n", provisionVersion, base, uid, gid)
	agents := append([]string(nil), p.Agents...)
	sort.Strings(agents)
	fmt.Fprintf(h, "agents=%s\n", strings.Join(agents, ","))
	pkgs := append([]string(nil), p.Packages...)
	sort.Strings(pkgs)
	fmt.Fprintf(h, "packages=%s\n", strings.Join(pkgs, ","))
	// Setup order matters, so it is hashed as written.
	fmt.Fprintf(h, "setup=%s\n", strings.Join(p.Setup, "\n"))
	return fmt.Sprintf("dispatch-env:%x", h.Sum(nil)[:6])
}

// ensureImage returns the image to run and $HOME inside it. It is a no-op
// when nothing was asked for, or when the base image already satisfies the
// request; otherwise it builds (once) a derived image and returns that.
func (f Factory) ensureImage(ctx context.Context, spec environment.Spec, uid, gid int) (image, home string, err error) {
	if spec.Provision.Empty() {
		return spec.Image, "", nil
	}
	p := *spec.Provision
	bin := f.binary()
	tag := imageTag(spec.Image, p, uid, gid)

	lock := buildLock(tag)
	lock.Lock()
	defer lock.Unlock()

	if imageExists(ctx, bin, tag) {
		return tag, ProvisionedHome, nil
	}
	// A purpose-built image that already carries the agent CLI is left
	// exactly as it is — dispatch only derives an image when it has to.
	if len(p.Packages) == 0 && len(p.Setup) == 0 {
		if v, ok := ready.Load(readyKey(spec.Image, p.Agents)); ok {
			if v.(bool) {
				return spec.Image, "", nil
			}
		} else {
			ok := baseIsReady(ctx, bin, spec.Image, p.Agents)
			ready.Store(readyKey(spec.Image, p.Agents), ok)
			if ok {
				return spec.Image, "", nil
			}
		}
	}
	if err := buildImage(ctx, bin, spec.Image, tag, p, uid, gid); err != nil {
		return "", "", err
	}
	return tag, ProvisionedHome, nil
}

func readyKey(base string, agents []string) string {
	a := append([]string(nil), agents...)
	sort.Strings(a)
	return base + "\x00" + strings.Join(a, ",")
}

func imageExists(ctx context.Context, bin, ref string) bool {
	return exec.CommandContext(ctx, bin, "image", "inspect", ref).Run() == nil
}

// baseIsReady reports whether the image can already run the agents without
// any changes: every agent binary plus git on PATH, and a writable HOME.
func baseIsReady(ctx context.Context, bin, image string, agents []string) bool {
	probe := []string{"git"}
	for _, a := range agents {
		if b := agentBinary[strings.ToLower(a)]; b != "" {
			probe = append(probe, b)
		}
	}
	var sb strings.Builder
	for _, c := range probe {
		fmt.Fprintf(&sb, "command -v %s >/dev/null 2>&1 || exit 1\n", c)
	}
	sb.WriteString("exit 0\n")
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "run", "--rm", "--entrypoint", "sh", image, "-c", sb.String())
	return cmd.Run() == nil
}

// buildImage runs the provisioning script in a throwaway container started
// from base and commits the result as tag. It shells out to `docker` like
// the rest of this package: no build context, no Dockerfile on disk, and the
// user's docker context applies unchanged.
func buildImage(ctx context.Context, bin, base, tag string, p environment.Provision, uid, gid int) error {
	slog.Info("docker: provisioning image", "base", base, "tag", tag, "agents", p.Agents)
	started := time.Now()

	out, err := run(ctx, bin, nil, "run", "-d", "--user", "0:0", "--entrypoint", "sleep", base, "infinity")
	if err != nil {
		return fmt.Errorf("docker: provision: start %s: %w", base, err)
	}
	id := strings.TrimSpace(out)
	defer func() {
		_, _ = run(context.WithoutCancel(ctx), bin, nil, "rm", "-f", id)
	}()

	script := provisionScript(p, uid, gid)
	cmd := exec.CommandContext(ctx, bin, "exec", "-i", "-u", "0:0", id, "sh", "-s")
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker: provision %s: %w\n%s", base, err, tail(stderr.String(), 40))
	}

	if _, err := run(ctx, bin, nil,
		"commit",
		"--change", "ENV HOME="+ProvisionedHome,
		"--change", fmt.Sprintf("LABEL %s=1", labelProvisioned),
		"--change", fmt.Sprintf("LABEL %s=%s", labelHome, ProvisionedHome),
		id, tag,
	); err != nil {
		return fmt.Errorf("docker: provision: commit %s: %w", tag, err)
	}
	slog.Info("docker: image ready", "tag", tag, "took", time.Since(started).Round(time.Second))
	return nil
}

// provisionScript is the whole of dispatch's opinion about what an agent needs:
// a package manager it can find, git and curl, Node for the agent CLIs, the
// CLIs themselves, and a real user with a writable home matching the host
// uid/gid so files on the bind-mounted workdir stay owned by the human.
//
// It is deliberately one POSIX sh script with no build-time dependencies, so
// it runs unchanged on debian/ubuntu, alpine, fedora/rocky and arch.
func provisionScript(p environment.Provision, uid, gid int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `set -eu
export DEBIAN_FRONTEND=noninteractive
DISPATCH_UID=%d
DISPATCH_GID=%d
DISPATCH_HOME=%s

say() { echo "dispatch: $*" >&2; }

pm=""
for c in apt-get apk dnf yum pacman zypper; do
	if command -v "$c" >/dev/null 2>&1; then pm="$c"; break; fi
done
[ -n "$pm" ] || { say "no supported package manager in this image"; exit 1; }

pm_refresh() {
	case "$pm" in
		apt-get) apt-get update -qq ;;
		pacman)  pacman -Sy --noconfirm ;;
	esac
}

pm_install() {
	[ "$#" -gt 0 ] || return 0
	case "$pm" in
		apt-get) apt-get install -y -qq --no-install-recommends "$@" ;;
		apk)     apk add --no-cache "$@" ;;
		dnf)     dnf install -y -q "$@" ;;
		yum)     yum install -y -q "$@" ;;
		pacman)  pacman -S --noconfirm --needed "$@" ;;
		zypper)  zypper --non-interactive install "$@" ;;
	esac
}

pm_refresh
say "installing base tools"
pm_install ca-certificates curl git sudo
# Nice to have for agents that grep; not worth failing the build over.
pm_install ripgrep || say "ripgrep unavailable, skipping"
# tar is only the gh fallback's business, and gh_from_release checks for it
# itself, so an image whose package manager has no tar package still builds.
command -v tar >/dev/null 2>&1 || pm_install tar || say "tar unavailable, skipping"
`, uid, gid, ProvisionedHome)

	// The GitHub CLI is part of the base kit rather than something to ask
	// for: an agent working on a repo opens pull requests, reads issues and
	// pushes branches, and dispatch hands the container the host's GitHub
	// login at run time so it can (internal/gh). Distros package it under
	// two names and not in every release, so the official release tarball
	// is the fallback — a static Go binary, which is why it also works on
	// musl. A container without it is worse, not broken, so a failure here
	// only warns.
	b.WriteString(`
gh_from_release() {
	command -v tar >/dev/null 2>&1 || return 1
	case "$(uname -m)" in
		x86_64|amd64)  gh_arch=amd64 ;;
		aarch64|arm64) gh_arch=arm64 ;;
		*) return 1 ;;
	esac
	gh_ver=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/cli/cli/releases/latest 2>/dev/null | sed -n 's#.*/tag/v##p')
	[ -n "$gh_ver" ] || return 1
	gh_dir="gh_${gh_ver}_linux_${gh_arch}"
	gh_base="https://github.com/cli/cli/releases/download/v${gh_ver}"
	curl -fsSL "${gh_base}/${gh_dir}.tar.gz" -o /tmp/gh.tgz || return 1
	# The release publishes its own checksums; a binary that goes on PATH
	# with the operator's GitHub token beside it is worth checking when the
	# image gives us something to check with.
	if command -v sha256sum >/dev/null 2>&1 &&
		curl -fsSL "${gh_base}/gh_${gh_ver}_checksums.txt" -o /tmp/gh.sums; then
		gh_want=$(grep " ${gh_dir}.tar.gz$" /tmp/gh.sums | cut -d" " -f1 || true)
		gh_got=$(sha256sum /tmp/gh.tgz | cut -d" " -f1)
		if [ -z "$gh_want" ] || [ "$gh_want" != "$gh_got" ]; then
			say "github cli tarball failed its checksum, not installing"
			rm -f /tmp/gh.tgz /tmp/gh.sums
			return 1
		fi
	else
		say "cannot verify the github cli tarball checksum in this image"
	fi
	tar -xzf /tmp/gh.tgz -C /tmp || return 1
	cp "/tmp/${gh_dir}/bin/gh" /usr/local/bin/gh || return 1
	chmod 0755 /usr/local/bin/gh
	rm -rf /tmp/gh.tgz /tmp/gh.sums "/tmp/${gh_dir}"
	command -v gh >/dev/null 2>&1
}

if command -v gh >/dev/null 2>&1; then
	say "github cli already present"
else
	say "installing github cli"
	case "$pm" in
		apk|pacman) pm_install github-cli >/dev/null 2>&1 || true ;;
		*)          pm_install gh >/dev/null 2>&1 || true ;;
	esac
	command -v gh >/dev/null 2>&1 || gh_from_release || say "github cli unavailable, skipping"
fi
`)

	if len(p.Agents) > 0 {
		b.WriteString(`
node_ok() {
	command -v node >/dev/null 2>&1 || return 1
	major=$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)
	[ "$major" -ge 18 ]
}

if ! node_ok; then
	say "installing node"
	case "$pm" in
		apt-get) curl -fsSL https://deb.nodesource.com/setup_22.x | bash - >/dev/null 2>&1 && apt-get install -y -qq nodejs || pm_install nodejs npm ;;
		apk)     pm_install nodejs npm ;;
		*)       pm_install nodejs npm ;;
	esac
fi
node_ok || { say "no node >= 18 available; use a base image with node, or add it to packages"; exit 1; }
command -v npm >/dev/null 2>&1 || pm_install npm
`)
		for _, a := range p.Agents {
			cmd, ok := agentInstall[strings.ToLower(strings.TrimSpace(a))]
			if !ok {
				continue
			}
			bin := agentBinary[strings.ToLower(strings.TrimSpace(a))]
			fmt.Fprintf(&b, "\nif command -v %s >/dev/null 2>&1; then say \"%s already present\"; else say \"installing %s\"; %s; fi\n", bin, bin, bin, cmd)
		}
	}

	if len(p.Packages) > 0 {
		fmt.Fprintf(&b, "\nsay \"installing extra packages\"\npm_install %s\n", shellArgs(p.Packages))
	}

	fmt.Fprintf(&b, `
say "creating user %d:%d with home $DISPATCH_HOME"
if getent passwd "$DISPATCH_UID" >/dev/null 2>&1; then
	existing=$(getent passwd "$DISPATCH_UID" | cut -d: -f1)
	usermod -d "$DISPATCH_HOME" "$existing" >/dev/null 2>&1 || true
else
	(groupadd -g "$DISPATCH_GID" dispatch || addgroup -g "$DISPATCH_GID" dispatch) >/dev/null 2>&1 || true
	(useradd -u "$DISPATCH_UID" -g "$DISPATCH_GID" -d "$DISPATCH_HOME" -s /bin/sh -M dispatch \
		|| adduser -D -u "$DISPATCH_UID" -G dispatch -h "$DISPATCH_HOME" -s /bin/sh dispatch) >/dev/null 2>&1 || true
fi
mkdir -p "$DISPATCH_HOME"
chown -R "$DISPATCH_UID:$DISPATCH_GID" "$DISPATCH_HOME"

# The agent runs as this user, not root, so the workdir it shares with the
# host stays owned by the human. Without sudo it could not install anything
# it turns out to need mid-task, which is most of the point of a container
# it gets to keep. What it may actually run is still gated by the agent's
# permission mode.
if command -v sudo >/dev/null 2>&1; then
	mkdir -p /etc/sudoers.d
	printf '#%%s ALL=(ALL) NOPASSWD:ALL\n' "$DISPATCH_UID" > /etc/sudoers.d/dispatch
	chmod 0440 /etc/sudoers.d/dispatch
else
	say "no sudo in this image; the agent cannot install packages itself"
fi

# The workdir is bind-mounted from the host and owned by the host user; git
# refuses to touch a repo it thinks belongs to someone else.
git config --system --add safe.directory '*' >/dev/null 2>&1 || true

# Keep a copy of the home the image ends up with: a reused container mounts a
# persistent volume over $HOME and seeds it from here on first start.
mkdir -p %s
cp -a "$DISPATCH_HOME/." %s/ 2>/dev/null || true
chown -R "$DISPATCH_UID:$DISPATCH_GID" %s
`, uid, gid, homeSkeleton, homeSkeleton, homeSkeleton)

	for _, c := range p.Setup {
		if strings.TrimSpace(c) == "" {
			continue
		}
		fmt.Fprintf(&b, "\nsay %s\n%s\n", shellQuote("setup: "+c), c)
	}
	b.WriteString("\nsay \"done\"\n")
	return b.String()
}

func shellArgs(xs []string) string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, shellQuote(x))
	}
	return strings.Join(out, " ")
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
