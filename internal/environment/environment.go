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

// Reuse says how long an environment lives and who shares it. It is only
// meaningful for environments that are expensive to create (docker).
type Reuse string

const (
	// ReuseTask is the default: a fresh environment per task, destroyed
	// when the task ends.
	ReuseTask Reuse = "task"
	// ReuseThread keeps one environment per conversation thread, so
	// follow-ups land in the same box with the same installed tools and
	// the same agent session history.
	ReuseThread Reuse = "thread"
	// ReuseDefinition keeps one environment per agent definition, shared
	// by every thread that runs it.
	ReuseDefinition Reuse = "definition"
)

// Provision describes how to turn a plain base image into one an agent can
// run in. A nil *Provision means "the image is already ready, change
// nothing".
type Provision struct {
	// Agents are the agent CLIs to make available ("claude", "codex").
	Agents []string
	// Packages are extra OS packages to install with the image's package
	// manager.
	Packages []string
	// Setup are extra shell commands run as root after everything else.
	Setup []string
}

// Empty reports whether the provision asks for nothing at all.
func (p *Provision) Empty() bool {
	return p == nil || (len(p.Agents) == 0 && len(p.Packages) == 0 && len(p.Setup) == 0)
}

// Spec describes an environment to create. Only the fields relevant to Kind
// are used.
type Spec struct {
	Kind    Kind
	Workdir string            // working directory inside the environment
	Env     map[string]string // extra environment variables for every Exec
	Image   string            // docker: image to run
	Host    string            // ssh: user@host[:port]
	KeyPath string            // ssh: private key path

	// Provision (docker) makes the image agent-ready before first use.
	Provision *Provision
	// Reuse (docker) is the lifetime of the environment; empty = ReuseTask.
	Reuse Reuse
	// ReuseKey identifies the environment to share for the chosen Reuse
	// scope (the thread id, the definition name). Empty = not shared.
	ReuseKey string
}

// String names the environment in a few words: the kind, the docker image
// or ssh host, and the working directory when one is set.
func (s Spec) String() string {
	out := string(s.Kind)
	switch s.Kind {
	case KindDocker:
		if s.Image != "" {
			out += " " + s.Image
		}
	case KindSSH:
		if s.Host != "" {
			out += " " + s.Host
		}
	}
	if s.Workdir != "" {
		out += " " + s.Workdir
	}
	return out
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
