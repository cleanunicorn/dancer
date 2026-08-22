// Package coordinator is the long-running brain. It owns tasks: it turns
// surface intents into executor work, fans executor/agent events back out
// to every surface, relays permission decisions, and persists everything
// in the store so a restart can resume sessions.
//
//	transports --Inbound--> surfaces --Intent--> Coordinator --Task--> Executor
//	transports <--Outbound-- surfaces <--Event-- Coordinator <--agent.Event--
package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	"github.com/cleanunicorn/dancer/internal/executor"
	execlocal "github.com/cleanunicorn/dancer/internal/executor/local"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// Coordinator wires transports, surfaces, executor and store together.
type Coordinator struct {
	Store      store.Store
	Executor   executor.Executor
	Transports []transport.Transport
	Surfaces   []surface.Surface // offered inbound messages in this order
	Log        *slog.Logger
	// DefaultDefinition is used by RunTask intents without an agent name
	// (or whose agent name is unknown, in which case the name is treated
	// as part of the prompt).
	DefaultDefinition string
	// WorkdirRoot hosts per-task working directories for definitions that
	// do not pin one.
	WorkdirRoot string

	transports map[string]transport.Transport

	mu      sync.Mutex
	threads map[transport.ThreadID]executor.TaskID // live task per thread
	owner   map[executor.TaskID]string             // task -> surface that started it
	pending map[string]chan transport.Decision     // prompt base id -> waiter
}

// New returns a Coordinator.
func New(st store.Store, ex executor.Executor, transports []transport.Transport, surfaces []surface.Surface, log *slog.Logger) *Coordinator {
	if log == nil {
		log = slog.Default()
	}
	c := &Coordinator{
		Store: st, Executor: ex, Transports: transports, Surfaces: surfaces, Log: log,
		transports: map[string]transport.Transport{},
		threads:    map[transport.ThreadID]executor.TaskID{},
		owner:      map[executor.TaskID]string{},
		pending:    map[string]chan transport.Decision{},
	}
	for _, t := range transports {
		c.transports[t.Name()] = t
	}
	return c
}

// Run blocks until ctx is cancelled.
func (c *Coordinator) Run(ctx context.Context) error {
	for _, s := range c.Surfaces {
		if _, ok := c.transports[s.Transport()]; !ok {
			return fmt.Errorf("coordinator: surface %q bound to unknown transport %q", s.Name(), s.Transport())
		}
	}
	if err := c.recover(ctx); err != nil {
		return err
	}
	inbox := make(chan transport.Inbound, 64)
	var wg sync.WaitGroup
	for _, t := range c.Transports {
		wg.Add(1)
		go func(t transport.Transport) {
			defer wg.Done()
			if err := t.Run(ctx, inbox); err != nil && !errors.Is(err, context.Canceled) {
				c.Log.Error("transport stopped", "transport", t.Name(), "err", err)
			}
		}(t)
	}
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case in := <-inbox:
			c.handle(ctx, in)
		}
	}
}

// recover marks tasks that were live before a restart as idle; their
// sessions are resumable with the next message on the thread.
func (c *Coordinator) recover(ctx context.Context) error {
	for _, status := range []string{store.StatusRunning, store.StatusWaitingPermission, store.StatusQueued} {
		tasks, err := c.Store.ListTasks(ctx, status)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			t.Status = store.StatusIdle
			if t.Session == "" {
				t.Status = store.StatusFailed
			}
			if err := c.Store.PutTask(ctx, t); err != nil {
				return err
			}
			c.Log.Info("recovered task", "task", t.ID, "status", t.Status)
		}
	}
	return nil
}

// handle offers an inbound message to the surfaces on its transport and
// executes the intents of the first one that claims it.
func (c *Coordinator) handle(ctx context.Context, in transport.Inbound) {
	c.append(ctx, "", in.Thread, "inbound", in)
	for _, s := range c.Surfaces {
		if s.Transport() != in.Transport {
			continue
		}
		intents, ok := s.Handle(ctx, in)
		if !ok {
			continue
		}
		for _, it := range intents {
			c.execute(ctx, s, in, it)
		}
		return
	}
	c.Log.Debug("unclaimed inbound", "transport", in.Transport, "thread", in.Thread)
}

func (c *Coordinator) execute(ctx context.Context, s surface.Surface, in transport.Inbound, it surface.Intent) {
	switch it := it.(type) {
	case surface.Say:
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: it.Text}, s)
	case surface.Decide:
		c.resolveDecision(ctx, it)
	case surface.RunTask:
		c.runTask(ctx, s, it)
	case surface.FollowUp:
		c.followUp(ctx, s, it)
	case surface.Status:
		st, err := c.Store.LatestTaskForThread(ctx, it.Thread)
		if err != nil {
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no task on this thread"}, s)
			return
		}
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, TaskID: st.ID, Task: &st,
			Text: fmt.Sprintf("task `%s` — agent *%s* — status *%s* — session `%s`", st.ID, st.Definition.Name, st.Status, st.Session)}, s)
	case surface.Cancel:
		id, ok := c.lookup(it.Thread)
		if !ok {
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "nothing running on this thread"}, s)
			return
		}
		if err := c.Executor.Cancel(ctx, id); err != nil {
			c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, TaskID: id, Text: "cancel: " + err.Error()}, s)
		}
	case surface.ListAgents:
		defs, err := c.Store.ListDefinitions(ctx)
		if err != nil || len(defs) == 0 {
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no agent definitions"}, s)
			return
		}
		var b strings.Builder
		for _, d := range defs {
			fmt.Fprintf(&b, "• *%s* — %s/%s, env %s, mode %s\n", d.Name, d.Kind, d.Model, d.Environment.Kind, d.PermissionMode)
		}
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: strings.TrimRight(b.String(), "\n")}, s)
	}
}

func (c *Coordinator) runTask(ctx context.Context, s surface.Surface, it surface.RunTask) {
	def, err := c.Store.GetDefinition(ctx, it.Agent)
	prompt := it.Prompt
	if errors.Is(err, store.ErrNotFound) && c.DefaultDefinition != "" {
		def, err = c.Store.GetDefinition(ctx, c.DefaultDefinition)
		prompt = strings.TrimSpace(it.Agent + " " + it.Prompt)
	}
	if err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: fmt.Sprintf("unknown agent %q — try `agents`", it.Agent)}, s)
		return
	}
	if strings.TrimSpace(prompt) == "" {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "usage: `run <agent> <prompt>`"}, s)
		return
	}
	if _, busy := c.lookup(it.Thread); busy {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "a task is already running on this thread — `cancel` it first or reply to it"}, s)
		return
	}
	id := executor.TaskID(newID())
	if def.Environment.Kind == "" {
		def.Environment.Kind = environment.KindLocal
	}
	if def.Environment.Workdir == "" && c.WorkdirRoot != "" {
		def.Environment.Workdir = filepath.Join(c.WorkdirRoot, string(id))
	}
	st := store.TaskState{ID: id, Thread: it.Thread, Definition: def, Status: store.StatusQueued}
	if err := c.Store.PutTask(ctx, st); err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: "store: " + err.Error()}, s)
		return
	}
	c.bind(it.Thread, id, s.Name())
	c.broadcast(ctx, surface.Event{Kind: surface.EventStarted, Thread: it.Thread, TaskID: id, Task: &st})
	go c.drive(ctx, st, prompt)
}

// followUp routes a plain message to the thread's task, resuming if needed.
func (c *Coordinator) followUp(ctx context.Context, s surface.Surface, it surface.FollowUp) {
	id, ok := c.lookup(it.Thread)
	if ok {
		if err := c.Executor.Send(ctx, id, it.Text); err == nil {
			return
		} else if !errors.Is(err, execlocal.ErrNotRunning) {
			c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, TaskID: id, Text: "send: " + err.Error()}, s)
			return
		}
	}
	st, err := c.Store.LatestTaskForThread(ctx, it.Thread)
	if errors.Is(err, store.ErrNotFound) && c.DefaultDefinition != "" {
		// A fresh thread with plain text: start a task with the default agent.
		c.runTask(ctx, s, surface.RunTask{Thread: it.Thread, Agent: c.DefaultDefinition, Prompt: it.Text})
		return
	}
	if err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no task on this thread — say `help`"}, s)
		return
	}
	if st.Session == "" || st.Status == store.StatusRunning {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "task cannot be resumed — start a new one with `run`"}, s)
		return
	}
	c.bind(it.Thread, st.ID, s.Name())
	c.broadcast(ctx, surface.Event{Kind: surface.EventResumed, Thread: it.Thread, TaskID: st.ID, Task: &st})
	go c.drive(ctx, st, it.Text)
}

// drive runs one executor turn-loop for a task and records the outcome.
func (c *Coordinator) drive(ctx context.Context, st store.TaskState, prompt string) {
	st.Status = store.StatusRunning
	_ = c.Store.PutTask(ctx, st)
	sink := &taskSink{c: c, state: st}
	err := c.Executor.Run(ctx, executor.Task{ID: st.ID, Definition: st.Definition, Prompt: prompt, Session: st.Session}, sink)
	final := sink.snapshot()
	switch {
	case err != nil && errors.Is(err, context.Canceled):
		final.Status = store.StatusCancelled
	case err != nil:
		final.Status = store.StatusFailed
		c.broadcast(ctx, surface.Event{Kind: surface.EventError, Thread: st.Thread, TaskID: st.ID, Task: &final, Text: err.Error()})
	case final.Session != "":
		final.Status = store.StatusIdle
	default:
		final.Status = store.StatusDone
	}
	_ = c.Store.PutTask(ctx, final)
	c.unbind(st.Thread, st.ID)
	c.broadcast(ctx, surface.Event{Kind: surface.EventFinished, Thread: st.Thread, TaskID: st.ID, Task: &final})
}

// taskSink implements executor.Sink for one task.
type taskSink struct {
	c *Coordinator

	mu    sync.Mutex
	state store.TaskState
}

func (s *taskSink) snapshot() store.TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *taskSink) OnEvent(ctx context.Context, id executor.TaskID, ev agent.Event) {
	seq := s.c.append(ctx, id, s.state.Thread, "agent", ev)
	s.mu.Lock()
	s.state.LastSeq = seq
	if ev.Session != "" {
		s.state.Session = ev.Session
	}
	switch ev.Type {
	case agent.EventNeedsPermission:
		s.state.Status = store.StatusWaitingPermission
	case agent.EventResult, agent.EventError:
		s.state.Status = store.StatusIdle
	default:
		s.state.Status = store.StatusRunning
	}
	st := s.state
	s.mu.Unlock()
	_ = s.c.Store.PutTask(ctx, st)
	if ev.Type == agent.EventNeedsPermission {
		return // rendered by AwaitDecision with its prompt id
	}
	e := ev
	s.c.broadcast(ctx, surface.Event{Kind: surface.EventAgent, Thread: st.Thread, TaskID: id, Task: &st, Agent: &e})
}

func (s *taskSink) AwaitDecision(ctx context.Context, id executor.TaskID, ev agent.Event) (agent.PermissionDecision, error) {
	base := string(id) + ":" + ev.ToolID
	ch := make(chan transport.Decision, 1)
	s.c.mu.Lock()
	s.c.pending[base] = ch
	s.c.mu.Unlock()
	defer func() {
		s.c.mu.Lock()
		delete(s.c.pending, base)
		s.c.mu.Unlock()
	}()
	st := s.snapshot()
	e := ev
	s.c.broadcast(ctx, surface.Event{Kind: surface.EventPermission, Thread: st.Thread, TaskID: id, Task: &st, Agent: &e, PromptID: base})

	select {
	case d := <-ch:
		s.c.append(ctx, id, st.Thread, "decision", d)
		return agent.PermissionDecision{ToolID: ev.ToolID, Allow: d.Choice == "allow", Reason: "operator chose " + d.Choice}, nil
	case <-ctx.Done():
		return agent.PermissionDecision{}, ctx.Err()
	}
}

// resolveDecision delivers a Decide intent. Prompt ids are
// "<surface>:<task>:<tool>"; the surface prefix is stripped so any surface
// that rendered the prompt can answer it.
func (c *Coordinator) resolveDecision(ctx context.Context, d surface.Decide) {
	base := d.PromptID
	if i := strings.Index(base, ":"); i >= 0 {
		base = base[i+1:]
	}
	c.mu.Lock()
	ch, ok := c.pending[base]
	c.mu.Unlock()
	if !ok {
		c.Log.Warn("decision for unknown prompt", "prompt", d.PromptID)
		return
	}
	select {
	case ch <- transport.Decision{PromptID: d.PromptID, Choice: d.Choice}:
	default:
	}
}

// emit renders an event through one surface.
func (c *Coordinator) emit(ctx context.Context, ev surface.Event, s surface.Surface) {
	t := c.transports[s.Transport()]
	for _, out := range s.Render(ev) {
		c.append(ctx, ev.TaskID, out.Thread, "outbound", out)
		if err := t.Send(ctx, out); err != nil {
			c.Log.Error("send failed", "transport", t.Name(), "surface", s.Name(), "err", err)
		}
	}
}

// broadcast renders an event through every surface.
func (c *Coordinator) broadcast(ctx context.Context, ev surface.Event) {
	for _, s := range c.Surfaces {
		c.emit(ctx, ev, s)
	}
}

func (c *Coordinator) append(ctx context.Context, id executor.TaskID, th transport.ThreadID, kind string, v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	seq, err := c.Store.Append(ctx, store.Record{At: time.Now(), Task: id, Thread: th, Kind: kind, Payload: b})
	if err != nil {
		c.Log.Error("append failed", "err", err)
	}
	return seq
}

func (c *Coordinator) bind(th transport.ThreadID, id executor.TaskID, surfaceName string) {
	c.mu.Lock()
	c.threads[th] = id
	c.owner[id] = surfaceName
	c.mu.Unlock()
}

func (c *Coordinator) unbind(th transport.ThreadID, id executor.TaskID) {
	c.mu.Lock()
	if c.threads[th] == id {
		delete(c.threads, th)
	}
	delete(c.owner, id)
	c.mu.Unlock()
}

func (c *Coordinator) lookup(th transport.ThreadID) (executor.TaskID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.threads[th]
	return id, ok
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
