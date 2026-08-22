// Package environment defines where agents run.
//
// An Environment is anything that can run a process and pipe to it: a local
// folder, a Docker container, or a machine over SSH. Everything above this
// layer assumes only "I can exec a command and stream its stdio".
package environment

import (
	"context"
	"io"
)

// Kind names an environment implementation.
type Kind string

const (
	KindLocal  Kind = "local"
	KindDocker Kind = "docker"
	KindSSH    Kind = "ssh"
)

// Spec describes an environment to create. Only the fields relevant to Kind
// are used.
type Spec struct {
	Kind    Kind
	Workdir string            // working directory inside the environment
	Env     map[string]string // extra environment variables for every Exec
	Image   string            // docker: image to run
	Host    string            // ssh: user@host[:port]
	KeyPath string            // ssh: private key path
}

// Process is a running command inside an environment.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	// Wait blocks until the process exits and returns its exit code.
	Wait() (int, error)
	// Kill terminates the process.
	Kill() error
}

// Environment is the interface every runtime implements.
type Environment interface {
	Kind() Kind
	// Start provisions the environment (start container, open SSH session).
	Start(ctx context.Context) error
	// Exec starts a command with stdio pipes attached.
	Exec(ctx context.Context, name string, args ...string) (Process, error)
	// CopyIn writes a file into the environment at dst.
	CopyIn(ctx context.Context, src io.Reader, dst string) error
	// CopyOut reads a file from the environment at src.
	CopyOut(ctx context.Context, src string) (io.ReadCloser, error)
	// Stop tears the environment down. Idempotent.
	Stop(ctx context.Context) error
}

// Factory builds an Environment from a Spec.
type Factory interface {
	New(spec Spec) (Environment, error)
}
