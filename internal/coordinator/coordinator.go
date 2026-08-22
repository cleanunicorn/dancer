// Package coordinator is the long-running brain. It owns tasks: it turns
// surface intents into executor work, fans executor/agent events back out
// to every surface, relays permission decisions (or lets a decider answer
// the ones inside the operator's auto_allow ceiling — see decide.go), and
// persists everything
// in the store so a restart can resume sessions.
//
//	transports --Inbound--> surfaces --Intent--> Coordinator --Task--> Executor
//	transports <--Outbound-- surfaces <--Event-- Coordinator <--agent.Event--
//
// It also owns which conversations are still open: a closed thread (see
// CloseThread) is not re-seeded on the transport, not resumed on a restart
// and not spoken to, until a human brings work back to it.
//
// And it keeps humans told that a task is alive: every Heartbeat while a
// turn runs it broadcasts surface.EventHeartbeat (the chat surface's
// status line lives on it), and on a transport that can react it marks
// the thread's root message ⏳ while the agent works and ✋ while the
// agent waits for an answer, clearing the mark when the turn ends.
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
	"github.com/cleanunicorn/dancer/internal/decider"
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
	// UpdateDefinition persists a definition changed from chat outside the
	// store (the config file). Nil keeps the change in the store only.
	UpdateDefinition func(ctx context.Context, d agent.Definition) error
	// DeleteDefinition removes a definition deleted from chat outside the
	// store (the config file). Nil removes it from the store only.
	DeleteDefinition func(ctx context.Context, name string) error
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
	// Heartbeat is how often surfaces hear that a running turn is still
	// going (default 10s). Negative turns heartbeats off.
	Heartbeat time.Duration
	// Decider answers policy questions the rules alone answer bluntly (see
	// internal/decider). Nil is decider.Static: dancer's own rules, which
	// is also what every failure falls back to.
	Decider decider.Decider
	// DeciderUses lists the question kinds Decider may answer; other kinds
	// are answered statically. Empty allows none, so turning a decider on
	// is always a deliberate, per-kind step.
	DeciderUses []string
	// DeciderTimeout bounds one question (default 15s). A decision never
	// blocks dancer: on timeout the static answer wins.
	DeciderTimeout time.Duration
	// MaxDecisionsPerTask caps how many questions one task may cost before
	// it falls back to the rules for good (default 20).
	MaxDecisionsPerTask int
	// AutoAllow is the ceiling for permission decisions: tool patterns
	// ("Read", "Bash(go test:*)") a decider may approve without a human.
	// Empty — the default — means every prompt reaches a person, whatever
	// the decider thinks.
	AutoAllow []string

	drives sync.WaitGroup

	transports map[string]transport.Transport

	mu      sync.Mutex
	threads map[transport.ThreadID]executor.TaskID    // live task per thread
	owner   map[executor.TaskID]string                // task -> surface that started it
	pending map[string]chan transport.Decision        // prompt base id -> waiter
	askText map[transport.ThreadID]string             // thread -> prompt base id accepting a typed answer
	wizards map[transport.ThreadID]context.CancelFunc // open question flows (agent add/edit/delete, run picker)
	closed  map[transport.ThreadID]bool               // threads a human ended; projection of the store
	sinks   map[executor.TaskID]*taskSink             // live tasks, for follow-up heartbeats
	marks   map[transport.ThreadID]string             // reaction currently on a thread's root message
	outMu   map[transport.ThreadID]*sync.Mutex        // serializes render+send per thread (keyed messages need order)
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
		closed:     map[transport.ThreadID]bool{},
		sinks:      map[executor.TaskID]*taskSink{},
		marks:      map[transport.ThreadID]string{},
		outMu:      map[transport.ThreadID]*sync.Mutex{},
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
	if err := c.loadClosed(ctx); err != nil {
		return err
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
	for th, id := range live {
		st, err := c.Store.GetTask(sctx, id)
		if err != nil {
			continue
		}
		// A task whose turn is done is only holding its process open for a
		// follow-up; stopping it cuts nothing short.
		tail := "reply in this thread to resume"
		if c.AutoResume && st.Status != store.StatusIdle {
			tail = "it continues on its own when dancer is back"
		}
		c.Log.Info("shutdown: notifying live task", "task", id, "thread", th, "status", st.Status)
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
		if c.threadClosed(t.Thread) {
			continue // closed on purpose: stay deaf until someone speaks up
		}
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

// recover picks up the tasks that were mid-execution when dancer stopped:
// interrupted (the stop cut the turn short), running or waiting_permission
// (dancer died before it could write anything else), and queued (never
// started). A task that had finished its turn is idle and is left alone —
// it was waiting for a human, not for dancer. With AutoResume the picked-up
// tasks continue on their own — resumed from their session, or started
// again when they never got one — so nobody has to type in the thread; the
// rest are marked idle and resume with the next message.
func (c *Coordinator) recover(ctx context.Context) error {
	// Deciding happens before the transports are up, so it is dead time on
	// every start. One question is seconds; a crash that left twenty tasks
	// behind would be minutes of a silent bot. The whole of recovery gets
	// one budget, and the tasks past it are answered by the rules.
	dctx, cancelDecisions := context.WithTimeout(ctx, c.recoveryBudget())
	defer cancelDecisions()

	var resume []resumable
	for _, status := range []string{store.StatusRunning, store.StatusWaitingPermission, store.StatusQueued, store.StatusInterrupted} {
		tasks, err := c.Store.ListTasks(ctx, status)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if c.threadClosed(t.Thread) {
				// A closed thread has no listener: mark the task idle so it
				// is not reported as live, but neither resume nor announce it.
				t.Status = store.StatusIdle
				if err := c.Store.PutTask(ctx, t); err != nil {
					return err
				}
				c.Log.Info("recovered task on a closed thread", "task", t.ID, "thread", t.Thread)
				continue
			}
			c.unmark(ctx, t.Transport, t.Thread) // a mark the previous process left
			v := decider.Verdict{Action: actionWait}
			if c.autoResumable(t) {
				// The rules say this one may continue; the decider chooses
				// what actually happens to it, and may word the resume.
				v = c.decide(dctx, decider.Question{
					Kind: kindResume, Task: string(t.ID), Thread: string(t.Thread),
					Options: []string{actionContinue, actionWait, actionAsk, actionAbandon},
					Facts:   c.factsForResume(ctx, t),
					Static:  decider.Verdict{Action: actionContinue, Prompt: c.resumePrompt()},
				})
			}
			switch {
			case v.Action == actionContinue:
				t.Status = store.StatusIdle
				t.Resumes++
			case v.Action == actionAbandon:
				t.Status = store.StatusCancelled
			case v.Action == actionAsk:
				t.Status = store.StatusIdle // the question needs it pick-up-able
			case t.Session == "":
				t.Status = store.StatusFailed
			default:
				t.Status = store.StatusIdle
			}
			if err := c.Store.PutTask(ctx, t); err != nil {
				return err
			}
			c.Log.Info("recovered task", "task", t.ID, "status", t.Status, "action", v.Action, "resumes", t.Resumes)
			tt := t
			switch {
			case v.Action == actionContinue:
				resume = append(resume, resumable{task: tt, prompt: v.Prompt})
			case v.Action == actionAsk:
				c.askAboutResume(ctx, tt, v)
			case v.Action == actionAbandon:
				c.emitTo(ctx, t.Transport, surface.Event{Kind: surface.EventReply, Thread: t.Thread, TaskID: t.ID, Task: &tt,
					Text: "⏹️ dancer is back — leaving this task: " + reasonOr(v.Reason, "it is no longer worth continuing") +
						". " + capitalize(pickUpHint(tt))})
			case tt.Status == store.StatusIdle:
				text := "▶️ dancer is back — reply in this thread to continue where the agent left off"
				if v.Reason != "" {
					text = "▶️ dancer is back — " + v.Reason + "; reply in this thread to continue"
				}
				c.emitTo(ctx, t.Transport, surface.Event{Kind: surface.EventReply, Thread: t.Thread, TaskID: t.ID, Task: &tt, Text: text})
			}
		}
	}
	for _, r := range resume {
		c.autoResume(ctx, r.task, r.prompt)
	}
	return nil
}

// recoveryBudget bounds the time all of recovery may spend on decisions:
// four questions' worth, so a handful of tasks each get their answer and a
// wedged CLI cannot hold the bot offline.
func (c *Coordinator) recoveryBudget() time.Duration {
	per := c.DeciderTimeout
	if per <= 0 {
		per = 15 * time.Second
	}
	return 4 * per
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// resumable is a task recover() decided to pick up, with the turn it is to
// be given (empty = the configured resume prompt).
type resumable struct {
	task   store.TaskState
	prompt string
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
func (c *Coordinator) autoResume(ctx context.Context, t store.TaskState, decided string) {
	prompt, note := c.resumePrompt(), "▶️ dancer is back — picking up this task where the agent left off"
	if decided != "" {
		prompt = decided
	}
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
	// Addressing the bot lifts the transport's tombstone before the
	// coordinator sees the message — that is how reopening works. If the
	// intent did not actually reopen the thread (`close` again, `status`,
	// a wizard step), put the tombstone back so plain replies stay ignored.
	defer func() {
		if in.Thread != "" && c.threadClosed(in.Thread) {
			c.forget(s.Transport(), in.Thread)
		}
	}()
	switch it := it.(type) {
	case surface.Say:
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: it.Text}, s)
	case surface.Decide:
		c.resolveDecision(ctx, it)
	case surface.RunTask:
		c.reopenThread(ctx, s, it.Thread)
		c.runTask(ctx, s, it)
	case surface.FollowUp:
		c.reopenThread(ctx, s, it.Thread)
		c.followUp(ctx, s, it)
	case surface.Status:
		st, err := c.Store.LatestTaskForThread(ctx, it.Thread)
		if err != nil {
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no task on this thread"}, s)
			return
		}
		text := fmt.Sprintf("task `%s` — agent *%s* — status *%s* — session `%s`", st.ID, st.Definition.Name, st.Status, st.Session)
		if v, ok := c.lastVerdict(ctx, st); ok && v.By != "" {
			text += fmt.Sprintf("\n· last decision: *%s* by %s", v.Action, v.By)
			if v.Reason != "" {
				text += " — " + v.Reason
			}
		}
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, TaskID: st.ID, Task: &st, Text: text}, s)
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
	case surface.CloseThread:
		c.closeThread(ctx, s, it)
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
	case surface.EditAgent:
		c.editAgent(ctx, s, it)
	case surface.DeleteAgent:
		c.deleteAgent(ctx, s, it)
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

// loadClosed reads the closed threads into memory once at startup; every
// later change goes through closeThread/reopenThread, which write both.
func (c *Coordinator) loadClosed(ctx context.Context) error {
	threads, err := c.Store.ClosedThreads(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	for _, th := range threads {
		c.closed[th] = true
	}
	n := len(c.closed)
	c.mu.Unlock()
	c.Log.Info("loaded closed threads", "threads", n)
	return nil
}

func (c *Coordinator) threadClosed(th transport.ThreadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed[th]
}

func (c *Coordinator) setClosed(th transport.ThreadID, closed bool) {
	c.mu.Lock()
	if closed {
		c.closed[th] = true
	} else {
		delete(c.closed, th)
	}
	c.mu.Unlock()
}

// closeThread ends the conversation on a thread: the open wizard and the
// running task are stopped, the thread is marked closed, and the transport
// is told to stop following it. The order matters — the notice and the
// reaction go out while the transport still follows the thread.
func (c *Coordinator) closeThread(ctx context.Context, s surface.Surface, it surface.CloseThread) {
	if c.threadClosed(it.Thread) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "this thread is already closed"}, s)
		return
	}
	c.cancelWizard(it.Thread)
	id, running := c.lookup(it.Thread)
	if running {
		if err := c.Executor.Cancel(ctx, id); err != nil && !errors.Is(err, execlocal.ErrNotRunning) {
			c.Log.Warn("close: cancel failed", "task", id, "err", err)
		}
		c.awaitStopped(it.Thread, id)
	}
	if err := c.Store.SetThreadClosed(ctx, it.Thread, true); err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: "close: " + err.Error()}, s)
		return
	}
	c.setClosed(it.Thread, true)
	c.append(ctx, id, it.Thread, "closed", map[string]any{"thread": it.Thread, "task": id})
	c.Log.Info("thread closed", "thread", it.Thread, "task", id, "was_running", running)
	c.emit(ctx, surface.Event{Kind: surface.EventClosed, Thread: it.Thread, TaskID: id}, s)
	c.markClosed(ctx, s.Transport(), it.Thread)
	c.forget(s.Transport(), it.Thread)
}

// awaitStopped waits (briefly) for the cancelled task to let go of the
// thread. Without it a message that reopens the thread right away would be
// sent into the dying process, and the unbind the finishing task does last
// would drop the reopened conversation's own binding.
func (c *Coordinator) awaitStopped(th transport.ThreadID, id executor.TaskID) {
	deadline := time.Now().Add(closeDrainTimeout)
	for time.Now().Before(deadline) {
		if cur, ok := c.lookup(th); !ok || cur != id {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.Log.Warn("close: task did not stop in time, unbinding it", "task", id, "thread", th)
	c.unbind(th, id)
}

// closeDrainTimeout bounds how long close waits for a cancelled task.
const closeDrainTimeout = 3 * time.Second

// reopenThread lifts the closed mark when a human brings work back to the
// thread. Nothing happens on a thread that was never closed.
func (c *Coordinator) reopenThread(ctx context.Context, s surface.Surface, th transport.ThreadID) {
	if !c.threadClosed(th) {
		return
	}
	if err := c.Store.SetThreadClosed(ctx, th, false); err != nil {
		c.Log.Error("reopen thread", "thread", th, "err", err)
		return
	}
	c.setClosed(th, false)
	c.Log.Info("thread reopened", "thread", th)
	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: "♻️ thread reopened"}, s)
}

// markClosed puts a ✅ on the thread's root message. Best-effort: the
// transport may not support reactions or lack the permission.
func (c *Coordinator) markClosed(ctx context.Context, transportName string, th transport.ThreadID) {
	r, ok := c.transports[transportName].(transport.Reactor)
	if !ok {
		return
	}
	if err := r.React(ctx, th, closedReaction); err != nil {
		c.Log.Warn("close: reaction failed (needs reactions:write scope?)", "thread", th, "err", err)
	}
}

// forget tells a thread-tracking transport to stop following a thread.
func (c *Coordinator) forget(transportName string, th transport.ThreadID) {
	if tc, ok := c.transports[transportName].(transport.ThreadCloser); ok {
		tc.Forget(th)
	}
}

// closedReaction marks a closed thread's root message.
const closedReaction = "white_check_mark"

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
	def.Environment = c.resolveEnv(def.Environment, def.Name, string(it.Thread), string(id))
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

// resolveEnv fills in the parts of a Spec that only the coordinator knows:
// which environment a task shares, and where its working directory goes.
//
// A reused environment has to keep the same workdir as well as the same
// container — the bind mount is fixed when the container is created — so the
// scope decides the directory name too.
func (c *Coordinator) resolveEnv(spec environment.Spec, agentName, thread, taskID string) environment.Spec {
	switch spec.Reuse {
	case environment.ReuseThread:
		spec.ReuseKey = thread
	case environment.ReuseDefinition:
		spec.ReuseKey = agentName
	default:
		spec.ReuseKey = ""
	}
	if spec.Workdir == "" && c.WorkdirRoot != "" {
		name := taskID
		if spec.ReuseKey != "" {
			name = dirName(spec.ReuseKey)
		}
		spec.Workdir = filepath.Join(c.WorkdirRoot, name)
	}
	return spec
}

// dirName turns a reuse key (a Slack "C123/1700.5" thread id, an agent name)
// into one path-safe directory name.
func dirName(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "shared"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
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
			c.wake(ctx, id)
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
	c.mu.Lock()
	c.sinks[st.ID] = sink
	c.mu.Unlock()
	c.mark(ctx, st.Transport, st.Thread, store.StatusRunning)
	stopBeat := c.beat(ctx, sink)
	err := c.Executor.Run(ctx, executor.Task{ID: st.ID, Definition: st.Definition, Prompt: prompt, Session: st.Session}, sink)
	stopBeat()
	c.mu.Lock()
	delete(c.sinks, st.ID)
	c.mu.Unlock()
	final := sink.snapshot()
	// The run may have ended because ctx was cancelled (shutdown); persist
	// and notify with a context that still works.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	switch {
	case err != nil && errors.Is(err, context.Canceled) && ctx.Err() != nil:
		// Shutdown. Only a turn that was still going is "interrupted" —
		// that is what recover() picks up again. A process that had
		// finished its turn and was only being kept alive for a follow-up
		// stays idle: nothing was cut short, so nothing has to continue.
		switch {
		case final.Session == "":
			final.Status = store.StatusFailed
		case sink.turnFinished():
			final.Status = store.StatusIdle
		default:
			final.Status = store.StatusInterrupted
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
	c.mark(pctx, st.Transport, st.Thread, final.Status)
	// The last heartbeat takes down whatever liveness display a surface
	// still has up — on a shutdown there is no finished event to do it.
	c.broadcast(pctx, heartbeat(final))
	c.unbind(st.Thread, st.ID)
	if ctx.Err() == nil {
		c.broadcast(pctx, surface.Event{Kind: surface.EventFinished, Thread: st.Thread, TaskID: st.ID, Task: &final})
	}
}

// heartbeat is the event that says where a task stands right now.
func heartbeat(st store.TaskState) surface.Event {
	return surface.Event{Kind: surface.EventHeartbeat, Thread: st.Thread, TaskID: st.ID, Task: &st}
}

// beat broadcasts a heartbeat every Heartbeat while the task is running,
// until the returned stop is called.
func (c *Coordinator) beat(ctx context.Context, sink *taskSink) (stop func()) {
	every := c.Heartbeat
	if every == 0 {
		every = defaultHeartbeat
	}
	if every < 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if st := sink.snapshot(); st.Status == store.StatusRunning {
					c.broadcast(ctx, heartbeat(st))
				}
			}
		}
	}()
	return func() { close(done) }
}

// defaultHeartbeat is the heartbeat period when Heartbeat is unset.
const defaultHeartbeat = 10 * time.Second

// wake records that a live task got a follow-up: it is running again
// before its first event says so, and surfaces hear it right away.
func (c *Coordinator) wake(ctx context.Context, id executor.TaskID) {
	c.mu.Lock()
	sink := c.sinks[id]
	c.mu.Unlock()
	if sink == nil {
		return
	}
	st, changed := sink.setStatus(ctx, store.StatusIdle, store.StatusRunning)
	if !changed {
		return
	}
	c.mark(ctx, st.Transport, st.Thread, st.Status)
	c.broadcast(ctx, heartbeat(st))
}

// mark shows a task's state on its thread's root message: ⏳ while the
// agent works, ✋ while it waits for a human, nothing otherwise. Best
// effort, on transports that can react; a mark is only touched when the
// state changes.
func (c *Coordinator) mark(ctx context.Context, transportName string, th transport.ThreadID, status string) {
	r, ok := c.transports[transportName].(transport.Reactor)
	if !ok {
		return
	}
	want := ""
	switch status {
	case store.StatusRunning:
		want = workingReaction
	case store.StatusWaitingPermission:
		want = waitingReaction
	}
	c.mu.Lock()
	have := c.marks[th]
	if have == want {
		c.mu.Unlock()
		return
	}
	c.marks[th] = want
	c.mu.Unlock()
	if have != "" {
		if err := r.Unreact(ctx, th, have); err != nil {
			c.Log.Warn("mark: removing reaction failed", "thread", th, "emoji", have, "err", err)
		}
	}
	if want != "" {
		if err := r.React(ctx, th, want); err != nil {
			c.Log.Warn("mark: reaction failed (needs reactions:write scope?)", "thread", th, "emoji", want, "err", err)
		}
	}
}

// unmark removes the state marks a previous dancer process may have left
// on a thread it was working in when it stopped.
func (c *Coordinator) unmark(ctx context.Context, transportName string, th transport.ThreadID) {
	r, ok := c.transports[transportName].(transport.Reactor)
	if !ok {
		return
	}
	for _, emoji := range []string{workingReaction, waitingReaction} {
		if err := r.Unreact(ctx, th, emoji); err != nil {
			c.Log.Debug("unmark: removing reaction failed", "thread", th, "emoji", emoji, "err", err)
		}
	}
}

const (
	workingReaction = "hourglass_flowing_sand" // the agent is working
	waitingReaction = "raised_hand"            // the agent waits for a human
)

// taskSink implements executor.Sink for one task.
type taskSink struct {
	c *Coordinator

	// putMu orders writes of the projection: it is taken before mu by
	// everything that changes state and held until the change is stored,
	// so a change made later cannot be written earlier. OnEvent runs on
	// the executor's goroutine, setStatus on whichever delivered the
	// follow-up or decision.
	putMu sync.Mutex
	mu    sync.Mutex
	state store.TaskState
	// answered is true while the agent's last word was a completed turn
	// that arrived before any shutdown. Events seen while the context is
	// already cancelled are the stop's own fallout (an agent that is being
	// drained still reports a result as it exits) and do not count, which
	// is what keeps a cut-short turn from looking finished.
	answered bool
}

func (s *taskSink) snapshot() store.TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// setStatus records a status change that did not come from the agent (a
// follow-up sent, a decision delivered) and persists it. It only applies
// when the task is still in the state from: the agent may already have
// moved on (a denied tool can end the turn at once), and its word wins.
func (s *taskSink) setStatus(ctx context.Context, from, to string) (st store.TaskState, changed bool) {
	s.putMu.Lock()
	defer s.putMu.Unlock()
	s.mu.Lock()
	if s.state.Status != from {
		st = s.state
		s.mu.Unlock()
		return st, false
	}
	s.state.Status = to
	if to == store.StatusRunning {
		s.answered = false
	}
	st = s.state
	s.mu.Unlock()
	s.persist(ctx, st)
	return st, true
}

// persist writes the projection; the caller holds putMu.
func (s *taskSink) persist(ctx context.Context, st store.TaskState) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = s.c.Store.PutTask(pctx, st)
}

// turnFinished reports whether the agent completed its turn before the stop.
func (s *taskSink) turnFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answered
}

func (s *taskSink) OnEvent(ctx context.Context, id executor.TaskID, ev agent.Event) {
	seq := s.c.append(ctx, id, s.state.Thread, "agent", ev)
	s.putMu.Lock()
	s.mu.Lock()
	s.state.LastSeq = seq
	if ev.Session != "" {
		s.state.Session = ev.Session
	}
	switch ev.Type {
	case agent.EventNeedsPermission, agent.EventQuestion:
		s.state.Status = store.StatusWaitingPermission
		s.answered = false
	case agent.EventResult, agent.EventError:
		s.state.Status = store.StatusIdle
		s.answered = ctx.Err() == nil // false while draining a stop
		if s.answered {
			s.state.Resumes = 0 // got through a turn; not a restart loop
		}
	default:
		s.state.Status = store.StatusRunning
		s.answered = false
	}
	st := s.state
	s.mu.Unlock()
	s.persist(ctx, st)
	s.putMu.Unlock()
	s.c.mark(ctx, st.Transport, st.Thread, st.Status)
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
	st0 := s.snapshot()
	if v, ok := s.c.decidePermission(ctx, st0, ev); ok {
		s.c.noteAutoAllowed(ctx, st0, ev, v)
		return agent.PermissionDecision{ToolID: ev.ToolID, Allow: true, Reason: "decider: " + v.Reason}, nil
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
		s.resume(ctx)
		return agent.PermissionDecision{ToolID: ev.ToolID, Allow: d.Choice == "allow", Reason: "operator chose " + d.Choice}, nil
	case <-ctx.Done():
		return agent.PermissionDecision{}, ctx.Err()
	}
}

// resume records that the human answered and the agent is working again,
// and tells surfaces so their liveness display comes back.
func (s *taskSink) resume(ctx context.Context) {
	st, changed := s.setStatus(ctx, store.StatusWaitingPermission, store.StatusRunning)
	if !changed {
		return
	}
	s.c.mark(ctx, st.Transport, st.Thread, st.Status)
	s.c.broadcast(ctx, heartbeat(st))
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
	s.resume(ctx)
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

// emit renders an event through one surface. Render and send happen under
// the thread's lock: a surface that edits a keyed message in place relies
// on its messages reaching the transport in the order it rendered them,
// and a heartbeat and an agent event can arrive from different goroutines.
func (c *Coordinator) emit(ctx context.Context, ev surface.Event, s surface.Surface) {
	mu := c.threadLock(ev.Thread)
	mu.Lock()
	defer mu.Unlock()
	t := c.transports[s.Transport()]
	for _, out := range s.Render(ev) {
		if ev.Kind != surface.EventHeartbeat {
			c.append(ctx, ev.TaskID, out.Thread, "outbound", out)
		}
		if err := t.Send(ctx, out); err != nil {
			c.Log.Error("send failed", "transport", t.Name(), "surface", s.Name(), "err", err)
		}
	}
}

// threadLock returns the lock that orders output on th.
func (c *Coordinator) threadLock(th transport.ThreadID) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	mu, ok := c.outMu[th]
	if !ok {
		mu = &sync.Mutex{}
		c.outMu[th] = mu
	}
	return mu
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
