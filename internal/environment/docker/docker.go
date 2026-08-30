// Package docker runs agents inside a container started from Spec.Image.
// It shells out to the `docker` CLI so the user's docker context, auth and
// rootless setup apply unchanged.
//
// Spec.Workdir on the host is bind-mounted at /work inside the container;
// every Exec runs in /work with Spec.Env exported.
//
// Two things make a plain base image usable:
//
//   - Provisioning (Spec.Provision). Given `ubuntu:24.04`, dispatch installs
//     git, curl, the GitHub CLI, Node and the agent CLIs, creates a user
//     matching the host uid/gid with a writable $HOME, and commits the
//     result as a derived image tagged by a hash of the request. The build
//     happens once; every later task starts from the cached tag. An image
//     that already carries the agent CLI is left untouched.
//
//   - Reuse (Spec.Reuse / Spec.ReuseKey). A container can outlive its task
//     and be shared by every task with the same key — one per thread, say.
//     Its $HOME is a named volume, so `~/.claude` (login, session history)
//     survives container restarts and `claude --resume` keeps working.
package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dispatch/internal/environment"
)

// ContainerWorkdir is the mount point of Spec.Workdir inside the container.
const ContainerWorkdir = "/work"

// Labels dispatch puts on everything it creates, so containers and volumes it
// owns can be found again after a restart.
const (
	labelManaged     = "dispatch.managed"
	labelProvisioned = "dispatch.provisioned"
	labelHome        = "dispatch.home"
	labelKey         = "dispatch.key"
)

// DefaultReuseTTL is how long a reused container may sit unused before Reap
// removes it.
const DefaultReuseTTL = 24 * time.Hour

// Factory builds docker environments.
type Factory struct {
	// Binary is the docker CLI (default "docker").
	Binary string
	// ExtraRunArgs are appended to `docker run` (e.g. --network, --memory).
	ExtraRunArgs []string
	// User is the `--user` for the container. Empty = current uid:gid, so
	// files written to the mounted workdir stay owned by the host user.
	// "root" runs as root.
	User string
	// StateDir is where the factory records when a reused container was
	// last used, so Reap can retire idle ones across restarts. Empty
	// disables reaping.
	StateDir string
}

// Env is one container.
type Env struct {
	bin   string
	spec  environment.Spec
	extra []string
	user  string
	uid   int
	gid   int
	state string

	// key is the reuse scope's identity ("" = throwaway).
	key string
	// name is the container name for a reused environment ("" = throwaway).
	// It is only known after Start has resolved the image, because a
	// rebuilt image has to mean a new container rather than an adopted one
	// still running the old build.
	name string
	// volume is the persistent $HOME volume for a reused environment.
	volume string
	// home is $HOME inside the image ("" = unknown, HOME=/tmp is injected).
	home string
	// derived is true when image was built by dispatch, which means its
	// entrypoint is the base image's and must be overridden.
	derived bool
	image   string

	mu sync.Mutex
	id string
}

// inUse counts the live Envs per reused container name, so Reap never
// removes a container a running task is talking to.
var inUse struct {
	sync.Mutex
	n map[string]int
}

func hold(name string) {
	if name == "" {
		return
	}
	inUse.Lock()
	defer inUse.Unlock()
	if inUse.n == nil {
		inUse.n = map[string]int{}
	}
	inUse.n[name]++
}

func release(name string) {
	if name == "" {
		return
	}
	inUse.Lock()
	defer inUse.Unlock()
	if inUse.n[name] > 1 {
		inUse.n[name]--
	} else {
		delete(inUse.n, name)
	}
}

func busy(name string) bool {
	inUse.Lock()
	defer inUse.Unlock()
	return inUse.n[name] > 0
}

func (f Factory) binary() string {
	if f.Binary == "" {
		return "docker"
	}
	return f.Binary
}

func (f Factory) New(spec environment.Spec) (environment.Environment, error) {
	if spec.Image == "" {
		return nil, fmt.Errorf("docker: image is required")
	}
	if spec.Workdir == "" {
		return nil, fmt.Errorf("docker: workdir is required")
	}
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return nil, err
	}
	spec.Workdir = abs
	// Docker would create a missing bind-mount source as root; make it
	// here instead so it belongs to the user dispatch runs as.
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("docker: workdir: %w", err)
	}
	user := f.User
	uid, gid := os.Getuid(), os.Getgid()
	if user == "" {
		user = fmt.Sprintf("%d:%d", uid, gid)
	}
	if user == "root" {
		user, uid, gid = "", 0, 0
	}
	e := &Env{
		bin:   f.binary(),
		spec:  spec,
		extra: f.ExtraRunArgs,
		user:  user,
		uid:   uid,
		gid:   gid,
		state: f.StateDir,
		image: spec.Image,
	}
	if key := reuseKey(spec); key != "" {
		e.key = key
		// The home volume deliberately leaves the image out of its name:
		// upgrading the image must not throw away the agent's login.
		e.volume = "dispatch-home-" + slug(key) + "-" + hash(key)
	}
	return e, nil
}

// reuseKey returns the identity a container is shared under, or "" when the
// environment is a throwaway.
func reuseKey(spec environment.Spec) string {
	switch spec.Reuse {
	case environment.ReuseThread, environment.ReuseDefinition:
		if spec.ReuseKey != "" {
			return spec.ReuseKey
		}
	}
	return ""
}

func (e *Env) Kind() environment.Kind { return environment.KindDocker }

func (e *Env) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id != "" {
		return nil
	}
	f := Factory{Binary: e.bin}
	image, home, err := f.ensureImage(ctx, e.spec, e.uid, e.gid)
	if err != nil {
		return err
	}
	e.derived = image != e.spec.Image
	e.image = image
	if home == "" {
		home = imageHome(ctx, e.bin, image)
	}
	e.home = home

	e.name = e.nameFor(image)

	if e.name != "" {
		id, err := e.adopt(ctx)
		if err != nil {
			return err
		}
		if id != "" {
			e.id = id
			hold(e.name)
			e.touch()
			return nil
		}
		if err := e.ensureVolume(ctx); err != nil {
			return err
		}
	}

	args := []string{"run", "-d", "-w", ContainerWorkdir, "-v", e.spec.Workdir + ":" + ContainerWorkdir}
	if e.name != "" {
		args = append(args, "--name", e.name, "--label", labelKey+"="+e.spec.ReuseKey)
		if e.home != "" && e.volume != "" {
			args = append(args, "-v", e.volume+":"+e.home)
		}
	} else {
		args = append(args, "--rm")
	}
	args = append(args, "--label", labelManaged+"=1")
	if e.derived {
		args = append(args, "--label", labelProvisioned+"=1")
	}
	if e.user != "" {
		args = append(args, "--user", e.user)
	}
	if _, ok := e.spec.Env["HOME"]; !ok {
		// Non-root users usually have no home in an unprovisioned image;
		// claude needs a writable one for ~/.claude.json.
		h := e.home
		if h == "" {
			h = "/tmp"
		}
		args = append(args, "-e", "HOME="+h)
	}
	for _, k := range sortedKeys(e.spec.Env) {
		args = append(args, "-e", k+"="+e.spec.Env[k])
	}
	args = append(args, e.extra...)
	if e.derived {
		// The derived image inherited the base image's entrypoint, which
		// would swallow the keep-alive command.
		args = append(args, "--entrypoint", "sleep", e.image, "infinity")
	} else {
		args = append(args, e.image, "sleep", "infinity")
	}
	out, err := run(ctx, e.bin, nil, args...)
	if err != nil {
		if e.name == "" || !strings.Contains(err.Error(), "already in use") {
			return err
		}
		// Another task created the shared container between adopt and run.
		id, aerr := e.adopt(ctx)
		if aerr != nil || id == "" {
			return err
		}
		e.id = id
		hold(e.name)
		e.touch()
		return nil
	}
	e.id = strings.TrimSpace(out)
	hold(e.name)
	e.touch()
	return nil
}

// nameFor is the container name this environment shares under, given the
// image it ended up with. The resolved image is part of the name on purpose:
// when provisioning rebuilds — a new agent CLI, changed packages, a newer
// dispatch — the next task must get a container built from the new image
// instead of adopting the one still running the old one.
func (e *Env) nameFor(image string) string {
	if e.key == "" {
		return ""
	}
	return "dispatch-" + slug(e.key) + "-" + hash(image, e.key, e.spec.Workdir, e.user, strings.Join(e.extra, " "), envFingerprint(e.spec.Env))
}

// adopt returns the id of an existing reusable container, starting it again
// if it had been stopped. It returns "" when there is nothing to adopt.
func (e *Env) adopt(ctx context.Context) (string, error) {
	out, err := run(ctx, e.bin, nil, "inspect", "-f", "{{.Id}} {{.State.Running}}", e.name)
	if err != nil {
		return "", nil // no such container
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return "", nil
	}
	id, running := fields[0], fields[1] == "true"
	if running {
		return id, nil
	}
	if _, err := run(ctx, e.bin, nil, "start", id); err != nil {
		// A container that will not start again is worse than no
		// container: drop it and let the caller create a fresh one.
		_, _ = run(ctx, e.bin, nil, "rm", "-f", id)
		return "", nil
	}
	return id, nil
}

// ensureVolume creates the persistent $HOME volume and seeds it from the
// image's home skeleton, owned by the container user. Without this the
// volume would be an empty root-owned directory and the agent could not
// write ~/.claude.
func (e *Env) ensureVolume(ctx context.Context) error {
	if e.volume == "" || e.home == "" {
		return nil
	}
	if _, err := run(ctx, e.bin, nil, "volume", "inspect", e.volume); err == nil {
		return nil
	}
	if _, err := run(ctx, e.bin, nil, "volume", "create", "--label", labelManaged+"=1", e.volume); err != nil {
		return fmt.Errorf("docker: create home volume: %w", err)
	}
	seed := fmt.Sprintf(`set -e
if [ -d %s ] && [ -z "$(ls -A %s 2>/dev/null)" ]; then cp -a %s/. %s/ 2>/dev/null || true; fi
chown -R %d:%d %s`,
		shellQuote(homeSkeleton), shellQuote(e.home), shellQuote(homeSkeleton), shellQuote(e.home), e.uid, e.gid, shellQuote(e.home))
	if _, err := run(ctx, e.bin, nil, "run", "--rm", "-u", "0:0", "-v", e.volume+":"+e.home,
		"--entrypoint", "sh", e.image, "-c", seed); err != nil {
		return fmt.Errorf("docker: seed home volume: %w", err)
	}
	return nil
}

func (e *Env) Exec(ctx context.Context, name string, args ...string) (environment.Process, error) {
	id, err := e.container()
	if err != nil {
		return nil, err
	}
	argv := []string{"exec", "-i", "-w", ContainerWorkdir}
	for _, k := range sortedKeys(e.spec.Env) {
		argv = append(argv, "-e", k+"="+e.spec.Env[k])
	}
	argv = append(argv, id, name)
	argv = append(argv, args...)
	return environment.StartCmd(exec.CommandContext(ctx, e.bin, argv...))
}

func (e *Env) CopyIn(ctx context.Context, src io.Reader, dst string) error {
	id, err := e.container()
	if err != nil {
		return err
	}
	dst = e.resolve(dst)
	_, err = run(ctx, e.bin, src, "exec", "-i", id, "sh", "-c", fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(filepath.Dir(dst)), shellQuote(dst)))
	return err
}

func (e *Env) CopyOut(ctx context.Context, src string) (io.ReadCloser, error) {
	id, err := e.container()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, e.bin, "exec", id, "cat", e.resolve(src))
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &waitCloser{ReadCloser: out, cmd: cmd}, nil
}

// Stop releases the environment. A throwaway container is removed; a reused
// one is left running for the next task on the same key and only its
// last-used stamp is refreshed, so Reap can retire it later.
func (e *Env) Stop(ctx context.Context) error {
	e.mu.Lock()
	id, name := e.id, e.name
	e.id = ""
	e.mu.Unlock()
	if id == "" {
		return nil
	}
	if name != "" {
		e.touch()
		release(name)
		return nil
	}
	_, err := run(ctx, e.bin, nil, "rm", "-f", id)
	return err
}

// ContainerID returns the running container id ("" before Start).
func (e *Env) ContainerID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.id
}

// ContainerName returns the shared container's name ("" when throwaway).
func (e *Env) ContainerName() string { return e.name }

// Image returns the image actually used, which is the provisioned one when
// provisioning ran.
func (e *Env) Image() string { return e.image }

// Home returns $HOME inside the container.
func (e *Env) Home() string {
	if e.home == "" {
		return "/tmp"
	}
	return e.home
}

// touch records that the shared container was in use just now.
func (e *Env) touch() {
	p := e.markerPath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	now := time.Now()
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
	_ = os.Chtimes(p, now, now)
}

func (e *Env) markerPath() string {
	if e.state == "" || e.name == "" {
		return ""
	}
	return filepath.Join(e.state, "containers", e.name)
}

// Reap removes reused containers that have been idle longer than ttl, along
// with their last-used stamps. Containers a live task is using are skipped.
// Home volumes are kept: the login inside them is worth more than the disk.
func (f Factory) Reap(ctx context.Context, ttl time.Duration) error {
	if f.StateDir == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = DefaultReuseTTL
	}
	out, err := run(ctx, f.binary(), nil, "ps", "-a", "--filter", "label="+labelManaged+"=1", "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	dir := filepath.Join(f.StateDir, "containers")
	cutoff := time.Now().Add(-ttl)
	for _, name := range strings.Fields(out) {
		if busy(name) {
			continue
		}
		marker := filepath.Join(dir, name)
		fi, err := os.Stat(marker)
		if err != nil {
			// Unknown to us — a leftover from an older dispatch. Stamp it
			// now so it is reaped ttl from here rather than immediately.
			if mkErr := os.MkdirAll(dir, 0o755); mkErr == nil {
				if fh, cErr := os.Create(marker); cErr == nil {
					fh.Close()
				}
			}
			continue
		}
		if fi.ModTime().After(cutoff) {
			continue
		}
		if _, err := run(ctx, f.binary(), nil, "rm", "-f", name); err != nil {
			continue
		}
		_ = os.Remove(marker)
	}
	return nil
}

func (e *Env) container() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id == "" {
		return "", fmt.Errorf("docker: environment not started")
	}
	return e.id, nil
}

func (e *Env) resolve(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return ContainerWorkdir + "/" + p
}

// imageHome reads HOME out of an image's configuration, so an image that
// already declares a home is not overridden with /tmp.
func imageHome(ctx context.Context, bin, ref string) string {
	out, err := run(ctx, bin, nil, "image", "inspect", "-f", "{{range .Config.Env}}{{println .}}{{end}}", ref)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "HOME="); ok {
			return v
		}
	}
	return ""
}

func run(ctx context.Context, bin string, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

type waitCloser struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (w *waitCloser) Close() error {
	w.ReadCloser.Close()
	return w.cmd.Wait()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// slug turns a reuse key (a Slack "C123/170.5" thread id, an agent name)
// into something docker accepts in a container name.
func slug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 24 {
		out = strings.Trim(out[:24], "-")
	}
	if out == "" {
		out = "env"
	}
	return out
}

func hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:4])
}

func envFingerprint(env map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\n')
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
