package environment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Run executes one short command in env, writes stdin to it and returns
// what it printed on stdout. It is for the small housekeeping scripts
// dancer runs inside an environment before an agent starts — lending a
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(&stdout, proc.Stdout())
	}()
	go func() { _, _ = io.Copy(&stderr, proc.Stderr()) }()
	if len(stdin) > 0 {
		if _, err := proc.Stdin().Write(stdin); err != nil {
			proc.Kill()
			return "", fmt.Errorf("write stdin: %w", err)
		}
	}
	if err := proc.Stdin().Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return "", fmt.Errorf("close stdin: %w", err)
	}
	code, err := proc.Wait()
	<-done
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
