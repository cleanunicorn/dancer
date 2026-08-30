// Package claude drives the Claude Code CLI (`claude -p`) over its
// bidirectional stream-json protocol and normalizes it into agent.Event.
//
// Besides the handshake (initialize, then answer every can_use_tool) the
// driver sends one more control request: on a subscription login it asks
// get_usage after each result and emits the answer — how much of the
// plan's 5-hour, 7-day and per-model windows is used — as its own
// agent.EventUsage, after the result. The result never waits for it: the
// usage line lands a moment after the result or, when the CLI cannot say
// (older version, offline), not at all. The CLI does the lookup itself,
// with its own login, so it works the same in a container or over SSH.
// Needs claude 2.1.240.
//
// A result line is not always the end of the turn: the CLI emits one
// whenever the model stops, and with a sub-agent still running it will
// start the model again (a second system/init, then another result) to
// deliver the outcome. The background tracker withholds such results so
// the layers above — which close the turn, idle the process and tell the
// human "done" on EventResult — only hear the last one; the second init
// passes through. If the process exits while a result is held, that
// result is delivered: nothing more is coming.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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
	// Credentials is the host login file lent to containers (see
	// login.go). Empty = the CLI's own location on this host.
	Credentials string
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
// attachments travel, both ways.
const shareFilesHint = "You are operated through a chat channel (e.g. Slack). The human cannot open files on this machine. " +
	"To show them a file you produced (screenshot, report, diagram), write its absolute path on its own in your reply, e.g. `/tmp/settings-top.png`; " +
	"dancer uploads every mentioned path that exists. Images and PDFs render inline in the chat. " +
	"Files the human attaches to a chat message are copied into this environment and their paths are listed at the end of that message; read them from disk (images and PDFs with the Read tool)."

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
	hint := a.lendLogin(ctx, env, def)
	proc, err := env.Exec(ctx, bin, argv...)
	if err != nil {
		return nil, fmt.Errorf("claude: exec: %w", err)
	}
	r := &run{
		proc:      proc,
		events:    make(chan agent.Event, 64),
		pending:   map[string]pendingPerm{},
		done:      make(chan struct{}),
		loginHint: hint,
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

	mu       sync.Mutex
	pending  map[string]pendingPerm // tool_use_id -> control request
	stderr   strings.Builder
	done     chan struct{}
	billing  agent.Billing // learned from init, stamped on results; subscription also asks for usage
	usageN   int           // get_usage requests sent, for their ids
	usageOff bool          // the CLI answered get_usage with an error: stop asking
	bg       background    // sub-agents and backgrounded commands still owed to the turn
	held     *agent.Event  // the last result withheld because bg was not settled
	// switching is the model a "/model <name>" now in flight asked for,
	// reported on the turn's result and then cleared (see modelArg).
	switching string

	// loginHint is appended to a "Not logged in" result when the driver
	// knew in advance the environment had no login (login.go).
	loginHint string
}

func (r *run) Events() <-chan agent.Event { return r.events }

func (r *run) Send(ctx context.Context, text string) error {
	if m := modelArg(text); m != "" {
		r.mu.Lock()
		r.switching = m
		r.mu.Unlock()
	}
	return r.writeUser(text)
}

// modelCmdRE matches the CLI's own "/model <name>", and only that: the
// bare "/model" is a question, and anything after the name is not a
// switch the CLI would accept.
var modelCmdRE = regexp.MustCompile(`^/model[ \t]+(\S+)[ \t]*$`)

// modelArg is the model a message asks the CLI to switch to, or "".
//
// dancer does not implement "/model" — the CLI reads it out of the
// message like every other command of its own, and this driver only
// reads along. It has to: the switch lives in this process, the CLI
// says so nowhere a machine can read (an English "Set model to Sonnet 5
// for this session only" is the whole of it), and the next --resume
// starts a process that would go back to the definition's model. What
// the human typed is exactly what --resume needs, so the driver notes
// it and reports it on the turn's result; the coordinator holds it
// (store.TaskState.ModelPin). Anything else a command changes still
// ends with the process.
func modelArg(text string) string {
	m := modelCmdRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return ""
	}
	return m[1]
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

func (r *run) loop() {
	defer close(r.events)
	defer close(r.done)
	sawResult := false // a result or error reached the caller this turn
	sc := bufio.NewScanner(r.proc.Stdout())
	sc.Buffer(make([]byte, 0, 256*1024), 16<<20)
	for sc.Scan() {
		raw := append([]byte(nil), sc.Bytes()...)
		if len(raw) == 0 {
			continue
		}
		p, err := translate(raw, time.Now())
		if err != nil {
			r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: "claude: bad line: " + err.Error(), Raw: raw}
			continue
		}
		r.bg.observe(p)
		for _, ev := range p.Events {
			switch ev.Type {
			case agent.EventInit:
				r.billing = ev.Billing
				sawResult = false
			case agent.EventResult, agent.EventError:
				ev.Billing = r.billing
				if ev.Type == agent.EventResult && !r.bg.settled() {
					// The model stopped, but a sub-agent is still owed to
					// this turn: the CLI will run the model again when it
					// finishes. The turn goes on.
					r.held = &ev
					continue
				}
				sawResult = true
				r.held = nil
				// A failed turn is an EventError, so a switch reported
				// here is one the CLI carried out; either way the note
				// is spent.
				r.mu.Lock()
				if ev.Type == agent.EventResult {
					ev.Model = r.switching
				}
				r.switching = ""
				r.mu.Unlock()
				if r.loginHint != "" && strings.Contains(ev.Text, "Not logged in") {
					ev.Text += r.loginHint
				}
			}
			r.events <- ev
			if (ev.Type == agent.EventResult || ev.Type == agent.EventError) && r.billing == agent.BillingSubscription {
				r.askUsage()
			}
		}
		if p.Permission != nil {
			r.mu.Lock()
			r.pending[p.Permission.Event.ToolID] = pendingPerm{reqID: p.Permission.RequestID, input: p.Permission.Event.ToolInput}
			r.mu.Unlock()
			r.events <- p.Permission.Event
		}
		if p.Control != nil {
			// Unknown control requests (hooks, dialogs) are acknowledged
			// with an empty success so the CLI does not block.
			_ = r.write(controlResponseOut{Type: "control_response", Response: controlResponseBody{Subtype: "success", RequestID: p.Control.RequestID, Response: map[string]any{}}})
		}
		if p.Response != nil && strings.HasPrefix(p.Response.RequestID, usageReqPrefix) {
			if ev, ok := r.usageEvent(p.Response, time.Now()); ok {
				r.events <- ev
			}
		}
	}
	code, _ := r.proc.Wait()
	if r.held != nil {
		// The process is gone with a result still held: whatever was
		// outstanding is not coming, so that result was the end — whichever
		// way the process went. A cancel or shutdown closes stdin on a CLI
		// still driving its sub-agent, which may outlive Stop's grace and
		// be killed; the model's last words beat an exit code.
		r.events <- *r.held
		r.held = nil
		sawResult = true
	}
	if code != 0 && !sawResult {
		r.mu.Lock()
		tail := r.stderr.String()
		r.mu.Unlock()
		r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: fmt.Sprintf("claude exited with code %d: %s", code, strings.TrimSpace(tail))}
	}
}

// usageReqPrefix starts the id of every get_usage request, so its answer
// is told apart from the initialize reply.
const usageReqPrefix = "usage-"

// askUsage sends a get_usage request after a turn, unless this CLI has
// already said it cannot answer one.
func (r *run) askUsage() {
	if r.usageOff {
		return
	}
	r.usageN++
	_ = r.write(controlRequestOut{Type: "control_request", RequestID: fmt.Sprintf("%s%d", usageReqPrefix, r.usageN), Request: map[string]any{"subtype": "get_usage"}})
}

// usageEvent turns a get_usage answer into an EventUsage. An error answer
// (a CLI without get_usage) switches the question off for this process.
func (r *run) usageEvent(resp *controlResponse, now time.Time) (agent.Event, bool) {
	if resp.Subtype != "success" {
		r.usageOff = true
		return agent.Event{}, false
	}
	u := parseUsage(resp.Response)
	if u == nil {
		return agent.Event{}, false
	}
	return agent.Event{Type: agent.EventUsage, At: now, Usage: u, Billing: r.billing}, true
}
