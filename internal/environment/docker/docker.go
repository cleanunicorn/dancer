// Package docker runs agents inside a container started from Spec.Image.
// It shells out to the `docker` CLI so the user's docker context, auth and
// rootless setup apply unchanged.
//
// Spec.Workdir on the host is bind-mounted at /work inside the container;
// every Exec runs in /work with Spec.Env exported.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cleanunicorn/dancer/internal/environment"
)

// ContainerWorkdir is the mount point of Spec.Workdir inside the container.
const ContainerWorkdir = "/work"

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
}

// Env is one container.
type Env struct {
	bin   string
	spec  environment.Spec
	extra []string
	user  string

	mu sync.Mutex
	id string
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
	bin := f.Binary
	if bin == "" {
		bin = "docker"
	}
	user := f.User
	if user == "" {
		user = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	if user == "root" {
		user = ""
	}
	return &Env{bin: bin, spec: spec, extra: f.ExtraRunArgs, user: user}, nil
}

func (e *Env) Kind() environment.Kind { return environment.KindDocker }

func (e *Env) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id != "" {
		return nil
	}
	args := []string{"run", "-d", "--rm", "-w", ContainerWorkdir, "-v", e.spec.Workdir + ":" + ContainerWorkdir}
	if e.user != "" {
		args = append(args, "--user", e.user)
	}
	if _, ok := e.spec.Env["HOME"]; !ok {
		// Non-root users usually have no home in the image; claude needs a
		// writable one for ~/.claude.json.
		args = append(args, "-e", "HOME=/tmp")
	}
	for k, v := range e.spec.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, e.extra...)
	args = append(args, e.spec.Image, "sleep", "infinity")
	out, err := e.docker(ctx, nil, args...)
	if err != nil {
		return err
	}
	e.id = strings.TrimSpace(out)
	return nil
}

func (e *Env) Exec(ctx context.Context, name string, args ...string) (environment.Process, error) {
	id, err := e.container()
	if err != nil {
		return nil, err
	}
	argv := []string{"exec", "-i", "-w", ContainerWorkdir}
	for k, v := range e.spec.Env {
		argv = append(argv, "-e", k+"="+v)
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
	_, err = e.docker(ctx, src, "exec", "-i", id, "sh", "-c", fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(filepath.Dir(dst)), shellQuote(dst)))
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

func (e *Env) Stop(ctx context.Context) error {
	e.mu.Lock()
	id := e.id
	e.id = ""
	e.mu.Unlock()
	if id == "" {
		return nil
	}
	_, err := e.docker(ctx, nil, "rm", "-f", id)
	return err
}

// ContainerID returns the running container id ("" before Start).
func (e *Env) ContainerID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.id
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

func (e *Env) docker(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.bin, args...)
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
