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
	// ChannelAgents overrides DefaultDefinition per channel, keyed by
	// "<transport>/<channel>" (the channel is the part of the thread id
	// before the first "/"). Read and updated under mu once Run started.
	ChannelAgents map[string]string
	// SaveChannelAgent persists a per-channel default set from chat (the
	// config file). Nil keeps it in memory only.
	SaveChannelAgent func(ctx context.Context, transportName, channel, agent string) error
	// WorkdirRoot hosts per-task working directories for definitions that
	// do not pin one.
	WorkdirRoot string
	// DrainTimeout bounds how long Run waits for live tasks to stop after
	// its context is cancelled (executors drain in-flight tool calls).
	DrainTimeout time.Duration
	// SaveDefinition persists a definition created from chat outside the
	// store (the config file), so it survives a re-seed on restart. Nil
	// keeps it in the store only.
	SaveDefinition func(ctx context.Context, d agent.Definition) error
	// AutoResume continues tasks that a restart cut short as soon as dancer
	// is back, instead of waiting for a message on their thread.
	AutoResume bool
	// ResumePrompt is what an auto-resumed session is told; empty uses
	// defaultResumePrompt.
	ResumePrompt string
	// AutoResumeWithin skips tasks last touched longer ago than this, so a
	// restart after a long stop does not relaunch stale work (default 12h).
	AutoResumeWithin time.Duration
	// MaxAutoResumes caps consecutive automatic resumes of one task, so a
	// task that keeps taking dancer down cannot restart-loop (default 3).
	MaxAutoResumes int

	drives sync.WaitGroup

	transports map[string]transport.Transport

	mu      sync.Mutex
	threads map[transport.ThreadID]executor.TaskID    // live task per thread
	owner   map[executor.TaskID]string                // task -> surface that started it
	pending map[string]chan transport.Decision        // prompt base id -> waiter
	askText map[transport.ThreadID]string             // thread -> prompt base id accepting a typed answer
	wizards map[transport.ThreadID]context.CancelFunc // open "add agent" flows
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
		askText:    map[transport.ThreadID]string{},
		wizards:    map[transport.ThreadID]context.CancelFunc{},
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
	c.seedThreads(ctx)
	c.resumeFlows(ctx)
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
			c.shutdown(ctx)
			wg.Wait()
			return ctx.Err()
		case in := <-inbox:
			c.handle(ctx, in)
		}
	}
}

// shutdown tells every live thread that dancer is restarting, then waits
// (bounded) for executors to drain and persist their final state.
func (c *Coordinator) shutdown(ctx context.Context) {
	timeout := c.DrainTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout+15*time.Second)
	defer cancel()

	c.mu.Lock()
	live := make(map[transport.ThreadID]executor.TaskID, len(c.threads))
	for th, id := range c.threads {
		live[th] = id
	}
	c.mu.Unlock()
	tail := "reply in this thread to resume"
	if c.AutoResume {
		tail = "it continues on its own when dancer is back"
	}
	for th, id := range live {
		st, err := c.Store.GetTask(sctx, id)
		if err != nil {
			continue
		}
		c.Log.Info("shutdown: notifying live task", "task", id, "thread", th)
		c.emitTo(sctx, st.Transport, surface.Event{Kind: surface.EventReply, Thread: th, TaskID: id, Task: &st,
			Text: "⏸️ dancer is restarting — the agent finishes its current step and stops; " + tail})
	}

	done := make(chan struct{})
	go func() { c.drives.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout + 10*time.Second):
		c.Log.Warn("shutdown: tasks still running after drain timeout")
	}
}

// seedThreads tells thread-tracking transports about every stored task
// thread, so replies in old threads are still forwarded after a restart.
func (c *Coordinator) seedThreads(ctx context.Context) {
	tasks, err := c.Store.ListTasks(ctx, "")
	if err != nil {
		c.Log.Error("seed threads", "err", err)
		return
	}
	n := 0
	for _, t := range tasks {
		for name, tr := range c.transports {
			if t.Transport != "" && t.Transport != name {
				continue
			}
			if tt, ok := tr.(transport.ThreadTracker); ok {
				tt.Remember(t.Thread)
				n++
			}
		}
	}
	c.Log.Info("seeded transport threads", "threads", n)
}

// defaultResumePrompt is the turn given to a session that a restart cut
// short. It has to stand on its own: the agent sees it as the next user
// message of the resumed session.
const defaultResumePrompt = "dancer restarted and cut your last turn short. Continue the work in progress from where it stopped, without waiting for further instructions. If the task was already finished, say so in one line instead of redoing it."

// recover picks up the tasks that a restart cut short. With AutoResume
// each of them continues on its own — resumed from its session, or started
// again when it never got one — so nobody has to type in the thread; the
// rest are marked idle and resume with the next message.
func (c *Coordinator) recover(ctx context.Context) error {
	var resume []store.TaskState
	for _, status := range []string{store.StatusRunning, store.StatusWaitingPermission, store.StatusQueued, store.StatusInterrupted} {
		tasks, err := c.Store.ListTasks(ctx, status)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			auto := c.autoResumable(t)
			switch {
			case auto:
				t.Status = store.StatusIdle
				t.Resumes++
			case t.Session == "":
				t.Status = store.StatusFailed
			default:
				t.Status = store.StatusIdle
			}
			if err := c.Store.PutTask(ctx, t); err != nil {
				return err
			}
			c.Log.Info("recovered task", "task", t.ID, "status", t.Status, "auto_resume", auto, "resumes", t.Resumes)
			tt := t
			switch {
			case auto:
				resume = append(resume, tt)
			case tt.Status == store.StatusIdle:
				c.emitTo(ctx, t.Transport, surface.Event{Kind: surface.EventReply, Thread: t.Thread, TaskID: t.ID, Task: &tt,
					Text: "▶️ dancer is back — reply in this thread to continue where the agent left off"})
			}
		}
	}
	for _, t := range resume {
		c.autoResume(ctx, t)
	}
	return nil
}

// autoResumable reports whether a task cut short by a restart may continue
// by itself: AutoResume is on, there is something to continue from (a
// session, or the prompt of a task that never started), it was touched
// recently enough, and it has not already been resumed too many times.
func (c *Coordinator) autoResumable(t store.TaskState) bool {
	if !c.AutoResume {
		return false
	}
	if t.Session == "" && strings.TrimSpace(t.Prompt) == "" {
		return false
	}
	max := c.MaxAutoResumes
	if max <= 0 {
		max = 3
	}
	if t.Resumes >= max {
		c.Log.Warn("task not auto-resumed: too many restarts", "task", t.ID, "resumes", t.Resumes)
		return false
	}
	within := c.AutoResumeWithin
	if within <= 0 {
		within = 12 * time.Hour
	}
	if !t.UpdatedAt.IsZero() && time.Since(t.UpdatedAt) > within {
		c.Log.Info("task not auto-resumed: too old", "task", t.ID, "age", time.Since(t.UpdatedAt).Round(time.Minute))
		return false
	}
	return true
}

// autoResume drives one recovered task without waiting for a message.
func (c *Coordinator) autoResume(ctx context.Context, t store.TaskState) {
	prompt, note := c.resumePrompt(), "▶️ dancer is back — picking up this task where the agent left off"
	if t.Session == "" {
		// The task never reached a session: run the original request again.
		prompt, note = t.Prompt, "▶️ dancer is back — this task never started, running it again"
	}
	if t.Definition.Environment.Workdir == "" && c.WorkdirRoot != "" {
		t.Definition.Environment.Workdir = filepath.Join(c.WorkdirRoot, string(t.ID))
	}
	c.emitTo(ctx, t.Transport, surface.Event{Kind: surface.EventReply, Thread: t.Thread, TaskID: t.ID, Task: &t, Text: note})
	c.bind(t.Thread, t.ID, c.surfaceOn(t.Transport))
	c.broadcast(ctx, surface.Event{Kind: surface.EventResumed, Thread: t.Thread, TaskID: t.ID, Task: &t})
	c.Log.Info("auto-resuming task", "task", t.ID, "thread", t.Thread, "session", t.Session)
	c.drives.Add(1)
	go c.drive(ctx, t, prompt)
}

func (c *Coordinator) resumePrompt() string {
	if strings.TrimSpace(c.ResumePrompt) != "" {
		return c.ResumePrompt
	}
	return defaultResumePrompt
}

// surfaceOn names a surface bound to a transport (for task ownership).
func (c *Coordinator) surfaceOn(transportName string) string {
	for _, s := range c.Surfaces {
		if transportName == "" || s.Transport() == transportName {
			return s.Name()
		}
	}
	return ""
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
		if c.cancelWizard(it.Thread) {
			return
		}
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
			fmt.Fprintf(&b, "• *%s* — %s", d.Name, describeDefinition(d))
			if d.Name == c.defaultAgent(s, it.Thread) {
				b.WriteString(" · _default here_")
			}
			b.WriteString("\n")
		}
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: strings.TrimRight(b.String(), "\n")}, s)
	case surface.AddAgent:
		c.addAgent(ctx, s, it)
	case surface.SetDefault:
		c.setDefault(ctx, s, it)
	}
}

func describeDefinition(d agent.Definition) string {
	return fmt.Sprintf("%s/%s, env %s, mode %s", d.Kind, d.Model, d.Environment.Kind, d.PermissionMode)
}

// channelOf returns the channel part of a thread id ("C123/1.0" → "C123").
func channelOf(th transport.ThreadID) string {
	ch, _, _ := strings.Cut(string(th), "/")
	return ch
}

func channelKey(s surface.Surface, th transport.ThreadID) string {
	return s.Transport() + "/" + channelOf(th)
}

// defaultAgent is the definition used on th when none is named: the
// channel's default if one is set, else DefaultDefinition.
func (c *Coordinator) defaultAgent(s surface.Surface, th transport.ThreadID) string {
	c.mu.Lock()
	name, ok := c.ChannelAgents[channelKey(s, th)]
	c.mu.Unlock()
	if ok && name != "" {
		return name
	}
	return c.DefaultDefinition
}

// setDefault shows or changes the default agent of a channel.
func (c *Coordinator) setDefault(ctx context.Context, s surface.Surface, it surface.SetDefault) {
	key := channelKey(s, it.Thread)
	if it.Agent == "" {
		c.mu.Lock()
		name, ok := c.ChannelAgents[key]
		c.mu.Unlock()
		switch {
		case ok && name != "":
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: fmt.Sprintf("default agent on this channel: *%s* — change it with `default <agent>`", name)}, s)
		case c.DefaultDefinition != "":
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: fmt.Sprintf("no default set for this channel; the global default *%s* is used — set one with `default <agent>`", c.DefaultDefinition)}, s)
		default:
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no default agent — set one with `default <agent>`"}, s)
		}
		return
	}
	def, err := c.Store.GetDefinition(ctx, it.Agent)
	if err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: fmt.Sprintf("unknown agent %q — try `agents`", it.Agent)}, s)
		return
	}
	if c.SaveChannelAgent != nil {
		if err := c.SaveChannelAgent(ctx, s.Transport(), channelOf(it.Thread), def.Name); err != nil {
			c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: "writing config: " + err.Error()}, s)
			return
		}
	}
	c.mu.Lock()
	if c.ChannelAgents == nil {
		c.ChannelAgents = map[string]string{}
	}
	c.ChannelAgents[key] = def.Name
	c.mu.Unlock()
	c.Log.Info("channel default set from chat", "channel", key, "agent", def.Name)
	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: fmt.Sprintf("✅ default agent on this channel is now *%s* — plain messages here run it", def.Name)}, s)
}

func (c *Coordinator) runTask(ctx context.Context, s surface.Surface, it surface.RunTask) {
	if _, busy := c.lookup(it.Thread); busy {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "a task is already running on this thread — `cancel` it first or reply to it"}, s)
		return
	}
	if c.wizardOpen(it.Thread) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "finish or `cancel` the questions on this thread first"}, s)
		return
	}
	if it.Agent == "" && strings.TrimSpace(it.Prompt) == "" {
		// Bare `run`: ask for the agent and the prompt on the thread.
		c.startPick(ctx, s, it.Thread, "")
		return
	}
	def, err := c.Store.GetDefinition(ctx, it.Agent)
	prompt := it.Prompt
	if fallback := c.defaultAgent(s, it.Thread); errors.Is(err, store.ErrNotFound) && fallback != "" {
		def, err = c.Store.GetDefinition(ctx, fallback)
		prompt = strings.TrimSpace(it.Agent + " " + it.Prompt)
	}
	if err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: fmt.Sprintf("unknown agent %q — try `agents`", it.Agent)}, s)
		return
	}
	if strings.TrimSpace(prompt) == "" {
		// `run <agent>` without a prompt: ask for it.
		c.startPick(ctx, s, it.Thread, def.Name)
		return
	}
	id := executor.TaskID(newID())
	if def.Environment.Kind == "" {
		def.Environment.Kind = environment.KindLocal
	}
	if def.Environment.Workdir == "" && c.WorkdirRoot != "" {
		def.Environment.Workdir = filepath.Join(c.WorkdirRoot, string(id))
	}
	st := store.TaskState{ID: id, Transport: s.Transport(), Thread: it.Thread, Definition: def, Status: store.StatusQueued}
	if err := c.Store.PutTask(ctx, st); err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: "store: " + err.Error()}, s)
		return
	}
	c.bind(it.Thread, id, s.Name())
	c.broadcast(ctx, surface.Event{Kind: surface.EventStarted, Thread: it.Thread, TaskID: id, Task: &st})
	c.drives.Add(1)
	go c.drive(ctx, st, prompt)
}

// followUp routes a plain message to the thread's task, resuming if needed.
// While a question is open on the thread, the text answers it instead.
func (c *Coordinator) followUp(ctx context.Context, s surface.Surface, it surface.FollowUp) {
	c.mu.Lock()
	base, asking := c.askText[it.Thread]
	c.mu.Unlock()
	if asking {
		c.deliver(base, transport.Decision{PromptID: base, Choice: it.Text})
		return
	}
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
	if def := c.defaultAgent(s, it.Thread); errors.Is(err, store.ErrNotFound) && def != "" {
		// A fresh thread with plain text: start a task with the channel's default agent.
		c.runTask(ctx, s, surface.RunTask{Thread: it.Thread, Agent: def, Prompt: it.Text})
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
	if st.Transport == "" {
		st.Transport = s.Transport()
	}
	c.bind(it.Thread, st.ID, s.Name())
	c.broadcast(ctx, surface.Event{Kind: surface.EventResumed, Thread: it.Thread, TaskID: st.ID, Task: &st})
	c.drives.Add(1)
	go c.drive(ctx, st, it.Text)
}

// drive runs one executor turn-loop for a task and records the outcome.
func (c *Coordinator) drive(ctx context.Context, st store.TaskState, prompt string) {
	defer c.drives.Done()
	st.Status = store.StatusRunning
	st.Prompt = prompt
	_ = c.Store.PutTask(ctx, st)
	sink := &taskSink{c: c, state: st}
	err := c.Executor.Run(ctx, executor.Task{ID: st.ID, Definition: st.Definition, Prompt: prompt, Session: st.Session}, sink)
	final := sink.snapshot()
	// The run may have ended because ctx was cancelled (shutdown); persist
	// and notify with a context that still works.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	switch {
	case err != nil && errors.Is(err, context.Canceled) && ctx.Err() != nil:
		// Shutdown: the session is resumable; recover() notifies the thread.
		final.Status = store.StatusInterrupted
		if final.Session == "" {
			final.Status = store.StatusFailed
		}
	case err != nil && errors.Is(err, context.Canceled):
		final.Status = store.StatusCancelled
	case err != nil:
		final.Status = store.StatusFailed
		c.broadcast(pctx, surface.Event{Kind: surface.EventError, Thread: st.Thread, TaskID: st.ID, Task: &final, Text: err.Error()})
	case final.Session != "":
		final.Status = store.StatusIdle
	default:
		final.Status = store.StatusDone
	}
	if err := c.Store.PutTask(pctx, final); err != nil {
		c.Log.Error("persist final task state", "task", st.ID, "err", err)
	}
	c.unbind(st.Thread, st.ID)
	if ctx.Err() == nil {
		c.broadcast(pctx, surface.Event{Kind: surface.EventFinished, Thread: st.Thread, TaskID: st.ID, Task: &final})
	}
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
		s.state.Resumes = 0 // the agent got through a turn; not a restart loop
	default:
		s.state.Status = store.StatusRunning
	}
	st := s.state
	s.mu.Unlock()
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	_ = s.c.Store.PutTask(pctx, st)
	cancel()
	if ev.Type == agent.EventNeedsPermission {
		return // rendered by AwaitDecision with its prompt id
	}
	e := ev
	s.c.broadcast(ctx, surface.Event{Kind: surface.EventAgent, Thread: st.Thread, TaskID: id, Task: &st, Agent: &e})
}

func (s *taskSink) AwaitDecision(ctx context.Context, id executor.TaskID, ev agent.Event) (agent.PermissionDecision, error) {
	if ev.Type == agent.EventQuestion {
		return s.awaitAnswers(ctx, id, ev)
	}
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

// awaitAnswers asks each question in turn and returns the answers as an
// allow decision. A button click (option value) or a typed reply answers.
func (s *taskSink) awaitAnswers(ctx context.Context, id executor.TaskID, ev agent.Event) (agent.PermissionDecision, error) {
	st := s.snapshot()
	answers := map[string]string{}
	for i := range ev.Questions {
		q := ev.Questions[i]
		base := fmt.Sprintf("%s:%s#%d", id, ev.ToolID, i)
		ch := make(chan transport.Decision, 1)
		s.c.mu.Lock()
		s.c.pending[base] = ch
		s.c.askText[st.Thread] = base
		s.c.mu.Unlock()

		e := ev
		s.c.broadcast(ctx, surface.Event{Kind: surface.EventQuestion, Thread: st.Thread, TaskID: id, Task: &st, Agent: &e, Question: &q, PromptID: base})

		var d transport.Decision
		select {
		case d = <-ch:
		case <-ctx.Done():
			s.c.clearAsk(st.Thread, base)
			return agent.PermissionDecision{}, ctx.Err()
		}
		s.c.clearAsk(st.Thread, base)
		s.c.append(ctx, id, st.Thread, "decision", d)
		answers[q.Text] = d.Choice
	}
	return agent.PermissionDecision{ToolID: ev.ToolID, Allow: true, Answers: answers}, nil
}

func (c *Coordinator) clearAsk(th transport.ThreadID, base string) {
	c.mu.Lock()
	delete(c.pending, base)
	if c.askText[th] == base {
		delete(c.askText, th)
	}
	c.mu.Unlock()
}

// resolveDecision delivers a Decide intent. Prompt ids are
// "<surface>:<task>:<tool>"; the surface prefix is stripped so any surface
// that rendered the prompt can answer it.
func (c *Coordinator) resolveDecision(ctx context.Context, d surface.Decide) {
	base := d.PromptID
	if i := strings.Index(base, ":"); i >= 0 {
		base = base[i+1:]
	}
	c.deliver(base, transport.Decision{PromptID: d.PromptID, Choice: d.Choice})
}

// deliver hands a decision to the waiter registered under base.
func (c *Coordinator) deliver(base string, d transport.Decision) {
	c.mu.Lock()
	ch, ok := c.pending[base]
	c.mu.Unlock()
	if !ok {
		c.Log.Warn("decision for unknown prompt", "prompt", d.PromptID)
		return
	}
	select {
	case ch <- d:
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

// emitTo renders an event through every surface bound to a transport
// (all surfaces when the transport is unknown).
func (c *Coordinator) emitTo(ctx context.Context, transportName string, ev surface.Event) {
	for _, s := range c.Surfaces {
		if transportName == "" || s.Transport() == transportName {
			c.emit(ctx, ev, s)
		}
	}
}

// broadcast renders an event through every surface.
func (c *Coordinator) broadcast(ctx context.Context, ev surface.Event) {
	for _, s := range c.Surfaces {
		c.emit(ctx, ev, s)
	}
}

// append writes a record to the log. It must not be lost during shutdown,
// so it ignores cancellation of ctx.
func (c *Coordinator) append(ctx context.Context, id executor.TaskID, th transport.ThreadID, kind string, v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
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

func (c *Coordinator) wizardOpen(th transport.ThreadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.wizards[th]
	return ok
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
