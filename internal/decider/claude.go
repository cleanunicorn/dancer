package decider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Claude answers with one `claude -p` call: a small model, no tools, no
// session (and none written to disk), and an empty scratch directory to
// run in. It always runs on the
// dispatch host — a policy question is about dispatch, not about the task's
// environment.
//
// The scratch directory matters more than it looks. dispatch is normally
// started from a repository checkout, and a CLI started there would read
// that project's CLAUDE.md, .claude/settings.json (hooks included) and MCP
// config — reaching the decider as instructions, ahead of its own policy.
// The whole point of this package is that project and agent text is
// evidence to judge, never orders, so the decider is run somewhere with
// nothing in it and with MCP servers switched off.
type Claude struct {
	Binary  string        // default "claude"
	Model   string        // default "haiku"
	Timeout time.Duration // default 15s
}

func (c Claude) Name() string { return "claude" }

func (c Claude) Decide(ctx context.Context, q Question) (Verdict, error) {
	body, err := json.Marshal(q)
	if err != nil {
		return Verdict{}, fmt.Errorf("decider: marshal question: %w", err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin, model := c.Binary, c.Model
	if bin == "" {
		bin = "claude"
	}
	if model == "" {
		model = "haiku"
	}
	dir, err := os.MkdirTemp("", "dispatch-decider-")
	if err != nil {
		return Verdict{}, fmt.Errorf("decider: scratch dir: %w", err)
	}
	defer os.RemoveAll(dir)

	cmd := exec.CommandContext(ctx, bin,
		"-p",
		"--output-format", "json",
		"--model", model,
		"--tools", "", // no built-in tools at all, not merely none pre-approved
		"--permission-mode", "manual",
		"--strict-mcp-config",      // no MCP servers: nothing to reach out to
		"--no-session-persistence", // one question, no transcript left under ~/.claude/projects
		"--append-system-prompt", policy,
	)
	cmd.Dir = dir // empty: no project CLAUDE.md, settings or hooks
	// On timeout Go kills the CLI, but Run still waits for its stdout to
	// close — and a child the CLI forked holds that pipe open. WaitDelay
	// is what makes the timeout a timeout.
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = bytes.NewReader(body)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Verdict{}, fmt.Errorf("decider: %s: %w: %s", bin, err, strings.TrimSpace(errb.String()))
	}
	var res struct {
		Result  string  `json:"result"`
		IsError bool    `json:"is_error"`
		Cost    float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return Verdict{}, fmt.Errorf("decider: parse cli output: %w", err)
	}
	if res.IsError {
		return Verdict{}, fmt.Errorf("decider: cli reported an error: %s", truncate(res.Result, 200))
	}
	v, err := parseVerdict(res.Result)
	if err != nil {
		return Verdict{}, err
	}
	v.By = c.Name()
	return Validate(q, v)
}

// parseVerdict pulls the JSON object out of a model reply that may be
// fenced, prefixed or followed by prose.
func parseVerdict(text string) (Verdict, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return Verdict{}, fmt.Errorf("decider: no JSON object in reply: %s", truncate(strings.TrimSpace(text), 200))
	}
	var v Verdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &v); err != nil {
		return Verdict{}, fmt.Errorf("decider: parse verdict: %w", err)
	}
	return v, nil
}
