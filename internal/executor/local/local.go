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
	return &Executor{Agents: agents, Envs: envs, IdleTimeout: idle, running: map[executor.TaskID]*live{}}
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

	var run agent.Run
	if t.Session != "" {
		run, err = ag.Resume(runCtx, env, t.Definition, t.Session, t.Prompt)
	} else {
		run, err = ag.Start(runCtx, env, t.Definition, t.Prompt)
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

	events := run.Events()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			switch ev.Type {
			case agent.EventNeedsPermission, agent.EventQuestion:
				sink.OnEvent(ctx, t.ID, ev)
				go e.relayPermission(ctx, t.ID, ev, run, sink)
			case agent.EventResult, agent.EventError:
				sink.OnEvent(ctx, t.ID, ev)
				idle.Reset(e.IdleTimeout)
			default:
				sink.OnEvent(ctx, t.ID, ev)
			}
		case <-l.activity:
			idle.Stop()
		case <-idle.C:
			run.Stop()
			// Drain remaining events (the closed channel ends the loop).
			for ev := range events {
				sink.OnEvent(ctx, t.ID, ev)
			}
			return nil
		case <-runCtx.Done():
			run.Stop()
			return runCtx.Err()
		}
	}
}

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
