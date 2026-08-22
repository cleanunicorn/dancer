package environment

import (
	"errors"
	"io"
	"os/exec"
	"sync"
)

// cmdProcess adapts *exec.Cmd to Process. Shared by every environment that
// ultimately runs a local binary (local exec, `docker exec`, `ssh`).
type cmdProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	waitOnce sync.Once
	code     int
	waitErr  error
}

// StartCmd wires stdio pipes, starts cmd and returns it as a Process.
func StartCmd(cmd *exec.Cmd) (Process, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

func (p *cmdProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *cmdProcess) Stdout() io.Reader     { return p.stdout }
func (p *cmdProcess) Stderr() io.Reader     { return p.stderr }

func (p *cmdProcess) Wait() (int, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			p.code = 0
		case errors.As(err, &exitErr):
			p.code = exitErr.ExitCode()
		default:
			p.code = -1
			p.waitErr = err
		}
	})
	return p.code, p.waitErr
}

func (p *cmdProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}
