// Package codex drives the Codex CLI app-server JSON-RPC protocol.
//
// Unlike `codex exec`, app-server keeps a thread open, streams tool and text
// events, accepts mid-turn steering, and sends approval requests back to its
// client. That is the part of Codex needed for dispatch's interactive agent
// contract.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
)

const shareFilesHint = "You are operated through a chat channel. The human cannot open files on this machine. To show them a file you produced, write its absolute path on its own in your reply; dispatch uploads every mentioned path that exists. Files the human attaches are copied into this environment and their paths are listed at the end of that message."

// Agent implements agent.Agent for Codex app-server (Codex CLI 0.153+).
type Agent struct {
	Binary      string
	Credentials string // host auth.json lent to Docker environments; empty uses Codex's default
}

func New() *Agent                 { return &Agent{Binary: "codex"} }
func (a *Agent) Kind() agent.Kind { return agent.KindCodex }

func (a *Agent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	return a.start(ctx, env, def, "", prompt)
}

func (a *Agent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	if session == "" {
		return nil, fmt.Errorf("codex: resume requires a session id")
	}
	return a.start(ctx, env, def, session, prompt)
}

func (a *Agent) start(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	bin := a.Binary
	if bin == "" {
		bin = "codex"
	}
	hint := a.lendLogin(ctx, env, def)
	proc, err := env.Exec(ctx, bin, "app-server")
	if err != nil {
		return nil, fmt.Errorf("codex: exec app-server: %w", err)
	}
	r := &run{proc: proc, events: make(chan agent.Event, 64), pending: map[string]int64{}, prompt: prompt, resume: session, loginHint: hint, done: make(chan struct{})}
	if err := r.request(initRequest, "initialize", map[string]any{"clientInfo": map[string]any{"name": "dispatch", "version": "1"}}); err != nil {
		_ = proc.Kill()
		return nil, fmt.Errorf("codex: initialize: %w", err)
	}
	go r.drainStderr()
	go r.loop(def)
	return r, nil
}

const (
	initRequest   int64 = 1
	threadRequest int64 = 2
	turnRequest   int64 = 3
)

type run struct {
	proc                         environment.Process
	events                       chan agent.Event
	writeMu                      sync.Mutex
	mu                           sync.Mutex
	pending                      map[string]int64 // item id -> server request id
	thread, turn, prompt, resume string
	next                         int64
	stderr                       strings.Builder
	loginHint                    string
	done                         chan struct{}
}

func (r *run) Events() <-chan agent.Event { return r.events }

func mode(m agent.PermissionMode) (approval, sandbox string) {
	switch m {
	case agent.PermissionBypass:
		return "never", "danger-full-access"
	case agent.PermissionAcceptEdits, agent.PermissionAuto:
		return "on-request", "workspace-write"
	default:
		return "untrusted", "read-only"
	}
}
func threadParams(def agent.Definition) map[string]any {
	approval, sandbox := mode(def.PermissionMode)
	p := map[string]any{"approvalPolicy": approval, "sandbox": sandbox, "developerInstructions": strings.TrimSpace(shareFilesHint + "\n\n" + def.SystemPrompt), "serviceName": "dispatch"}
	if def.Model != "" {
		p["model"] = def.Model
	}
	return p
}
func turnParams(thread, prompt string) map[string]any {
	return map[string]any{"threadId": thread, "input": []map[string]string{{"type": "text", "text": prompt}}}
}

func (r *run) Send(ctx context.Context, text string) error {
	r.mu.Lock()
	thread, turn := r.thread, r.turn
	r.mu.Unlock()
	if thread == "" {
		return fmt.Errorf("codex: session is not ready")
	}
	method := "turn/start"
	p := turnParams(thread, text)
	if turn != "" {
		method = "turn/steer"
		p = map[string]any{"threadId": thread, "expectedTurnId": turn, "input": []map[string]string{{"type": "text", "text": text}}}
	}
	return r.request(r.id(), method, p)
}
func (r *run) Decide(ctx context.Context, d agent.PermissionDecision) error {
	r.mu.Lock()
	id, ok := r.pending[d.ToolID]
	if ok {
		delete(r.pending, d.ToolID)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("codex: no pending permission for tool id %q", d.ToolID)
	}
	decision := "decline"
	if d.Allow {
		decision = "accept"
	}
	return r.reply(id, map[string]any{"decision": decision})
}
func (r *run) Stop() error {
	r.mu.Lock()
	thread, turn := r.thread, r.turn
	r.mu.Unlock()
	if thread != "" && turn != "" {
		_ = r.request(r.id(), "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	}
	r.writeMu.Lock()
	_ = r.proc.Stdin().Close()
	r.writeMu.Unlock()
	select {
	case <-r.done:
		return nil
	case <-time.After(5 * time.Second):
		return r.proc.Kill()
	}
}
func (r *run) id() int64 { r.mu.Lock(); defer r.mu.Unlock(); r.next++; return turnRequest + r.next }
func (r *run) request(id int64, method string, params any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}
func (r *run) reply(id int64, result any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
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
			r.stderr.WriteString(sc.Text() + "\n")
		}
		r.mu.Unlock()
	}
}

func (r *run) loop(def agent.Definition) {
	defer close(r.events)
	defer close(r.done)
	sc := bufio.NewScanner(r.proc.Stdout())
	sc.Buffer(make([]byte, 0, 256*1024), 16<<20)
	sawEnd := false
	for sc.Scan() {
		raw := append([]byte(nil), sc.Bytes()...)
		var m message
		if err := json.Unmarshal(raw, &m); err != nil {
			r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: "codex: bad line: " + err.Error(), Raw: raw}
			continue
		}
		// A server-initiated approval is also a JSON-RPC request and so has
		// an id. Only messages without a method are responses to requests we
		// sent; requests continue into translate so the human can answer them.
		if m.ID != nil && m.Method == "" && m.Error == nil {
			switch *m.ID {
			case initRequest:
				_ = r.write(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
				if r.resume != "" {
					_ = r.request(threadRequest, "thread/resume", map[string]any{"threadId": r.resume})
				} else {
					_ = r.request(threadRequest, "thread/start", threadParams(def))
				}
			case threadRequest:
				var x struct {
					Thread struct {
						ID    string `json:"id"`
						Model string `json:"model"`
					} `json:"thread"`
				}
				_ = json.Unmarshal(m.Result, &x)
				if x.Thread.ID == "" {
					r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: "codex: thread start returned no id", Raw: raw}
					continue
				}
				r.mu.Lock()
				r.thread = x.Thread.ID
				r.mu.Unlock()
				model := x.Thread.Model
				if model == "" {
					model = def.Model
				}
				r.events <- agent.Event{Type: agent.EventInit, At: time.Now(), Session: x.Thread.ID, Model: model, Mode: def.PermissionMode, Billing: agent.BillingUnknown, Raw: raw}
				_ = r.request(turnRequest, "turn/start", turnParams(x.Thread.ID, r.prompt))
			}
			continue
		}
		if m.ID != nil && m.Method == "" && m.Error != nil {
			r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: r.errorText("codex: " + m.Error.Message), Raw: raw}
			sawEnd = true
			_ = r.proc.Kill() // a failed setup has no turn that could later end
			continue
		}
		for _, ev := range r.translate(m, raw) {
			if ev.Type == agent.EventResult || ev.Type == agent.EventError {
				sawEnd = true
			}
			r.events <- ev
		}
	}
	code, _ := r.proc.Wait()
	if code != 0 && !sawEnd {
		r.mu.Lock()
		tail := strings.TrimSpace(r.stderr.String())
		r.mu.Unlock()
		r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: r.errorText(fmt.Sprintf("codex exited with code %d: %s", code, tail))}
	}
}

func (r *run) errorText(text string) string {
	if r.loginHint != "" && strings.Contains(strings.ToLower(text), "login") {
		return text + r.loginHint
	}
	return text
}

type rpcError struct {
	Message string `json:"message"`
}
type message struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (r *run) translate(m message, raw []byte) []agent.Event {
	now := time.Now()
	base := agent.Event{At: now, Raw: raw}
	var out []agent.Event
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Item struct {
			ID               string         `json:"id"`
			Type             string         `json:"type"`
			Text             string         `json:"text"`
			Command          string         `json:"command"`
			Status           string         `json:"status"`
			AggregatedOutput string         `json:"aggregatedOutput"`
			Changes          map[string]any `json:"changes"`
		} `json:"item"`
		ItemID  string `json:"itemId"`
		Command string `json:"command"`
		Reason  string `json:"reason"`
		Delta   string `json:"delta"`
	}
	_ = json.Unmarshal(m.Params, &p)
	base.Session = p.ThreadID
	switch m.Method {
	case "turn/started":
		r.mu.Lock()
		r.turn = p.Turn.ID
		r.mu.Unlock()
	case "item/agentMessage/delta":
		out = append(out, event(base, agent.EventText, "", p.ItemID, p.Delta, true, nil))
	case "item/started":
		if p.Item.Type == "commandExecution" {
			out = append(out, event(base, agent.EventToolUse, agent.ToolBash, p.Item.ID, "", false, map[string]any{"command": p.Item.Command}))
		} else if p.Item.Type == "fileChange" {
			out = append(out, event(base, agent.EventToolUse, agent.ToolEdit, p.Item.ID, "", false, p.Item.Changes))
		}
	case "item/completed":
		switch p.Item.Type {
		case "commandExecution":
			out = append(out, event(base, agent.EventToolResult, agent.ToolBash, p.Item.ID, p.Item.AggregatedOutput, false, nil))
		case "fileChange":
			out = append(out, event(base, agent.EventToolResult, agent.ToolEdit, p.Item.ID, "", false, nil))
		case "agentMessage":
			if p.Item.Text != "" {
				out = append(out, event(base, agent.EventText, "", p.Item.ID, p.Item.Text, false, nil))
			}
		}
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		tool := agent.ToolBash
		input := map[string]any{"command": p.Command}
		if m.Method == "item/fileChange/requestApproval" {
			tool = agent.ToolEdit
			input = p.Item.Changes
		}
		r.mu.Lock()
		if m.ID != nil {
			r.pending[p.ItemID] = *m.ID
		}
		r.mu.Unlock()
		out = append(out, event(base, agent.EventNeedsPermission, tool, p.ItemID, p.Reason, false, input))
	case "turn/completed":
		r.mu.Lock()
		r.turn = ""
		r.mu.Unlock()
		if p.Turn.Error != nil {
			out = append(out, event(base, agent.EventError, "", "", r.errorText(p.Turn.Error.Message), false, nil))
		} else {
			out = append(out, event(base, agent.EventResult, "", "", "", false, nil))
		}
	}
	return out
}
func event(base agent.Event, typ agent.EventType, tool, id, text string, partial bool, input map[string]any) agent.Event {
	base.Type = typ
	base.Tool = tool
	base.ToolID = id
	base.Text = text
	base.Partial = partial
	base.ToolInput = input
	return base
}
