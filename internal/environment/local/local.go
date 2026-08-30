// Package local runs agents directly on this machine in a working directory.
package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/cleanunicorn/dispatch/internal/environment"
)

// Env is a local-folder environment.
type Env struct {
	spec environment.Spec
}

// Factory builds local environments.
type Factory struct{}

// New implements environment.Factory.
func (Factory) New(spec environment.Spec) (environment.Environment, error) {
	if spec.Workdir == "" {
		return nil, fmt.Errorf("local: workdir is required")
	}
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return nil, err
	}
	spec.Workdir = abs
	return &Env{spec: spec}, nil
}

func (e *Env) Kind() environment.Kind { return environment.KindLocal }

// Start creates the working directory if it does not exist.
func (e *Env) Start(ctx context.Context) error {
	return os.MkdirAll(e.spec.Workdir, 0o755)
}

func (e *Env) Exec(ctx context.Context, name string, args ...string) (environment.Process, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = e.spec.Workdir
	cmd.Env = append(os.Environ(), flatten(e.spec.Env)...)
	return environment.StartCmd(cmd)
}

func (e *Env) CopyIn(ctx context.Context, src io.Reader, dst string) error {
	path := e.resolve(dst)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, src)
	return err
}

func (e *Env) CopyOut(ctx context.Context, src string) (io.ReadCloser, error) {
	return os.Open(e.resolve(src))
}

// Stop is a no-op for local folders.
func (e *Env) Stop(ctx context.Context) error { return nil }

func (e *Env) resolve(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(e.spec.Workdir, p)
}

func flatten(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
