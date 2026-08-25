// Package local is an in-process executor: one goroutine per task, agents
// started through the registered agent.Agent and environment.Factory
// implementations.
//
// Files cross the environment boundary here, both ways. A path the agent
// mentions in its text is pulled out (CopyOut) and attached to the event;
// a file the human attached to a message (Task.Files, Send's files) is
// copied in (CopyIn) under InboxDir/<task id>/ before the message reaches
// the agent, and the message gets the list of paths appended, so the
// agent reads them from disk like anything else. Names are made path-safe
// and kept unique within one run; the directory is the same for every
// turn of a task, so a resumed session still finds what it was sent.
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	"github.com/cleanunicorn/dancer/internal/executor"
	"github.com/cleanunicorn/dancer/internal/gh"
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
	// MaxFileBytes caps a single attachment pulled from the environment
	// (default 20 MiB); MaxFilesPerEvent caps attachments per message (default 10).
	MaxFileBytes     int64
	MaxFilesPerEvent int
	// InboxDir is where attachments the human sends are put inside the
	// environment, one directory per task under it (default
	// /tmp/dancer/inbox). An absolute path, the same in every environment
	// kind: /tmp is writable in a container, over ssh and on the host.
	InboxDir string

	mu      sync.Mutex
	running map[executor.TaskID]*live
}

type live struct {
	run    agent.Run
	env    environment.Environment
	cancel context.CancelFunc
	// turnDone is signalled by the event loop after each result; Send
	// resets the idle timer through it.
	activity chan struct{}
	// staged is the set of names already handed out, to keep a second
	// image.png from overwriting the first.
	stageMu sync.Mutex
	staged  map[string]bool
	inbox   string // this task's attachment directory
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
	// A container has no GitHub login of its own; lend the host's before
	// the agent starts, so `gh` and `git push` work in it (internal/gh).
	gh.Lend(ctx, env, spec.Env)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	l := &live{env: env, cancel: cancel, activity: make(chan struct{}, 1), staged: map[string]bool{}, inbox: e.inbox(t.ID)}
	prompt, err := l.stage(ctx, t.Prompt, t.Files)
	if err != nil {
		return err
	}
	// The agent process must outlive runCtx so a shutdown can drain it;
	// its lifetime is managed explicitly through run.Stop().
	procCtx := context.WithoutCancel(runCtx)
	var run agent.Run
	if t.Session != "" {
		run, err = ag.Resume(procCtx, env, t.Definition, t.Session, prompt)
	} else {
		run, err = ag.Start(procCtx, env, t.Definition, prompt)
	}
	if err != nil {
		return err
	}
	l.run = run
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
	sent := map[string]bool{}     // attachments already delivered this run
	handle := func(ev agent.Event) {
		if (ev.Type == agent.EventText && !ev.Partial) || ev.Type == agent.EventResult {
			ev.Files = e.attachFiles(ctx, env, ev.Text, sent)
		}
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

// filePathRE finds file paths with a shareable extension in agent text:
// absolute (/tmp/shot.png), relative with a directory (out/report.pdf) or
// bare (shot.png). Surrounding markdown/backticks are tolerated. The
// trailing boundary is checked in code (pathEndsAt) rather than consumed
// by the pattern, so paths on adjacent lines or separated by one space
// are all found.
var filePathRE = regexp.MustCompile(`(?im)(?:^|[\s` + "`" + `("'\[<])((?:/|\./|~/)?(?:[\w.@-]+/)*[\w.@-]+\.(?:png|jpe?g|gif|webp|svg|pdf|mp4|webm|mov|txt|md|csv|json|log|html?|zip|tar\.gz|diff|patch))`)

// pathEndsAt reports whether a path match ending at end is not just the
// prefix of a longer token (shot.png.bak, shot.pngx, a/b.png/c).
func pathEndsAt(text string, end int) bool {
	if end >= len(text) {
		return true
	}
	c := text[end]
	switch {
	case c == '/' || c == '-' || c == '_' || c == '@':
		return false
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return false
	case c == '.':
		if end+1 < len(text) {
			n := text[end+1]
			return !(n >= '0' && n <= '9' || n >= 'a' && n <= 'z' || n >= 'A' && n <= 'Z' || n == '_')
		}
	}
	return true
}

// mentionedPaths returns the candidate file paths in text, in order.
func mentionedPaths(text string) []string {
	var out []string
	for _, m := range filePathRE.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[2], m[3]
		if pathEndsAt(text, end) {
			out = append(out, text[start:end])
		}
	}
	return out
}

// attachFiles pulls files the agent mentioned out of its environment.
func (e *Executor) attachFiles(ctx context.Context, env environment.Environment, text string, sent map[string]bool) []agent.File {
	if text == "" {
		return nil
	}
	maxBytes := e.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = 20 << 20
	}
	maxFiles := e.MaxFilesPerEvent
	if maxFiles <= 0 {
		maxFiles = 10
	}
	var out []agent.File
	for _, p := range mentionedPaths(text) {
		if sent[p] || len(out) >= maxFiles {
			continue
		}
		sent[p] = true
		r, err := env.CopyOut(ctx, p)
		if err != nil {
			continue // not a real file; the agent was just talking about it
		}
		data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
		r.Close()
		if err != nil || len(data) == 0 || int64(len(data)) > maxBytes {
			continue
		}
		out = append(out, agent.File{Name: path.Base(strings.TrimRight(p, "/")), Path: p, Data: data})
	}
	return out
}

func (e *Executor) relayPermission(ctx context.Context, id executor.TaskID, ev agent.Event, run agent.Run, sink executor.Sink) {
	d, err := sink.AwaitDecision(ctx, id, ev)
	if err != nil {
		d = agent.PermissionDecision{ToolID: ev.ToolID, Allow: false, Reason: "no decision: " + err.Error()}
	}
	d.ToolID = ev.ToolID
	_ = run.Decide(ctx, d)
}

func (e *Executor) Send(ctx context.Context, id executor.TaskID, text string, files []agent.File) error {
	e.mu.Lock()
	l, ok := e.running[id]
	e.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	text, err := l.stage(ctx, text, files)
	if err != nil {
		return err
	}
	select {
	case l.activity <- struct{}{}:
	default:
	}
	return l.run.Send(ctx, text)
}

// defaultInboxDir is where attachments land when InboxDir is unset.
const defaultInboxDir = "/tmp/dancer/inbox"

// inbox is the attachment directory of a task inside its environment.
func (e *Executor) inbox(id executor.TaskID) string {
	root := e.InboxDir
	if root == "" {
		root = defaultInboxDir
	}
	return path.Join(root, string(id))
}

// stage copies files into the task's inbox and returns text with their
// paths appended, so the agent knows what it was sent and where to find
// it. Without files the text is returned as is. A copy that fails is an
// error: the human sent a file, and a silently missing one would have the
// agent answer about something it never saw.
func (l *live) stage(ctx context.Context, text string, files []agent.File) (string, error) {
	if len(files) == 0 {
		return text, nil
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(text, "\n"))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString("Files attached to this message, copied from the chat into this environment:")
	for _, f := range files {
		dst := l.inbox + "/" + l.uniqueName(f.Name)
		if err := l.env.CopyIn(ctx, bytes.NewReader(f.Data), dst); err != nil {
			return "", fmt.Errorf("executor: copy attachment %q in: %w", f.Name, err)
		}
		fmt.Fprintf(&b, "\n- %s", dst)
	}
	return b.String(), nil
}

// uniqueName is a path-safe name for an attachment that no earlier
// attachment of this run has taken: the second image.png becomes
// image-2.png. It counts on the names handed out rather than on the ones
// asked for, so a rename cannot land on a name already in use — sending
// image.png, image-2.png and image.png again gives the last image-3.png.
func (l *live) uniqueName(name string) string {
	name = safeName(name)
	l.stageMu.Lock()
	defer l.stageMu.Unlock()
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	out := name
	for n := 2; l.staged[out]; n++ {
		out = fmt.Sprintf("%s-%d%s", stem, n, ext)
	}
	l.staged[out] = true
	return out
}

// safeName reduces an attachment's name to one base name of letters,
// digits, dots, dashes and underscores, so it needs no quoting wherever
// the agent uses it. Spaces and the rest become underscores.
func safeName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "file"
	}
	if len(out) > 100 {
		ext := path.Ext(out)
		if len(ext) > 20 {
			ext = ""
		}
		out = out[:100-len(ext)] + ext
	}
	return out
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
