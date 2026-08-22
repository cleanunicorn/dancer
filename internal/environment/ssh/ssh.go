// Package ssh runs agents on a remote machine through the `ssh` CLI, so the
// user's ~/.ssh/config, agent and known_hosts apply unchanged.
//
// Spec.Host is "user@host" or a Host alias; Spec.KeyPath is optional;
// Spec.Workdir is the remote directory every Exec runs in.
package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path"
	"sort"
	"strings"

	"github.com/cleanunicorn/dancer/internal/environment"
)

// Factory builds ssh environments.
type Factory struct {
	// Binary is the ssh CLI (default "ssh").
	Binary string
	// ExtraArgs are inserted before the host (e.g. -p 2222, -J jump).
	ExtraArgs []string
}

// Env is one remote host + workdir.
type Env struct {
	bin   string
	spec  environment.Spec
	extra []string
}

func (f Factory) New(spec environment.Spec) (environment.Environment, error) {
	if spec.Host == "" {
		return nil, fmt.Errorf("ssh: host is required")
	}
	if spec.Workdir == "" {
		return nil, fmt.Errorf("ssh: workdir is required")
	}
	bin := f.Binary
	if bin == "" {
		bin = "ssh"
	}
	return &Env{bin: bin, spec: spec, extra: f.ExtraArgs}, nil
}

func (e *Env) Kind() environment.Kind { return environment.KindSSH }

func (e *Env) Start(ctx context.Context) error {
	_, err := e.run(ctx, nil, "mkdir -p "+shellQuote(e.spec.Workdir))
	return err
}

func (e *Env) Exec(ctx context.Context, name string, args ...string) (environment.Process, error) {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(name))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	script := e.prefix() + strings.Join(parts, " ")
	return environment.StartCmd(exec.CommandContext(ctx, e.bin, e.sshArgs(script)...))
}

func (e *Env) CopyIn(ctx context.Context, src io.Reader, dst string) error {
	dst = e.resolve(dst)
	_, err := e.run(ctx, src, fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(path.Dir(dst)), shellQuote(dst)))
	return err
}

func (e *Env) CopyOut(ctx context.Context, src string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, e.bin, e.sshArgs("cat "+shellQuote(e.resolve(src)))...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &waitCloser{ReadCloser: out, cmd: cmd}, nil
}

// Stop is a no-op; the remote workdir is left in place.
func (e *Env) Stop(ctx context.Context) error { return nil }

// prefix builds "cd workdir && export K=V ... && ".
func (e *Env) prefix() string {
	b := "cd " + shellQuote(e.spec.Workdir) + " && "
	keys := make([]string, 0, len(e.spec.Env))
	for k := range e.spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b += "export " + k + "=" + shellQuote(e.spec.Env[k]) + " && "
	}
	return b
}

func (e *Env) sshArgs(script string) []string {
	args := []string{"-o", "BatchMode=yes"}
	if e.spec.KeyPath != "" {
		args = append(args, "-i", e.spec.KeyPath)
	}
	args = append(args, e.extra...)
	args = append(args, e.spec.Host, "--", script)
	return args
}

func (e *Env) run(ctx context.Context, stdin io.Reader, script string) (string, error) {
	cmd := exec.CommandContext(ctx, e.bin, e.sshArgs(script)...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh %s: %w: %s", e.spec.Host, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (e *Env) resolve(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return path.Join(e.spec.Workdir, p)
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
