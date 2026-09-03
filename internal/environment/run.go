package environment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Both helpers here read the process to EOF and only then wait for it,
// which is the one ordering os/exec allows: a Process's streams are pipes
// (StartCmd, execproc.go), and Wait closes them the moment the command
// exits — so a copy still running when Wait returns loses whatever the
// child had written but nobody had read yet, and reads a buffer somebody
// is still writing. The claude driver has always done it this way (it
// scans stdout to EOF, then waits); these two now agree with it.
//
// It costs what it costs: a child that leaves a grandchild holding the
// pipe open keeps the copy running, and the wait with it. Both of these
// are for short commands with the context on them, and the driver accepts
// the same bargain for the same reason.

// Run executes one short command in env, writes stdin to it and returns
// what it printed on stdout. It is for the small housekeeping scripts
// dispatch runs inside an environment before an agent starts — lending a
// login, probing for a binary — not for anything long-lived: it reads to
// EOF and waits for the exit.
//
// A non-zero exit is an error carrying the tail of stderr.
func Run(ctx context.Context, env Environment, stdin []byte, name string, args ...string) (string, error) {
	proc, err := env.Exec(ctx, name, args...)
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&stdout, proc.Stdout()) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&stderr, proc.Stderr()) }()
	if len(stdin) > 0 {
		if _, err := proc.Stdin().Write(stdin); err != nil {
			proc.Kill()
			return "", fmt.Errorf("write stdin: %w", err)
		}
	}
	if err := proc.Stdin().Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return "", fmt.Errorf("close stdin: %w", err)
	}
	// Both copies, before the wait and before either buffer is read: the
	// stderr one used to be waited for by nothing at all, and the error
	// below read it while it was still being written.
	wg.Wait()
	code, err := proc.Wait()
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Check runs one command through a shell and reports how it went: the
// exit code and everything it printed, stdout and stderr interleaved in
// the order a person would have seen them.
//
// It is Run's opposite number. Run is for housekeeping dispatch needs to
// succeed, so a non-zero exit is an error and the output is thrown away;
// Check is for a command whose *failing* is the answer — a workflow
// step's `check = "make test"` — so the exit code is data and the output
// is the evidence that goes into the failure message. Which is also why
// the draining above matters most here: the output of a check that failed
// is the whole of what the thread is told, and a test log is exactly the
// size that gets cut off.
//
// The command is handed to `sh -c` rather than split here: it comes from
// a human's config and is written as a shell line ("make test && make
// lint"), and the environment is the one the agent works in, which has
// already run far more than this on the agent's say-so.
func Check(ctx context.Context, env Environment, cmd string) (code int, output string, err error) {
	proc, err := env.Exec(ctx, "sh", "-c", cmd)
	if err != nil {
		return 0, "", err
	}
	var out lockedBuffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&out, proc.Stdout()) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&out, proc.Stderr()) }()
	_ = proc.Stdin().Close()
	wg.Wait()
	code, err = proc.Wait()
	return code, strings.TrimSpace(out.String()), err
}

// lockedBuffer lets stdout and stderr be copied into one buffer from two
// goroutines. Their lines interleave the way they would on a terminal;
// which of two simultaneous writes lands first is not something a check's
// output has to promise.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
