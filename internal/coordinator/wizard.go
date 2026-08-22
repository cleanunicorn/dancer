package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// The "add agent" flow asks for one setting at a time on the thread that
// requested it, reusing the question machinery agents use: every step is
// an EventQuestion with a prompt id, answered by a button or a typed reply.

// wizardTimeout is how long one question waits for an answer.
const wizardTimeout = 15 * time.Minute

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var errWizardCancelled = errors.New("cancelled")

// toolPresets are the button choices of the tools step.
var toolPresets = []struct {
	label, desc string
	tools       []string
}{
	{"Read-only", "Read, Glob, Grep", []string{"Read", "Glob", "Grep"}},
	{"Edit files", "Read, Glob, Grep, Edit, Write", []string{"Read", "Glob", "Grep", "Edit", "Write"}},
	{"Edit + git", "…plus Bash(git:*)", []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git:*)"}},
	{"None", "ask for every tool", nil},
}

// addAgent starts the flow on a thread.
func (c *Coordinator) addAgent(ctx context.Context, s surface.Surface, it surface.AddAgent) {
	if _, busy := c.lookup(it.Thread); busy {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "a task is running on this thread — start `add agent` in a new thread"}, s)
		return
	}
	if c.wizardOpen(it.Thread) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "already adding an agent on this thread — answer the question above or `cancel`"}, s)
		return
	}
	c.startFlow(ctx, s, it.Thread, flowAddAgent, nil, func(ctx context.Context, w *wizard) (string, error) {
		def, err := w.run(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("✅ agent *%s* saved — try `run %s <prompt>`", def.Name, def.Name), nil
	})
}

// startPick runs the `run` picker on a thread: choose the agent (unless
// given), then type the prompt, then start the task.
func (c *Coordinator) startPick(ctx context.Context, s surface.Surface, th transport.ThreadID, agentName string) {
	c.startFlow(ctx, s, th, flowRun, nil, func(ctx context.Context, w *wizard) (string, error) {
		name := agentName
		if name == "" {
			defs, err := w.c.Store.ListDefinitions(ctx)
			if err != nil {
				return "", err
			}
			if len(defs) == 0 {
				return "", errors.New("no agent definitions — say `add agent` first")
			}
			q := agent.Question{Header: "Run", Text: "Which agent?"}
			def := w.c.defaultAgent(w.s, th)
			for _, d := range defs {
				desc := describeDefinition(d)
				if d.Name == def {
					desc = "default here · " + desc
				}
				q.Options = append(q.Options, agent.Option{Label: d.Name, Description: desc})
			}
			name, err = w.askUntil(ctx, q, func(a string) (string, error) {
				for _, d := range defs {
					if strings.EqualFold(a, d.Name) {
						return d.Name, nil
					}
				}
				return "", fmt.Errorf("no agent named %q", a)
			})
			if err != nil {
				return "", err
			}
		}
		prompt, err := w.askUntil(ctx, agent.Question{Header: "Prompt", Text: fmt.Sprintf("What should *%s* do? Reply in this thread.", name)}, nonEmpty("prompt"))
		if err != nil {
			return "", err
		}
		w.then = func(ctx context.Context) {
			w.c.runTask(ctx, w.s, surface.RunTask{Thread: th, Agent: name, Prompt: prompt})
		}
		return "", nil
	})
}

// resumeFlows restarts flows that were open before a restart: the saved
// answers are replayed silently and the next question is asked again.
func (c *Coordinator) resumeFlows(ctx context.Context) {
	flows, err := c.Store.ListFlows(ctx)
	if err != nil {
		c.Log.Error("list flows", "err", err)
		return
	}
	for _, f := range flows {
		var s surface.Surface
		for _, cand := range c.Surfaces {
			if cand.Name() == f.Surface {
				s = cand
			}
		}
		if s == nil || f.Kind != flowAddAgent {
			c.Log.Warn("dropping flow without a surface", "thread", f.Thread, "surface", f.Surface, "kind", f.Kind)
			_ = c.Store.DeleteFlow(ctx, f.Thread)
			continue
		}
		c.Log.Info("resuming flow", "thread", f.Thread, "answers", len(f.Answers))
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: f.Thread, Text: "▶️ dancer is back — continuing with the agent questions"}, s)
		c.startWizard(ctx, s, f.Thread, f.Answers)
	}
}

// Flow kinds. Only add_agent is persisted and resumed after a restart; the
// run picker is short-lived and simply asked again.
const (
	flowAddAgent = "add_agent"
	flowRun      = "run"
)

// flowLabel names a flow in messages.
func flowLabel(kind string) string {
	if kind == flowAddAgent {
		return "add agent"
	}
	return kind
}

// startWizard resumes an add-agent flow with saved answers.
func (c *Coordinator) startWizard(ctx context.Context, s surface.Surface, th transport.ThreadID, answers []string) {
	c.startFlow(ctx, s, th, flowAddAgent, answers, func(ctx context.Context, w *wizard) (string, error) {
		def, err := w.run(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("✅ agent *%s* saved — try `run %s <prompt>`", def.Name, def.Name), nil
	})
}

// startFlow runs a question flow in its own goroutine; one per thread.
// answers are replayed before any new question is asked. run returns the
// success message (empty posts nothing); w.then, if set by run, executes
// after the flow is unregistered from the thread.
func (c *Coordinator) startFlow(ctx context.Context, s surface.Surface, th transport.ThreadID, kind string, answers []string, run func(context.Context, *wizard) (string, error)) {
	wctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.wizards[th] = cancel
	c.mu.Unlock()

	w := &wizard{c: c, s: s, thread: th, kind: kind, id: newID(), parent: ctx, answers: answers}
	c.drives.Add(1)
	go func() {
		defer c.drives.Done()
		done, err := run(wctx, w)
		cancel()
		c.mu.Lock()
		delete(c.wizards, th)
		c.mu.Unlock()
		// Report with a context that survives shutdown.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer rcancel()
		label := flowLabel(kind)
		if ctx.Err() != nil {
			if kind == flowAddAgent {
				// Shutdown: keep the saved answers; resumeFlows continues after restart.
				w.say(rctx, "⏸️ dancer is restarting — the agent questions continue when it is back")
			} else {
				w.say(rctx, fmt.Sprintf("⏸️ dancer is restarting — say `%s` again when it is back", label))
			}
			return
		}
		if kind == flowAddAgent {
			_ = c.Store.DeleteFlow(rctx, th)
		}
		switch {
		case errors.Is(err, errWizardCancelled):
			w.say(rctx, fmt.Sprintf("⏹️ %s cancelled", label))
		case errors.Is(err, context.DeadlineExceeded):
			w.say(rctx, fmt.Sprintf("⌛ no answer — %s abandoned; say `%s` to start over", label, label))
		case err != nil:
			w.say(rctx, fmt.Sprintf("❌ %s: %s", label, err.Error()))
		default:
			if done != "" {
				w.say(rctx, done)
			}
			if w.then != nil {
				w.then(ctx)
			}
		}
	}()
}

// cancelWizard stops an open flow on th. Returns false if none.
func (c *Coordinator) cancelWizard(th transport.ThreadID) bool {
	c.mu.Lock()
	cancel, ok := c.wizards[th]
	c.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

type wizard struct {
	c      *Coordinator
	s      surface.Surface
	thread transport.ThreadID
	kind   string
	id     string
	parent context.Context // the coordinator's context; ends only on shutdown
	step   int
	then   func(context.Context) // runs after a successful flow, once the thread is free

	answers []string // every answer so far; persisted after each one
	next    int      // answers[next:] are still to be replayed
}

func (w *wizard) say(ctx context.Context, text string) {
	w.c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: w.thread, Text: text}, w.s)
}

// ask posts one question and waits for its answer (button value or typed
// text). Questions are rendered only on the surface that started the flow.
// While saved answers remain they are returned instead of asking.
func (w *wizard) ask(ctx context.Context, q agent.Question) (string, error) {
	w.step++
	if w.next < len(w.answers) {
		a := w.answers[w.next]
		w.next++
		return a, nil
	}
	base := fmt.Sprintf("%s-%s#%d", w.kind, w.id, w.step)
	ch := make(chan transport.Decision, 1)
	w.c.mu.Lock()
	w.c.pending[base] = ch
	w.c.askText[w.thread] = base
	w.c.mu.Unlock()
	defer w.c.clearAsk(w.thread, base)

	w.c.emit(ctx, surface.Event{Kind: surface.EventQuestion, Thread: w.thread, Question: &q, PromptID: base}, w.s)

	select {
	case d := <-ch:
		w.c.append(ctx, "", w.thread, "decision", d)
		a := unquote(d.Choice)
		w.answers = append(w.answers, a)
		w.next = len(w.answers)
		if w.kind == flowAddAgent {
			if err := w.c.Store.PutFlow(ctx, store.FlowState{Thread: w.thread, Transport: w.s.Transport(), Surface: w.s.Name(), Kind: w.kind, Answers: w.answers}); err != nil {
				w.c.Log.Error("persist flow", "thread", w.thread, "err", err)
			}
		}
		return a, nil
	case <-time.After(wizardTimeout):
		return "", context.DeadlineExceeded
	case <-ctx.Done():
		if w.parent.Err() != nil {
			return "", ctx.Err() // shutdown
		}
		return "", errWizardCancelled
	}
}

// askUntil repeats a question until validate accepts the answer.
func (w *wizard) askUntil(ctx context.Context, q agent.Question, validate func(string) (string, error)) (string, error) {
	orig := q.Text
	for {
		a, err := w.ask(ctx, q)
		if err != nil {
			return "", err
		}
		v, verr := validate(a)
		if verr == nil {
			return v, nil
		}
		q.Text = "⚠️ " + verr.Error() + "\n" + orig
	}
}

// unquote strips the backticks or quotes people wrap answers in. Slack in
// particular refuses to send a message that starts with "/" (it looks like
// a slash command), so paths arrive as `/home/me/app`.
func unquote(a string) string {
	a = strings.TrimSpace(a)
	for _, q := range []string{"`", "\"", "'"} {
		if len(a) >= 2 && strings.HasPrefix(a, q) && strings.HasSuffix(a, q) {
			return strings.TrimSpace(a[1 : len(a)-1])
		}
	}
	return a
}

// pathHint tells Slack users how to send a path.
const pathHint = " Wrap it in backticks (Slack drops messages that start with `/`)."

func options(pairs ...string) []agent.Option {
	var out []agent.Option
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, agent.Option{Label: pairs[i], Description: pairs[i+1]})
	}
	return out
}

func isNone(a string) bool {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "", "none", "no", "skip", "-":
		return true
	}
	return false
}

// run drives the questions and saves the result.
func (w *wizard) run(ctx context.Context) (agent.Definition, error) {
	def := agent.Definition{Kind: agent.KindClaude}
	var err error

	def.Name, err = w.askUntil(ctx, agent.Question{Header: "New agent", Text: "Name for the new agent? (letters, digits, `.`, `_`, `-`)"}, func(a string) (string, error) {
		if !nameRE.MatchString(a) {
			return "", fmt.Errorf("%q is not a valid name", a)
		}
		if _, err := w.c.Store.GetDefinition(ctx, a); err == nil {
			return "", fmt.Errorf("an agent named *%s* already exists", a)
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		return a, nil
	})
	if err != nil {
		return def, err
	}

	def.Model, err = w.askUntil(ctx, agent.Question{Header: "Model", Text: "Which model? Pick one or type a full model id.",
		Options: options("sonnet", "balanced", "opus", "most capable", "haiku", "fastest")}, func(a string) (string, error) {
		if a == "" {
			return "", errors.New("model cannot be empty")
		}
		return a, nil
	})
	if err != nil {
		return def, err
	}

	kind, err := w.askUntil(ctx, agent.Question{Header: "Environment", Text: "Where does it run?",
		Options: options("local", "on the dancer host", "docker", "a container per task", "ssh", "on a remote host")}, func(a string) (string, error) {
		switch k := strings.ToLower(a); environment.Kind(k) {
		case environment.KindLocal, environment.KindDocker, environment.KindSSH:
			return k, nil
		}
		return "", fmt.Errorf("pick local, docker or ssh")
	})
	if err != nil {
		return def, err
	}
	def.Environment.Kind = environment.Kind(kind)

	switch def.Environment.Kind {
	case environment.KindLocal:
		def.Environment.Workdir, err = w.askUntil(ctx, agent.Question{Header: "Working directory", Text: "Absolute path of the working directory on the dancer host? `none` = a fresh directory per task." + pathHint,
			Options: options("none", "fresh directory per task")}, validateLocalDir)
	case environment.KindDocker:
		def.Environment.Image, err = w.askUntil(ctx, agent.Question{Header: "Image", Text: "Docker image? It must contain the `claude` binary."}, nonEmpty("image"))
		if err == nil {
			def.Environment.Workdir, err = w.askUntil(ctx, agent.Question{Header: "Working directory", Text: "Host directory to mount at `/work`? `none` = a fresh directory per task." + pathHint,
				Options: options("none", "fresh directory per task")}, validateLocalDir)
		}
	case environment.KindSSH:
		def.Environment.Host, err = w.askUntil(ctx, agent.Question{Header: "Host", Text: "SSH host? `user@host` or an alias from `~/.ssh/config`."}, nonEmpty("host"))
		if err == nil {
			def.Environment.Workdir, err = w.askUntil(ctx, agent.Question{Header: "Working directory", Text: "Remote working directory? `none` = a fresh directory per task." + pathHint,
				Options: options("none", "fresh directory per task")}, func(a string) (string, error) {
				if isNone(a) {
					return "", nil
				}
				return a, nil
			})
		}
	}
	if err != nil {
		return def, err
	}

	mode, err := w.askUntil(ctx, agent.Question{Header: "Permissions", Text: "Permission mode?",
		Options: options(
			string(agent.PermissionManual), "ask before every tool not pre-approved",
			string(agent.PermissionAcceptEdits), "file edits without asking, ask for the rest",
			string(agent.PermissionAuto), "let Claude decide",
			string(agent.PermissionBypass), "never ask (trusted sandboxes only)",
		)}, func(a string) (string, error) {
		for _, m := range []agent.PermissionMode{agent.PermissionManual, agent.PermissionAcceptEdits, agent.PermissionAuto, agent.PermissionBypass} {
			if strings.EqualFold(a, string(m)) {
				return string(m), nil
			}
		}
		return "", fmt.Errorf("pick one of the listed modes")
	})
	if err != nil {
		return def, err
	}
	def.PermissionMode = agent.PermissionMode(mode)

	var toolOpts []string
	for _, p := range toolPresets {
		toolOpts = append(toolOpts, p.label, p.desc)
	}
	tools, err := w.askUntil(ctx, agent.Question{Header: "Tools", Text: "Pre-approved tools? Pick a preset or type a comma-separated list (e.g. `Read,Edit,Bash(npm test:*)`).",
		Options: options(toolOpts...)}, func(a string) (string, error) { return a, nil })
	if err != nil {
		return def, err
	}
	def.AllowedTools = parseTools(tools)

	def.SystemPrompt, err = w.askUntil(ctx, agent.Question{Header: "System prompt", Text: "Extra instructions for the agent? Type them, or skip.",
		Options: options("Skip", "no extra instructions")}, func(a string) (string, error) {
		if isNone(a) {
			return "", nil
		}
		return a, nil
	})
	if err != nil {
		return def, err
	}

	ok, err := w.askUntil(ctx, agent.Question{Header: "Save?", Text: summarize(def), Options: options("Save", "write it to the config and enable it now", "Cancel", "discard")},
		func(a string) (string, error) {
			switch strings.ToLower(a) {
			case "save", "yes", "y", "ok":
				return "save", nil
			case "cancel", "no", "n", "discard":
				return "cancel", nil
			}
			return "", fmt.Errorf("answer Save or Cancel")
		})
	if err != nil {
		return def, err
	}
	if ok != "save" {
		return def, errWizardCancelled
	}

	if w.c.SaveDefinition != nil {
		if err := w.c.SaveDefinition(ctx, def); err != nil {
			return def, fmt.Errorf("writing config: %w", err)
		}
	}
	if err := w.c.Store.PutDefinition(ctx, def); err != nil {
		return def, fmt.Errorf("store: %w", err)
	}
	w.c.Log.Info("definition added from chat", "name", def.Name, "thread", w.thread)
	return def, nil
}

func nonEmpty(what string) func(string) (string, error) {
	return func(a string) (string, error) {
		if isNone(a) {
			return "", fmt.Errorf("%s cannot be empty", what)
		}
		return a, nil
	}
}

func validateLocalDir(a string) (string, error) {
	if isNone(a) {
		return "", nil
	}
	if strings.HasPrefix(a, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			a = filepath.Join(home, a[2:])
		}
	}
	if !filepath.IsAbs(a) {
		return "", fmt.Errorf("%q is not an absolute path", a)
	}
	if fi, err := os.Stat(a); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%q is not a directory on this host", a)
	}
	return filepath.Clean(a), nil
}

// parseTools maps a preset label or a comma-separated list to tool names.
func parseTools(a string) []string {
	for _, p := range toolPresets {
		if strings.EqualFold(a, p.label) {
			return append([]string(nil), p.tools...)
		}
	}
	var out []string
	for _, t := range strings.Split(a, ",") {
		if t = strings.TrimSpace(t); t != "" && !isNone(t) {
			out = append(out, t)
		}
	}
	return out
}

func summarize(d agent.Definition) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Save this agent?\n• name: *%s*\n• model: %s\n• environment: %s", d.Name, d.Model, d.Environment.Kind)
	switch d.Environment.Kind {
	case environment.KindDocker:
		fmt.Fprintf(&b, " · image `%s`", d.Environment.Image)
	case environment.KindSSH:
		fmt.Fprintf(&b, " · host `%s`", d.Environment.Host)
	}
	if d.Environment.Workdir != "" {
		fmt.Fprintf(&b, "\n• workdir: `%s`", d.Environment.Workdir)
	} else {
		b.WriteString("\n• workdir: fresh directory per task")
	}
	fmt.Fprintf(&b, "\n• permission mode: %s", d.PermissionMode)
	if len(d.AllowedTools) > 0 {
		fmt.Fprintf(&b, "\n• pre-approved tools: %s", strings.Join(d.AllowedTools, ", "))
	} else {
		b.WriteString("\n• pre-approved tools: none")
	}
	if d.SystemPrompt != "" {
		fmt.Fprintf(&b, "\n• system prompt: %s", truncate(d.SystemPrompt, 200))
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
