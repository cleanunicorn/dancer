// Package claude drives the Claude Code CLI (`claude -p`) over its
// bidirectional stream-json protocol and normalizes it into agent.Event.
//
// Besides the handshake (initialize, then answer every can_use_tool) the
// driver sends one more control request: on a subscription login it asks
// get_usage after each result and stamps the answer — how much of the
// plan's 5-hour, 7-day and per-model windows is used — on the result as
// agent.Event.Usage. The CLI does the lookup itself, with its own login,
// so it works the same in a container or over SSH; the result waits at
// most usageTimeout for it and goes out without usage when the CLI
// cannot say (older version, offline).
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
)

// Agent implements agent.Agent for Claude Code.
type Agent struct {
	// Binary is the claude executable name or path inside the environment.
	Binary string
}

// New returns an Agent using the default "claude" binary.
func New() *Agent { return &Agent{Binary: "claude"} }

func (a *Agent) Kind() agent.Kind { return agent.KindClaude }

func (a *Agent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	return a.start(ctx, env, def, "", prompt)
}

func (a *Agent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	if session == "" {
		return nil, fmt.Errorf("claude: resume requires a session id")
	}
	return a.start(ctx, env, def, session, prompt)
}

// shareFilesHint is appended to every system prompt so agents know how
// attachments reach the human.
const shareFilesHint = "You are operated through a chat channel (e.g. Slack). The human cannot open files on this machine. " +
	"To show them a file you produced (screenshot, report, diagram), write its absolute path on its own in your reply, e.g. `/tmp/settings-top.png`; " +
	"dancer uploads every mentioned path that exists. Images and PDFs render inline in the chat."

// cliModes maps the one permission mode the CLI names differently: dancer's
// ask-for-everything "manual" is the CLI's "default". Every other mode shares
// its name on both sides.
var cliModes = map[agent.PermissionMode]string{agent.PermissionManual: "default"}

// cliMode is the --permission-mode value for a definition's mode.
func cliMode(m agent.PermissionMode) string {
	if m == "" {
		m = agent.PermissionManual
	}
	if s, ok := cliModes[m]; ok {
		return s
	}
	return string(m)
}

// fromCLIMode maps the mode the CLI reports on init back to agent.PermissionMode.
func fromCLIMode(s string) agent.PermissionMode {
	for m, c := range cliModes {
		if c == s {
			return m
		}
	}
	return agent.PermissionMode(s)
}

// args builds the CLI argument list for a definition.
func args(def agent.Definition, session string) ([]string, error) {
	out := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
	}
	out = append(out, "--permission-mode", cliMode(def.PermissionMode))
	if def.Model != "" {
		out = append(out, "--model", def.Model)
	}
	out = append(out, "--append-system-prompt", strings.TrimSpace(shareFilesHint+"\n\n"+def.SystemPrompt))
	if len(def.AllowedTools) > 0 {
		out = append(out, "--allowedTools", strings.Join(def.AllowedTools, ","))
	}
	if len(def.SubAgents) > 0 {
		b, err := json.Marshal(def.SubAgents)
		if err != nil {
			return nil, fmt.Errorf("claude: marshal sub-agents: %w", err)
		}
		out = append(out, "--agents", string(b))
	}
	if def.MCPConfig != "" {
		out = append(out, "--mcp-config", def.MCPConfig)
	}
	if session != "" {
		out = append(out, "--resume", session)
	}
	return out, nil
}

func (a *Agent) start(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	argv, err := args(def, session)
	if err != nil {
		return nil, err
	}
	bin := a.Binary
	if bin == "" {
		bin = "claude"
	}
	proc, err := env.Exec(ctx, bin, argv...)
	if err != nil {
		return nil, fmt.Errorf("claude: exec: %w", err)
	}
	r := &run{
		proc:    proc,
		events:  make(chan agent.Event, 64),
		pending: map[string]pendingPerm{},
		done:    make(chan struct{}),
	}
	// Handshake: the CLI only routes permission prompts to stdio after an
	// initialize control request.
	if err := r.write(controlRequestOut{Type: "control_request", RequestID: "init", Request: map[string]any{"subtype": "initialize"}}); err != nil {
		proc.Kill()
		return nil, fmt.Errorf("claude: initialize: %w", err)
	}
	if err := r.writeUser(prompt); err != nil {
		proc.Kill()
		return nil, fmt.Errorf("claude: prompt: %w", err)
	}
	go r.drainStderr()
	go r.loop()
	return r, nil
}

// pendingPerm is a permission request awaiting a decision.
type pendingPerm struct {
	reqID string
	input map[string]any
}

// run is one live claude process.
type run struct {
	proc   environment.Process
	events chan agent.Event

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]pendingPerm // tool_use_id -> control request
	stderr  strings.Builder
	done    chan struct{}
	billing agent.Billing // learned from init, stamped on results; subscription also asks for usage
}

func (r *run) Events() <-chan agent.Event { return r.events }

func (r *run) Send(ctx context.Context, text string) error {
	return r.writeUser(text)
}

func (r *run) Decide(ctx context.Context, d agent.PermissionDecision) error {
	r.mu.Lock()
	pp, ok := r.pending[d.ToolID]
	if ok {
		delete(r.pending, d.ToolID)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("claude: no pending permission for tool id %q", d.ToolID)
	}
	var payload any
	if d.Allow {
		input := pp.input
		if d.Answers != nil {
			// AskUserQuestion: the answers travel back inside the tool input.
			input = make(map[string]any, len(pp.input)+1)
			for k, v := range pp.input {
				input[k] = v
			}
			input["answers"] = d.Answers
		}
		payload = permissionAllow{Behavior: "allow", UpdatedInput: input}
	} else {
		msg := d.Reason
		if msg == "" {
			msg = "denied by operator"
		}
		payload = permissionDeny{Behavior: "deny", Message: msg}
	}
	return r.write(controlResponseOut{Type: "control_response", Response: controlResponseBody{Subtype: "success", RequestID: pp.reqID, Response: payload}})
}

func (r *run) Stop() error {
	r.writeMu.Lock()
	r.proc.Stdin().Close()
	r.writeMu.Unlock()
	select {
	case <-r.done:
		return nil
	case <-time.After(5 * time.Second):
		return r.proc.Kill()
	}
}

func (r *run) writeUser(text string) error {
	return r.write(userMessage{Type: "user", Message: userMsgPayload{Role: "user", Content: text}})
}

func (r *run) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_, err = r.proc.Stdin().Write(append(b, '\n'))
	return err
}

func (r *run) drainStderr() {
	sc := bufio.NewScanner(r.proc.Stderr())
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		r.mu.Lock()
		if r.stderr.Len() < 16*1024 {
			r.stderr.WriteString(sc.Text())
			r.stderr.WriteByte('\n')
		}
		r.mu.Unlock()
	}
}

// usageTimeout bounds how long a turn's result waits for the plan usage
// that follows it. The CLI's own lookup gives up after five seconds; a
// result held longer than that is a result nobody sees.
var usageTimeout = 5 * time.Second

func (r *run) loop() {
	defer close(r.events)
	defer close(r.done)
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r.proc.Stdout())
		sc.Buffer(make([]byte, 0, 256*1024), 16<<20)
		for sc.Scan() {
			if raw := append([]byte(nil), sc.Bytes()...); len(raw) > 0 {
				lines <- raw
			}
		}
	}()

	// On a subscription every result is held back, briefly, for the
	// plan usage that answers the get_usage request sent after it: the
	// closing line should say how much of the plan is left *after* the
	// turn, not before. Anything else that arrives meanwhile queues
	// behind the held result so the event order survives.
	sawResult := false
	var held *agent.Event
	var queued []agent.Event
	var deadline <-chan time.Time
	usageReq, usageN := "", 0
	flush := func() {
		if held != nil {
			r.events <- *held
			held, deadline, usageReq = nil, nil, ""
		}
		for _, ev := range queued {
			r.events <- ev
		}
		queued = nil
	}
	emit := func(ev agent.Event) {
		if held != nil {
			queued = append(queued, ev)
			return
		}
		r.events <- ev
	}
	hold := func(ev agent.Event) {
		usageN++
		usageReq = fmt.Sprintf("usage-%d", usageN)
		if err := r.write(controlRequestOut{Type: "control_request", RequestID: usageReq, Request: map[string]any{"subtype": "get_usage"}}); err != nil {
			usageReq = ""
			r.events <- ev
			return
		}
		held = &ev
		deadline = time.After(usageTimeout)
	}

loop:
	for {
		var raw []byte
		var ok bool
		select {
		case <-deadline:
			flush()
			continue
		case raw, ok = <-lines:
			if !ok {
				break loop
			}
		}
		p, err := translate(raw, time.Now())
		if err != nil {
			emit(agent.Event{Type: agent.EventError, At: time.Now(), Text: "claude: bad line: " + err.Error(), Raw: raw})
			continue
		}
		for _, ev := range p.Events {
			switch ev.Type {
			case agent.EventInit:
				r.billing = ev.Billing
			case agent.EventResult:
				sawResult = true
				ev.Billing = r.billing
				if r.billing == agent.BillingSubscription && held == nil {
					hold(ev)
					continue
				}
			case agent.EventError:
				sawResult = true
				ev.Billing = r.billing
			}
			emit(ev)
		}
		if p.Permission != nil {
			r.mu.Lock()
			r.pending[p.Permission.Event.ToolID] = pendingPerm{reqID: p.Permission.RequestID, input: p.Permission.Event.ToolInput}
			r.mu.Unlock()
			emit(p.Permission.Event)
		}
		if p.Control != nil {
			// Unknown control requests (hooks, dialogs) are acknowledged
			// with an empty success so the CLI does not block.
			_ = r.write(controlResponseOut{Type: "control_response", Response: controlResponseBody{Subtype: "success", RequestID: p.Control.RequestID, Response: map[string]any{}}})
		}
		if p.Response != nil && held != nil && p.Response.RequestID == usageReq {
			if p.Response.Subtype == "success" {
				held.Usage = parseUsage(p.Response.Response)
			}
			flush()
		}
	}
	flush()
	code, _ := r.proc.Wait()
	if code != 0 && !sawResult {
		r.mu.Lock()
		tail := r.stderr.String()
		r.mu.Unlock()
		r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: fmt.Sprintf("claude exited with code %d: %s", code, strings.TrimSpace(tail))}
	}
}
