// Package local is an in-process executor: one goroutine per task, agents
// started through the registered agent.Agent and environment.Factory
// implementations.
package local

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	"github.com/cleanunicorn/dancer/internal/executor"
)

// ErrNotRunning is returned by Send/Cancel when the task has no live process.
var ErrNotRunning = errors.New("executor: task is not running")

// Executor implements executor.Executor in-process.
type Executor struct {
	Agents map[agent.Kind]agent.Agent
	Envs   map[environment.Kind]environment.Factory
	// IdleTimeout is how long a finished turn keeps its process alive waiting
	// for a follow-up before it is stopped (the session stays resumable).
	IdleTimeout time.Duration
	// DrainTimeout bounds how long a shutdown (parent context cancelled)
	// waits for in-flight tool calls to finish before stopping the agent.
	// Zero stops immediately. Cancel() always stops immediately.
	DrainTimeout time.Duration

	mu      sync.Mutex
	running map[executor.TaskID]*live
}

type live struct {
	run    agent.Run
	cancel context.CancelFunc
	// turnDone is signalled by the event loop after each result; Send
	// resets the idle timer through it.
	activity chan struct{}
}

// New returns an executor with the given registries.
func New(agents map[agent.Kind]agent.Agent, envs map[environment.Kind]environment.Factory, idle time.Duration) *Executor {
	if idle <= 0 {
		idle = 10 * time.Minute
	}
	return &Executor{Agents: agents, Envs: envs, IdleTimeout: idle, DrainTimeout: 2 * time.Minute, running: map[executor.TaskID]*live{}}
}

func (e *Executor) Run(ctx context.Context, t executor.Task, sink executor.Sink) error {
	ag, ok := e.Agents[t.Definition.Kind]
	if !ok {
		return fmt.Errorf("executor: unknown agent kind %q", t.Definition.Kind)
	}
	spec := t.Definition.Environment
	if spec.Kind == "" {
		spec.Kind = environment.KindLocal
	}
	factory, ok := e.Envs[spec.Kind]
	if !ok {
		return fmt.Errorf("executor: unknown environment kind %q", spec.Kind)
	}
	env, err := factory.New(spec)
	if err != nil {
		return err
	}
	if err := env.Start(ctx); err != nil {
		return fmt.Errorf("executor: start environment: %w", err)
	}
	defer env.Stop(context.Background())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The agent process must outlive runCtx so a shutdown can drain it;
	// its lifetime is managed explicitly through run.Stop().
	procCtx := context.WithoutCancel(runCtx)
	var run agent.Run
	if t.Session != "" {
		run, err = ag.Resume(procCtx, env, t.Definition, t.Session, t.Prompt)
	} else {
		run, err = ag.Start(procCtx, env, t.Definition, t.Prompt)
	}
	if err != nil {
		return err
	}
	l := &live{run: run, cancel: cancel, activity: make(chan struct{}, 1)}
	e.mu.Lock()
	e.running[t.ID] = l
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.running, t.ID)
		e.mu.Unlock()
	}()

	idle := time.NewTimer(e.IdleTimeout)
	idle.Stop()
	defer idle.Stop()

	inflight := map[string]bool{} // tool_use ids without a tool_result yet
	handle := func(ev agent.Event) {
		switch ev.Type {
		case agent.EventToolUse:
			inflight[ev.ToolID] = true
			sink.OnEvent(ctx, t.ID, ev)
		case agent.EventToolResult:
			delete(inflight, ev.ToolID)
			sink.OnEvent(ctx, t.ID, ev)
		case agent.EventNeedsPermission, agent.EventQuestion:
			sink.OnEvent(ctx, t.ID, ev)
			go e.relayPermission(ctx, t.ID, ev, run, sink)
		case agent.EventResult, agent.EventError:
			inflight = map[string]bool{}
			sink.OnEvent(ctx, t.ID, ev)
			idle.Reset(e.IdleTimeout)
		default:
			sink.OnEvent(ctx, t.ID, ev)
		}
	}
	stop := func() {
		run.Stop()
		for ev := range events(run) {
			sink.OnEvent(ctx, t.ID, ev)
		}
	}

	evs := run.Events()
	for {
		select {
		case ev, ok := <-evs:
			if !ok {
				return nil
			}
			handle(ev)
		case <-l.activity:
			idle.Stop()
		case <-idle.C:
			stop()
			return nil
		case <-runCtx.Done():
			// Shutdown (parent cancelled) drains in-flight tool calls;
			// an explicit Cancel stops at once.
			if ctx.Err() != nil && e.DrainTimeout > 0 && len(inflight) > 0 {
				drain := time.NewTimer(e.DrainTimeout)
				defer drain.Stop()
				for len(inflight) > 0 {
					select {
					case ev, ok := <-evs:
						if !ok {
							return runCtx.Err()
						}
						handle(ev)
					case <-drain.C:
						inflight = nil
					}
				}
			}
			stop()
			return runCtx.Err()
		}
	}
}

func events(run agent.Run) <-chan agent.Event { return run.Events() }

func (e *Executor) relayPermission(ctx context.Context, id executor.TaskID, ev agent.Event, run agent.Run, sink executor.Sink) {
	d, err := sink.AwaitDecision(ctx, id, ev)
	if err != nil {
		d = agent.PermissionDecision{ToolID: ev.ToolID, Allow: false, Reason: "no decision: " + err.Error()}
	}
	d.ToolID = ev.ToolID
	_ = run.Decide(ctx, d)
}

func (e *Executor) Send(ctx context.Context, id executor.TaskID, text string) error {
	e.mu.Lock()
	l, ok := e.running[id]
	e.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	select {
	case l.activity <- struct{}{}:
	default:
	}
	return l.run.Send(ctx, text)
}

func (e *Executor) Cancel(ctx context.Context, id executor.TaskID) error {
	e.mu.Lock()
	l, ok := e.running[id]
	e.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	l.cancel()
	return nil
}

// IsRunning reports whether the task has a live process.
func (e *Executor) IsRunning(id executor.TaskID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.running[id]
	return ok
}
