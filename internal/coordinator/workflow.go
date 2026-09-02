package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/decider"
	"github.com/cleanunicorn/dispatch/internal/executor"
	execlocal "github.com/cleanunicorn/dispatch/internal/executor/local"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/work"
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// The workflow runner: the loop `merge` has always been — ask an agent for
// something, wait for *that* turn, read the log back, decide what happens
// next — over a list of steps instead of a hard-coded one
// (internal/workflow for what a step is and what makes one done).
//
// A run lives beside the inbox, never on it: `merge` already works from its
// own goroutine because a thread must keep hearing everything while its turn
// runs, and a workflow lives for possibly an hour and owes its home thread
// the same. A human typing on the home thread mid-run is a follow-up the
// runner did not ask for; the turn id (turns.go) is what keeps it from being
// mistaken for the step's turn, and a step queued behind a human turn waits
// for that turn to end before asking for its own.
//
// `review` and `merge` are one-step runs of this engine (finish.go starts
// them, quiet: the word's own lines are the step's lines, and a run that is
// over when its step is needs no progress chatter and no table row). That is
// the test that the abstraction is real — there is one way to run a turn and
// read the log back, and the bare words are its shortest workflows.
//
// A named run is state first and a goroutine second: every transition is
// written to the log (recordWorkflow) and to the workflows table
// (workflow.State), so a restart replays into the same place — the same
// contract as store.FlowState for the wizards. Resuming grades before it
// resends: a step whose turn the restart cut short is judged on the records
// that made it to the log, and only a step that did not happen is asked for
// again. `cancel` stops a run and writes that down, so a restart cannot
// resume a workflow a human stopped.

// stepTimeout bounds one step's whole attempt: the turn it asked for, plus
// whatever human turn it queued behind. A step that outlasts it is failed,
// not forgotten — the turn keeps running, and its records still land in the
// log for a human to read.
const stepTimeout = 45 * time.Minute

// goneTimeout bounds how long a model-switching step waits for the warm
// process it stopped to let go of the thread.
const goneTimeout = 10 * time.Second

// recordWorkflow is the log kind of a workflow transition.
const recordWorkflow = "workflow"

// kindStep is the decider's question for an ExpectJudge step.
const kindStep = "step"

// wfRecord is what a workflow record holds.
type wfRecord struct {
	Workflow string `json:"workflow"`
	Step     string `json:"step"`
	Event    string `json:"event"` // started, resumed, running, passed, failed, gate, note, stopped, done
	Detail   string `json:"detail,omitempty"`
}

// workflowRun is one workflow on one thread.
type workflowRun struct {
	c      *Coordinator
	s      surface.Surface
	st     *workflow.State
	id     string
	cancel context.CancelFunc
	// quiet is a bare word's run: no progress lines, no log records, no
	// workflows-table row. Its lines are the word's own (the step
	// functions post them), it is over when its step is, and it is not
	// resumable — the merge record finish.go writes is what says what it
	// was in the middle of.
	quiet bool
	// resumed marks a run replayed by resumeWorkflows, whose current step
	// is graded before anything is asked for again.
	resumed bool
	// mergeClaimed says the bare `merge` already claimed the thread's
	// merge before starting the run, so the step does not refuse its own
	// word; mergeMethod is the gh flag the word was given.
	mergeClaimed bool
	mergeMethod  string
	// asks counts the questions a run has asked, for prompt ids.
	asks int
	// addendum is what a human typed with their retry, said again to the
	// next attempt.
	addendum string
	// stopMu guards stopReason, which is written by whoever asked for the
	// stop (the inbox goroutine, a shutdown) and read by the run's own on
	// the way out of the loop.
	stopMu     sync.Mutex
	stopReason string
}

// stop records why the run is ending, for exit to read.
func (r *workflowRun) stop(reason string) {
	r.stopMu.Lock()
	r.stopReason = reason
	r.stopMu.Unlock()
}

// stopped is the reason a human gave, "" when nobody stopped it.
func (r *workflowRun) stopped() string {
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	return r.stopReason
}

// workflowStart is everything a run begins with.
type workflowStart struct {
	s  surface.Surface
	st *workflow.State
	// quiet, resumed, mergeClaimed and mergeMethod are the bare-word and
	// restart paths; see workflowRun.
	quiet        bool
	resumed      bool
	mergeClaimed bool
	mergeMethod  string
	// opening is the line posted before the first step. beginWorkflow
	// posts it *before* it starts the goroutine, because from then on
	// the state the line is rendered from belongs to that goroutine and
	// reading it from the caller's is a race.
	opening string
}

// startWorkflow runs one of the config's workflows by name on a thread.
func (c *Coordinator) startWorkflow(ctx context.Context, s surface.Surface, it surface.RunWorkflow) {
	def, ok := c.workflowDefinition(it.Name)
	if !ok {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no workflow named " + strconv.Quote(it.Name) + " — `workflows` lists them"}, s)
		return
	}
	if err := workflow.Validate(def, c.knownAgent(ctx)); err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: err.Error()}, s)
		return
	}
	if reason := c.workflowBlocked(it.Thread); reason != "" {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: reason}, s)
		return
	}
	// Every refusal is past: the workflow is work on this thread, and the
	// thread has to hear it.
	c.reopenThread(ctx, s, it.Thread)
	st := workflow.Start(def, it.Thread, c.taskTransport(ctx, s, it.Thread), s.Name(), it.Ask, it.User, time.Now())
	c.beginWorkflow(ctx, workflowStart{s: s, st: st,
		opening: fmt.Sprintf("🧗 workflow *%s* started — %s", def.Name, st.Summary())})
}

// workflowDefinition finds a workflow by name.
func (c *Coordinator) workflowDefinition(name string) (workflow.Definition, bool) {
	for _, w := range c.workflowList() {
		if w.Name == name {
			return w, true
		}
	}
	return workflow.Definition{}, false
}

// workflowList is the workflows that can be started. It is read under the
// lock because `workflow save` appends to it from a thread (plan.go) while
// the inbox goroutine is reading it.
func (c *Coordinator) workflowList() []workflow.Definition {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]workflow.Definition(nil), c.Workflows...)
}

// knownAgent says whether a definition name exists, for workflow.Validate.
func (c *Coordinator) knownAgent(ctx context.Context) func(string) bool {
	return func(name string) bool {
		_, err := c.Store.GetDefinition(ctx, name)
		return err == nil
	}
}

// workflowBlocked says why a workflow cannot start on th, "" when it can.
func (c *Coordinator) workflowBlocked(th transport.ThreadID) string {
	if c.turnRunning(th) {
		return "a turn is running on this thread — let it finish, or `cancel` it, then start the workflow"
	}
	if c.wizardOpen(th) {
		return "finish or `cancel` the questions on this thread first"
	}
	if c.answering(th) {
		return "answer the open question on this thread first, or `cancel` it"
	}
	c.mu.Lock()
	_, merging := c.merging[th]
	_, running := c.workflows[th]
	c.mu.Unlock()
	switch {
	case merging:
		return "this thread's pull request is being merged — wait for it"
	case running:
		return "a workflow is already running on this thread — `cancel` stops it"
	}
	return ""
}

// beginWorkflow registers a run and drives it on its own goroutine. A quiet
// run is not registered: it holds no slot (the bare `review` must not block
// a `merge` the way a named run would), persists nothing, and is bounded by
// its step.
func (c *Coordinator) beginWorkflow(ctx context.Context, w workflowStart) *workflowRun {
	r := &workflowRun{c: c, s: w.s, st: w.st, id: newID(), quiet: w.quiet, resumed: w.resumed,
		mergeClaimed: w.mergeClaimed, mergeMethod: w.mergeMethod}
	// Said here, while this goroutine still owns the state: past the `go`
	// below it belongs to the run, and rendering a line from it — even a
	// copy of it, whose Steps slice is the same backing array — races.
	if w.opening != "" {
		r.event(ctx, w.opening)
	}
	if w.quiet {
		c.drives.Add(1)
		go func() {
			defer c.drives.Done()
			r.loop(ctx)
		}()
		return r
	}
	if w.resumed {
		r.record(ctx, "resumed", "")
	} else {
		r.record(ctx, "started", "")
	}
	r.save(ctx)
	rctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	c.mu.Lock()
	c.workflows[w.st.Thread] = r
	c.mu.Unlock()
	c.drives.Add(1)
	go func() {
		defer c.drives.Done()
		r.loop(rctx)
		cancel()
		c.mu.Lock()
		if c.workflows[w.st.Thread] == r {
			delete(c.workflows, w.st.Thread)
		}
		c.mu.Unlock()
	}()
	return r
}

// loop drives the run to its end: one step at a time, each one asked for,
// waited on and judged before the next is started.
func (r *workflowRun) loop(ctx context.Context) {
	st := r.st
	if r.resumed {
		if !r.resumeStep(ctx) {
			r.exit(ctx)
			return
		}
	}
	for st.Live() {
		step := st.Current()
		if step == nil {
			break
		}
		if step.Gate != "" {
			if !r.gate(ctx, step) {
				break
			}
			continue
		}
		if !r.runStep(ctx, step) {
			break
		}
	}
	r.exit(ctx)
}

// exit writes the run's last state down. A loop that ended while still live
// was stopped or cut short by a shutdown; the shutdown keeps the row (the
// next start resumes it), a stop says so and goes.
func (r *workflowRun) exit(ctx context.Context) {
	st := r.st
	reason := r.stopped()
	if st.Live() {
		switch {
		case reason != "":
			st.Status = workflow.RunStopped
		case ctx.Err() != nil:
			// Shutdown mid-run: the row stays so resumeWorkflows picks it
			// up; nothing is said — shutdown says it for every thread.
			r.save(ctx)
			return
		default:
			st.Status = workflow.RunFailed
		}
	}
	if r.quiet {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// The row goes first, and the line second. A restart resumes from the
	// row, so between saying "stopped" and clearing it there is a window
	// where a human has been told the run is over and a SIGTERM would
	// start it again. Make it true, then say it.
	if err := r.c.Store.DeleteWorkflow(rctx, st.Thread); err != nil {
		r.c.Log.Error("delete workflow", "thread", st.Thread, "err", err)
	}
	switch st.Status {
	case workflow.RunDone:
		r.record(rctx, "done", "")
		r.event(rctx, fmt.Sprintf("🏁 workflow *%s* %s", st.Def.Name, st.Summary()))
	case workflow.RunStopped:
		r.record(rctx, "stopped", reason)
		r.event(rctx, fmt.Sprintf("⏹️ workflow *%s* %s", st.Def.Name, st.Summary()))
	default:
		detail := ""
		if cs := st.CurrentState(); cs != nil {
			detail = cs.Detail
		}
		r.record(rctx, "failed", detail)
		r.event(rctx, fmt.Sprintf("❌ workflow *%s* %s", st.Def.Name, st.Summary()))
	}
}

// stopWorkflow ends the run on th where it is. The step's turn is not the
// run's to kill: `cancel` stops both, and a stop that only raced the turn
// would judge a half-finished window.
func (c *Coordinator) stopWorkflow(ctx context.Context, th transport.ThreadID, reason string) bool {
	c.mu.Lock()
	r := c.workflows[th]
	c.mu.Unlock()
	if r == nil {
		return false
	}
	r.stop(reason)
	if r.cancel != nil {
		r.cancel()
	}
	return true
}

// runStep asks for one step until it is done, or puts it to the thread when
// it is not. false when the run is over (stopped, shut down, or halted).
func (r *workflowRun) runStep(ctx context.Context, step *workflow.Step) bool {
	st := r.st
	cs := st.CurrentState()
	if cs != nil && cs.Status == workflow.StepFailed && st.Status == workflow.RunWaiting {
		// A restart replayed a failed step that was waiting on a human:
		// ask again, ask nothing of the agent.
		return r.stepFailed(ctx, step, cs.Detail)
	}
	for {
		ok, detail, cont := r.stepAttempts(ctx, step)
		if !cont {
			return false
		}
		if ok {
			r.passed(step)
			return true
		}
		switch step.Failure() {
		case workflow.OnFailStop:
			if cs := st.CurrentState(); cs != nil {
				cs.Status = workflow.StepFailed
				cs.Detail = detail
			}
			st.Status = workflow.RunFailed
			r.save(ctx)
			r.event(ctx, fmt.Sprintf("❌ workflow *%s* step *%s* failed — %s; stopping", st.Def.Name, step.Name, detail))
			return false
		case workflow.OnFailRetry:
			// stepAttempts already spent the tries; falling to the thread
			// is what a step that kept failing has earned.
		}
		if cs := st.CurrentState(); cs != nil {
			cs.Status = workflow.StepFailed
			cs.Detail = detail
		}
		return r.stepFailed(ctx, step, detail)
	}
}

// stepFailed puts a failed step to the thread: retry, skip or stop. A typed
// reply is a retry with the human's words attached.
func (r *workflowRun) stepFailed(ctx context.Context, step *workflow.Step, detail string) bool {
	st := r.st
	st.Status = workflow.RunWaiting
	r.record(ctx, "failed", step.Name+": "+detail)
	r.save(ctx)
	r.event(ctx, fmt.Sprintf("❌ workflow *%s* step *%s* failed — %s", st.Def.Name, step.Name, detail))
	answer, note, cont := r.ask(ctx,
		fmt.Sprintf("Step *%s* of workflow *%s* failed: %s", step.Name, st.Def.Name, detail),
		[]string{"Retry", "run the step again", "Skip", "leave it failed and go on to the next step", "Stop", "end the workflow here"})
	if !cont {
		return false
	}
	cs := st.CurrentState()
	switch answer {
	case "skip":
		cs.Status = workflow.StepSkipped
		cs.Detail = detail
		st.Status = workflow.RunRunning
		st.Step++
		if st.Step >= len(st.Steps) {
			st.Status = workflow.RunDone
		}
		r.save(ctx)
		return st.Status != workflow.RunDone
	case "stop":
		r.stop("the thread stopped it at " + step.Name)
		return false
	default: // retry, or the human's own words for what to do differently
		if note != "" {
			r.addendum = note
			r.note(ctx, step.Name, note)
		}
		cs.Status = workflow.StepPending
		cs.Tries = 0
		cs.Detail = ""
		st.Status = workflow.RunRunning
		r.save(ctx)
		for {
			ok, detail, cont := r.stepAttempts(ctx, step)
			if !cont {
				return false
			}
			if ok {
				r.passed(step)
				return true
			}
			if step.Failure() == workflow.OnFailStop {
				cs.Status, cs.Detail = workflow.StepFailed, detail
				st.Status = workflow.RunFailed
				r.save(ctx)
				r.event(ctx, fmt.Sprintf("❌ workflow *%s* step *%s* failed — %s; stopping", st.Def.Name, step.Name, detail))
				return false
			}
			return r.stepFailed(ctx, step, detail)
		}
	}
}

// passed marks the current step done and advances, finishing the run when
// none remain.
func (r *workflowRun) passed(step *workflow.Step) {
	st := r.st
	if cs := st.CurrentState(); cs != nil {
		cs.Status = workflow.StepPassed
		cs.Detail = ""
	}
	st.Status = workflow.RunRunning
	st.Step++
	if st.Step >= len(st.Steps) {
		st.Status = workflow.RunDone
	}
	r.save(context.Background())
}

// stepAttempts runs the step as many times as its on_fail allows, stopping
// at the first attempt that is done. cont is false only when the run itself
// is over.
func (r *workflowRun) stepAttempts(ctx context.Context, step *workflow.Step) (ok bool, detail string, cont bool) {
	tries := step.Tries()
	for i := 0; i < tries; i++ {
		if i > 0 {
			r.say(ctx, fmt.Sprintf("↻ workflow *%s*: retrying *%s* — %s", r.st.Def.Name, step.Name, detail))
		}
		ok, detail, cont = r.attempt(ctx, step)
		if !cont || ok {
			return ok, detail, cont
		}
		if step.Failure() != workflow.OnFailRetry {
			break
		}
	}
	return false, detail, true
}

// attempt asks for the step's turn once, waits for it and judges it.
func (r *workflowRun) attempt(ctx context.Context, step *workflow.Step) (ok bool, detail string, cont bool) {
	st := r.st
	ss := st.CurrentState()
	ss.Status = workflow.StepRunning
	ss.Tries++
	ss.Detail = ""
	r.save(ctx)

	switch {
	case step.Builtin == workflow.BuiltinReview:
		return r.attemptReview(ctx, step, ss)
	case step.Builtin == workflow.BuiltinMerge:
		return r.attemptMerge(ctx, step, ss)
	case step.Where() == workflow.ThreadNew:
		return r.attemptPromptNew(ctx, step, ss)
	default:
		return r.attemptPromptSame(ctx, step, ss)
	}
}

// attemptPromptSame runs the step as a follow-up on the home thread.
func (r *workflowRun) attemptPromptSame(ctx context.Context, step *workflow.Step, ss *workflow.StepState) (ok bool, detail string, cont bool) {
	c, st := r.c, r.st
	text, err := r.prompt(ctx, step)
	if err != nil {
		return false, err.Error(), true
	}
	// Registered before the floor is written: an end announced into a
	// waiter that does not exist yet is an end nobody hears (turns.go).
	ends := c.awaitTurnEnd(st.Thread)
	defer c.dropTurnWaiter(st.Thread, ends)
	ss.Since = c.append(ctx, ss.Task, st.Thread, recordWorkflow, wfRecord{st.Def.Name, step.Name, "running", ""})
	r.event(ctx, fmt.Sprintf("▶️ %d/%d *%s* — asking %s", st.Step+1, len(st.Steps), step.Name, agentLabel(step)))
	r.save(ctx)

	// A human's turn may be in flight; the step queues behind it, and its
	// own turn is the next end past the floor.
	if !r.awaitIdle(ctx, st.Thread, ends, ss.Since) {
		return false, "", false
	}

	// A step that names a model gets it by resuming the session with it
	// (drive reads ModelPin); a warm process keeps its session's model, so
	// it is stopped first — the cancelled line is the process ending, not
	// the step.
	if step.Model != "" {
		if id, bound := c.lookup(st.Thread); bound {
			prev := c.pinModel(ctx, id, step.Model)
			if prev != step.Model {
				defer func() {
					rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					defer cancel()
					c.pinModel(rctx, id, prev)
				}()
				if c.Executor.IsRunning(id) {
					if err := c.Executor.Cancel(ctx, id); err == nil || errors.Is(err, execlocal.ErrNotRunning) {
						c.awaitGone(st.Thread, id, goneTimeout)
					}
				}
			}
		}
	}

	var task executor.TaskID
	if c.taskOnThread(ctx, st.Thread) || step.Agent == "" {
		var started bool
		task, started = c.followUp(ctx, r.s, surface.FollowUp{Thread: st.Thread, Text: text, User: st.User})
		if !started {
			return false, "nothing here could be asked — start a task with `run` first", true
		}
	} else {
		c.runTaskModel(ctx, r.s, surface.RunTask{Thread: st.Thread, Agent: step.Agent, Prompt: text, User: st.User}, step.Model)
		var bound bool
		task, bound = c.lookup(st.Thread)
		if !bound {
			return false, "the agent " + strconv.Quote(step.Agent) + " could not be started", true
		}
	}
	ss.Task, ss.Thread = task, st.Thread

	switch c.waitForTurn(ctx, task, ss.Since, ends, stepTimeout) {
	case turnGone:
		return false, "", false
	case turnSlow:
		r.say(ctx, "· the step's turn is still going — the workflow is not waiting for it")
		return false, "the turn outlasted the workflow's patience", true
	}
	return r.judgeStep(ctx, step, ss)
}

// attemptPromptNew runs the step on a thread of its own, the way `review`
// does: a session that has never seen the work.
func (r *workflowRun) attemptPromptNew(ctx context.Context, step *workflow.Step, ss *workflow.StepState) (ok bool, detail string, cont bool) {
	c, st := r.c, r.st
	text, err := r.prompt(ctx, step)
	if err != nil {
		return false, err.Error(), true
	}
	host := c.taskTransport(ctx, r.s, st.Thread)
	opener, canOpen := c.transports[host].(transport.ThreadOpener)
	if !canOpen {
		return false, fmt.Sprintf("the %s transport cannot open a thread — the step needs one of its own", host), true
	}
	ss.Since = c.append(ctx, ss.Task, st.Thread, recordWorkflow, wfRecord{st.Def.Name, step.Name, "running", ""})
	r.event(ctx, fmt.Sprintf("▶️ %d/%d *%s* — asking %s in a new thread", st.Step+1, len(st.Steps), step.Name, agentLabel(step)))

	newTh, err := opener.OpenThread(ctx, channelOf(st.Thread), transport.Outbound{Text: text})
	if err != nil {
		c.Log.Error("workflow: open thread", "transport", host, "thread", st.Thread, "err", err)
		return false, "could not open a thread — " + err.Error(), true
	}
	c.setHost(newTh, host)
	if tt, ok := c.transports[host].(transport.ThreadTracker); ok {
		tt.Remember(newTh)
	}
	name := step.Agent
	if name == "" {
		name = c.agentOf(ctx, r.s, st.Thread)
	}
	c.runTaskModel(ctx, r.s, surface.RunTask{Thread: newTh, Agent: name, Prompt: text, User: st.User}, step.Model)
	task, bound := c.lookup(newTh)
	if !bound {
		return false, "the agent could not be started", true
	}
	ss.Task, ss.Thread = task, newTh
	// Nothing can end on a thread that has no task yet, so registering
	// here still hears the turn runTask is about to start.
	ends := c.awaitTurnEnd(newTh)
	defer c.dropTurnWaiter(newTh, ends)
	switch c.waitForTurn(ctx, task, ss.Since, ends, stepTimeout) {
	case turnGone:
		return false, "", false
	case turnSlow:
		r.say(ctx, "· the step's turn is still going — the workflow is not waiting for it")
		return false, "the turn outlasted the workflow's patience", true
	}
	return r.judgeStep(ctx, step, ss)
}

// attemptReview is `review` as a step: open a thread beside the home one and
// set an agent to review the pull request the home thread is working on.
func (r *workflowRun) attemptReview(ctx context.Context, step *workflow.Step, ss *workflow.StepState) (ok bool, detail string, cont bool) {
	c, st := r.c, r.st
	w := c.overview(ctx, st.Thread)
	if w == nil || w.PR == nil {
		return false, noPullRequest(w, "review"), true
	}
	host := c.taskTransport(ctx, r.s, st.Thread)
	opener, canOpen := c.transports[host].(transport.ThreadOpener)
	if !canOpen {
		return false, fmt.Sprintf("the %s transport cannot open a thread — start the review thread yourself", host), true
	}
	ss.Since = c.append(ctx, ss.Task, st.Thread, recordWorkflow, wfRecord{st.Def.Name, step.Name, "running", ""})
	r.event(ctx, fmt.Sprintf("▶️ %d/%d *%s* — reviewing %s", st.Step+1, len(st.Steps), step.Name, prLink(*w)))

	prompt := reviewPrompt(*w)
	newTh, err := opener.OpenThread(ctx, channelOf(st.Thread), transport.Outbound{Text: prompt})
	if err != nil {
		c.Log.Error("workflow: open thread", "transport", host, "thread", st.Thread, "err", err)
		return false, "could not open a thread — " + err.Error(), true
	}
	c.setHost(newTh, host)
	if tt, ok := c.transports[host].(transport.ThreadTracker); ok {
		tt.Remember(newTh)
	}
	c.Log.Info("review thread opened", "from", st.Thread, "thread", newTh, "pr", w.PR.Number)
	// The review is happening, so the home thread is live again: without
	// this, execute's defer puts the tombstone back and the step's own
	// lines land in a thread dispatch has stopped listening to.
	c.reopenThread(ctx, r.s, st.Thread)
	name := step.Agent
	if name == "" {
		name = c.agentOf(ctx, r.s, st.Thread)
	}
	r.say(ctx, "🔍 reviewing "+prLink(*w)+" in a new thread")
	c.runTaskModel(ctx, r.s, surface.RunTask{Thread: newTh, Agent: name, Prompt: prompt, User: st.User}, step.Model)
	task, bound := c.lookup(newTh)
	if !bound {
		return false, "the agent could not be started", true
	}
	ss.Task, ss.Thread = task, newTh
	ends := c.awaitTurnEnd(newTh)
	defer c.dropTurnWaiter(newTh, ends)
	switch c.waitForTurn(ctx, task, ss.Since, ends, stepTimeout) {
	case turnGone:
		return false, "", false
	case turnSlow:
		r.say(ctx, "· the review turn is still going — the workflow is not waiting for it")
		return false, "the review turn outlasted the workflow's patience", true
	}
	return r.judgeStep(ctx, step, ss)
}

// attemptMerge is `merge` as a step: ask the thread's agent to get the pull
// request merged, and close the thread only on what the log says back.
func (r *workflowRun) attemptMerge(ctx context.Context, step *workflow.Step, ss *workflow.StepState) (ok bool, detail string, cont bool) {
	c, st := r.c, r.st
	w := c.overview(ctx, st.Thread)
	if w == nil || w.PR == nil {
		return false, noPullRequest(w, "merge"), true
	}
	if w.Merged {
		return false, prLink(*w) + " is already merged", true
	}
	// Registered before the floor is written (turns.go).
	ends := c.awaitTurnEnd(st.Thread)
	defer c.dropTurnWaiter(st.Thread, ends)
	if !r.awaitIdle(ctx, st.Thread, ends, c.lastSeq(ctx, st.Thread)) {
		return false, "", false
	}
	if !r.mergeClaimed {
		if !c.startMerge(st.Thread) {
			return false, "already merging this thread's pull request", true
		}
		r.mergeClaimed = true
	}
	defer c.endMerge(st.Thread)
	// Claimed: a turn is about to run here, so the thread is live again.
	c.reopenThread(ctx, r.s, st.Thread)

	method := r.mergeMethod
	if method == "" {
		method = "squash"
	}
	since := c.append(ctx, "", st.Thread, recordMerge, mergeRecord{PR: prURL(*w), Method: method, Phase: mergeAsked})
	ss.Since = since
	ss.Thread = st.Thread
	r.event(ctx, fmt.Sprintf("▶️ %d/%d *%s* — merging %s", st.Step+1, len(st.Steps), step.Name, prLink(*w)))
	r.say(ctx, fmt.Sprintf("🚢 asking the agent to merge %s (%s)…", prLink(*w), method))
	task, started := c.followUp(ctx, r.s, surface.FollowUp{Thread: st.Thread, Text: mergePrompt(*w, method), User: st.User})
	if !started {
		// followUp reports its own failures, but not all of them, so say
		// something rather than going quiet on a thread that just asked.
		c.Log.Info("merge: no turn to wait for", "thread", st.Thread)
		r.say(ctx, "· nothing here could be asked to merge it — start a task with `run` first")
		return false, "nothing here could be asked to merge it — start a task with `run` first", true
	}
	ss.Task = task
	switch c.waitForTurn(ctx, task, since, ends, mergeTimeout) {
	case turnGone:
		return false, "", false
	case turnSlow:
		c.Log.Warn("merge: the turn never ended", "thread", st.Thread, "task", task, "after", mergeTimeout)
		r.say(ctx, "· the merging turn is still going — the thread stays open")
		return false, "the merging turn is still going", true
	}

	// The turn is over; record and close on a context a shutdown cannot
	// take away.
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	ev := r.evidence(jctx, step, ss)
	ev.Carried = carriedPR(w)
	ok, detail = workflow.Judge(*step, ev)
	ss.Report = ev.Report
	c.append(jctx, task, st.Thread, recordMerge, mergeRecord{PR: prURL(*w), Method: method, Phase: mergeFinished, Merged: ok})
	if !ok {
		c.Log.Info("merge: the log does not say it merged", "thread", st.Thread, "pr", prURL(*w))
		r.say(jctx, "the log does not show "+prLink(*w)+" merged — the thread stays open. `merge` again, or `close` if you merged it elsewhere")
		return false, detail, true
	}
	c.Log.Info("merged", "thread", st.Thread, "pr", prURL(*w), "method", method)
	c.closeThread(jctx, r.s, surface.CloseThread{Thread: st.Thread})
	return true, "", true
}

// judgeStep reads the step's window back and says whether it is done.
func (r *workflowRun) judgeStep(ctx context.Context, step *workflow.Step, ss *workflow.StepState) (ok bool, detail string, cont bool) {
	if ctx.Err() != nil {
		// On the way down: judging a half-drained window would fail a
		// step whose turn is still finishing. The row says the step is
		// running; the next start grades the whole window.
		return false, "", false
	}
	ev := r.evidence(ctx, step, ss)
	if step.Judged() == workflow.ExpectJudge && ev.Judged == "" {
		// Nobody has said. The decider is asked first (its static answer
		// is ask), then the thread.
		ev.Judged = r.askJudge(ctx, step, ev)
		if ev.Judged == "" {
			return false, "", false // shut down mid-question
		}
	}
	ok, detail = workflow.Judge(*step, ev)
	ss.Report = ev.Report
	if ok {
		r.record(ctx, "passed", step.Name)
	} else {
		r.record(ctx, "failed", step.Name+": "+detail)
		r.c.Log.Info("workflow: step not done", "workflow", r.st.Def.Name, "step", step.Name, "why", detail)
	}
	return ok, detail, true
}

// evidence gathers what the step's own records say, and runs the step's
// check in the environment the turn worked in.
func (r *workflowRun) evidence(ctx context.Context, step *workflow.Step, ss *workflow.StepState) workflow.Evidence {
	c := r.c
	ev := workflow.Evidence{}
	recs, err := c.Store.ThreadRecords(ctx, ss.Thread, overviewRecords)
	if err != nil {
		c.Log.Warn("workflow: reading the step's window", "thread", ss.Thread, "err", err)
	}
	window := make([]store.Record, 0, len(recs))
	sawEnd := false
	for _, rec := range recs {
		if rec.Seq <= ss.Since {
			continue
		}
		window = append(window, rec)
		if rec.Kind != "agent" {
			continue
		}
		var e agent.Event
		if json.Unmarshal(rec.Payload, &e) != nil {
			continue
		}
		switch e.Type {
		case agent.EventText:
			if !e.Partial {
				ev.Report = e.Text
			}
		case agent.EventResult:
			sawEnd = true
			if strings.TrimSpace(ev.Report) == "" {
				ev.Report = e.Text
			}
		case agent.EventError:
			sawEnd = true
			ev.Failed = true
		}
	}
	if !sawEnd {
		// Every turn that ended left a result or an error behind; a
		// window with neither is a turn a restart cut short.
		ev.Failed = true
	}
	if ss.Task != "" {
		if t, err := c.Store.GetTask(ctx, ss.Task); err == nil {
			switch t.Status {
			case store.StatusFailed, store.StatusCancelled:
				ev.Failed = true
			case store.StatusInterrupted:
				ev.Failed = true
			}
		}
		// A result in the window is not proof the turn finished. An agent
		// being drained reports one on its way out and it lands in the
		// log like any other, so the window cannot tell "it answered"
		// from "it was cut off mid-sentence" — and grading on that would
		// pass a step for work that never happened. What the previous
		// process left in flight is read at startup, before recover()
		// normalises the rows (seedCutShort).
		if c.wasCutShort(ss.Task) {
			ev.Failed = true
		}
	}
	w := work.Scan(window)
	if w.PR != nil {
		ev.Saw = w.PR.Number
		if w.PR.Seen == work.SeenCreated {
			ev.Opened = w.PR.Number
		}
		if r.st.PR == 0 {
			r.st.PR = w.PR.Number // the workflow carries it from here
		}
	}
	ev.Pushed = w.Pushed
	if w.Merged && w.PR != nil {
		ev.Merged = w.PR.Number
	}
	if strings.TrimSpace(step.Check) != "" {
		cmd, rerr := workflow.Render(step.Check, r.data(ctx))
		switch {
		case rerr != nil:
			ev.Check = &workflow.Check{OK: false, Output: "the check did not render: " + rerr.Error()}
		default:
			if code, out, cerr := c.Executor.Check(ctx, ss.Task, cmd); cerr != nil {
				ev.Check = nil // "the check never ran"
			} else {
				ev.Check = &workflow.Check{OK: code == 0, Output: out}
			}
		}
	}
	return ev
}

// askJudge puts an ExpectJudge step to the decider, then to the thread.
func (r *workflowRun) askJudge(ctx context.Context, step *workflow.Step, ev workflow.Evidence) string {
	c := r.c
	task := ""
	if ss := r.st.CurrentState(); ss != nil {
		task = string(ss.Task)
	}
	v := c.decide(ctx, decider.Question{
		Kind: kindStep, Task: task, Thread: string(r.st.Thread),
		Options: workflow.Judgements(),
		Facts:   map[string]any{"workflow": r.st.Def.Name, "step": step.Name, "report": workflow.Trim(ev.Report)},
		Static:  decider.Verdict{Action: workflow.JudgeAsk},
	})
	switch v.Action {
	case workflow.JudgePass, workflow.JudgeFail:
		return v.Action
	}
	// The static answer is ask: a human has to say.
	answer, _, cont := r.ask(ctx,
		fmt.Sprintf("Is step *%s* of workflow *%s* done? Nobody could say but you.", step.Name, r.st.Def.Name),
		[]string{"Pass", "the step is done — go on", "Fail", "it is not done — treat it as failed"})
	if !cont {
		return ""
	}
	if answer == "pass" {
		return workflow.JudgePass
	}
	return workflow.JudgeFail
}

// gate asks a human and runs no turn. The workflow stops on it until
// somebody answers; a restart re-asks (the row says RunWaiting).
func (r *workflowRun) gate(ctx context.Context, step *workflow.Step) bool {
	st := r.st
	ss := st.CurrentState()
	text, err := workflow.Render(step.Gate, r.data(ctx))
	if err != nil {
		ss.Status = workflow.StepFailed
		ss.Detail = err.Error()
		st.Status = workflow.RunFailed
		r.save(ctx)
		r.event(ctx, fmt.Sprintf("❌ workflow *%s*: the gate did not render — %s", st.Def.Name, err.Error()))
		return false
	}
	ss.Status = workflow.StepRunning
	st.Status = workflow.RunWaiting
	r.record(ctx, "gate", step.Name)
	r.save(ctx)
	r.event(ctx, fmt.Sprintf("✋ %d/%d *%s* — waiting for a human", st.Step+1, len(st.Steps), step.Name))
	answer, _, cont := r.ask(ctx, text, []string{"Yes", "go on to the next step", "No", "stop the workflow here"})
	if !cont {
		return false
	}
	if answer != "yes" {
		r.stop("the gate said no at " + step.Name)
		return false
	}
	ss.Status = workflow.StepPassed
	st.Status = workflow.RunRunning
	st.Step++
	if st.Step >= len(st.Steps) {
		st.Status = workflow.RunDone
	}
	r.record(ctx, "passed", step.Name)
	r.save(ctx)
	return st.Status != workflow.RunDone
}

// ask posts one question on the home thread and waits. It uses the question
// machinery agents and wizards use (pending + askText), so a button or a
// typed reply answers it, on any surface. No timeout: a gate can wait a
// day, and a restart re-asks.
func (r *workflowRun) ask(ctx context.Context, text string, choices []string) (answer, note string, cont bool) {
	c, st := r.c, r.st
	r.asks++
	base := fmt.Sprintf("workflow-%s#%d", r.id, r.asks)
	q := agent.Question{Header: "Workflow " + st.Def.Name, Text: text}
	for i := 0; i+1 < len(choices); i += 2 {
		q.Options = append(q.Options, agent.Option{Label: choices[i], Description: choices[i+1]})
	}
	ch := make(chan transport.Decision, 1)
	c.mu.Lock()
	c.pending[base] = ch
	c.askText[st.Thread] = base
	c.mu.Unlock()
	defer c.clearAsk(st.Thread, base)

	c.emit(ctx, surface.Event{Kind: surface.EventQuestion, Thread: st.Thread, Question: &q, PromptID: base}, r.s)

	var d transport.Decision
	select {
	case d = <-ch:
	case <-ctx.Done():
		return "", "", false // shutdown or stop: the row says what waits
	}
	c.append(ctx, "", st.Thread, "decision", d)
	answer = strings.ToLower(strings.TrimSpace(d.Choice))
	if note = unquote(d.Choice); strings.EqualFold(note, answer) {
		note = "" // a button, not words
	}
	return answer, note, true
}

// prompt renders the step's prompt over the run's state, with whatever a
// human added to their retry.
func (r *workflowRun) prompt(ctx context.Context, step *workflow.Step) (string, error) {
	text, err := workflow.Render(step.Prompt, r.data(ctx))
	if err != nil {
		return "", fmt.Errorf("the prompt did not render: %w", err)
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("the prompt rendered to nothing — the state it asked for is not there")
	}
	if r.addendum != "" {
		text += "\n\nA human watching this workflow added:\n" + r.addendum
		r.addendum = ""
	}
	return text, nil
}

// data is what the next prompt is rendered against: the home thread's work,
// re-read, never cached.
func (r *workflowRun) data(ctx context.Context) workflow.Data {
	w := r.c.overview(ctx, r.st.Thread)
	var repo, branch, pr, issue string
	if w != nil {
		repo, branch = w.Repo, w.Branch
		if w.PR != nil {
			pr = prArg(*w)
			if r.st.PR == 0 {
				r.st.PR = w.PR.Number
			}
		}
		if w.Issue != nil {
			if w.Issue.URL != "" {
				issue = w.Issue.URL
			} else {
				issue = strconv.Itoa(w.Issue.Number)
			}
		}
	}
	return r.st.Data(repo, branch, pr, issue)
}

// awaitIdle waits for whatever turn is running on th to end, so the step's
// own ask is not queued behind a stranger's.
func (r *workflowRun) awaitIdle(ctx context.Context, th transport.ThreadID, ends chan turnEnd, since int64) bool {
	deadline := time.Now().Add(stepTimeout)
	for r.c.turnRunning(th) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			r.say(ctx, "· a turn is still running on this thread — the workflow is not waiting for it")
			return false
		}
		switch r.c.waitForTurn(ctx, "", since, ends, remaining) {
		case turnGone:
			return false
		case turnSlow:
			continue
		}
	}
	return true
}

// say posts the run's own line on the home thread.
func (r *workflowRun) say(ctx context.Context, text string) {
	r.c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: r.st.Thread, Text: text}, r.s)
}

// event broadcasts a workflow move. Quiet runs say nothing: their lines are
// the step's own.
func (r *workflowRun) event(ctx context.Context, text string) {
	if r.quiet {
		return
	}
	st := *r.st
	r.c.broadcast(ctx, surface.Event{Kind: surface.EventWorkflow, Thread: st.Thread, Text: text, Workflow: &st})
}

// record appends a workflow transition to the log. Quiet runs write none:
// the bare words have their own records (the merge record), and a review
// has nothing to record but its turn, which the log already has.
func (r *workflowRun) record(ctx context.Context, event, detail string) {
	if r.quiet {
		return
	}
	r.c.append(ctx, "", r.st.Thread, recordWorkflow, wfRecord{Workflow: r.st.Def.Name, Step: stepName(r.st), Event: event, Detail: detail})
}

// save persists the run's state — the row a restart resumes from, so a
// shutdown's own save runs on a context the shutdown cannot take away.
func (r *workflowRun) save(ctx context.Context) {
	if r.quiet {
		return
	}
	r.st.UpdatedAt = time.Now()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := r.c.Store.PutWorkflow(sctx, *r.st); err != nil {
		r.c.Log.Error("persist workflow", "thread", r.st.Thread, "err", err)
	}
}

// note records what a human added to a retry, in the log only: the next
// attempt says it in its prompt.
func (r *workflowRun) note(ctx context.Context, step, text string) {
	r.c.append(ctx, "", r.st.Thread, recordWorkflow, wfRecord{Workflow: r.st.Def.Name, Step: step, Event: "note", Detail: text})
}

// --- wiring: words, cancel, status, resume ---

// listWorkflows answers `workflows`.
func (c *Coordinator) listWorkflows(ctx context.Context, s surface.Surface, it surface.ListWorkflows) {
	all := c.workflowList()
	if len(all) == 0 {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no workflows in config — see WORKFLOWS.md for what one is"}, s)
		return
	}
	var b strings.Builder
	for _, w := range all {
		fmt.Fprintf(&b, "• *%s*", w.Name)
		if w.Description != "" {
			b.WriteString(" — " + w.Description)
		}
		fmt.Fprintf(&b, " (%d steps)\n", len(w.Steps))
	}
	b.WriteString("start one with `workflow <name> <what you want>`")
	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: b.String()}, s)
}

// workflowOf returns a copy of the run on th, for `status`.
func (c *Coordinator) workflowOf(th transport.ThreadID) *workflow.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.workflows[th]
	if r == nil {
		return nil
	}
	st := *r.st
	return &st
}

// resumeWorkflows picks up the runs a restart left behind. A run whose step
// was mid-turn is graded before anything is asked for again: the records
// that made it to the log are the step's evidence, and a step that already
// happened is not asked for twice. A run that was waiting on a human is
// asked again.
func (c *Coordinator) resumeWorkflows(ctx context.Context) {
	runs, err := c.Store.ListWorkflows(ctx)
	if err != nil {
		c.Log.Error("list workflows", "err", err)
		return
	}
	for _, w := range runs {
		if !w.Live() {
			_ = c.Store.DeleteWorkflow(ctx, w.Thread) // a finished run is history, not state
			continue
		}
		var s surface.Surface
		for _, cand := range c.Surfaces {
			if cand.Name() == w.Surface {
				s = cand
			}
		}
		if s == nil {
			c.Log.Warn("dropping workflow without a surface", "thread", w.Thread, "surface", w.Surface)
			_ = c.Store.DeleteWorkflow(ctx, w.Thread)
			continue
		}
		c.Log.Info("resuming workflow", "thread", w.Thread, "workflow", w.Def.Name, "step", w.Step, "status", w.Status)
		c.beginWorkflow(ctx, workflowStart{s: s, st: &w, resumed: true})
	}
}

// resumeStep is what a resumed run does first: grade the step the restart
// cut short, wait for one that is running again, or re-ask a waiting one.
// false when the run ended here.
func (r *workflowRun) resumeStep(ctx context.Context) bool {
	st := r.st
	step := st.Current()
	if step == nil {
		return false
	}
	ss := st.CurrentState()
	if st.Status == workflow.RunWaiting {
		if step.Gate == "" && ss != nil && ss.Status == workflow.StepFailed {
			r.say(ctx, "▶️ dispatch is back — the failed step still needs an answer")
		} else {
			r.say(ctx, "▶️ dispatch is back — the workflow still needs an answer")
		}
		return true // the loop re-asks through gate / stepFailed
	}
	if ss == nil || ss.Status != workflow.StepRunning || ss.Since == 0 {
		return true // nothing was in flight; the loop starts the step
	}
	// The turn was cut short — or finished after dispatch died. If it is
	// running again (an auto-resume got there first), wait for it; else
	// grade what made it to the log.
	if ss.Task != "" {
		if t, err := r.c.Store.GetTask(ctx, ss.Task); err == nil {
			switch t.Status {
			case store.StatusRunning, store.StatusWaitingPermission, store.StatusQueued:
				ends := r.c.awaitTurnEnd(ss.Thread)
				defer r.c.dropTurnWaiter(ss.Thread, ends)
				switch r.c.waitForTurn(ctx, ss.Task, ss.Since, ends, stepTimeout) {
				case turnDone:
				default:
					return false
				}
				return r.graded(ctx, step, ss)
			}
		}
	}
	return r.graded(ctx, step, ss)
}

// graded judges the step a restart cut short and goes where the answer
// goes: on, or to the failure path.
func (r *workflowRun) graded(ctx context.Context, step *workflow.Step, ss *workflow.StepState) bool {
	st := r.st
	ok, detail, cont := r.judgeStep(ctx, step, ss)
	if !cont {
		return false
	}
	if ok {
		ss.Status = workflow.StepPassed
		st.Status = workflow.RunRunning
		st.Step++
		if st.Step >= len(st.Steps) {
			st.Status = workflow.RunDone
		}
		r.record(ctx, "passed", step.Name)
		r.save(ctx)
		r.event(ctx, fmt.Sprintf("▶️ dispatch is back — the step had already happened; %s", st.Summary()))
		return st.Status != workflow.RunDone
	}
	ss.Status = workflow.StepFailed
	ss.Detail = detail
	st.Status = workflow.RunWaiting
	r.save(ctx)
	return r.stepFailed(ctx, step, detail)
}

// --- small helpers ---

// taskOnThread says whether th has any task to follow up (bound, or idle in
// the store with a session).
func (c *Coordinator) taskOnThread(ctx context.Context, th transport.ThreadID) bool {
	if _, bound := c.lookup(th); bound {
		return true
	}
	st, err := c.Store.LatestTaskForThread(ctx, th)
	return err == nil && st.Session != ""
}

// lastSeq is the highest seq the thread has recorded.
func (c *Coordinator) lastSeq(ctx context.Context, th transport.ThreadID) int64 {
	recs, err := c.Store.ThreadRecords(ctx, th, 1)
	if err != nil || len(recs) == 0 {
		return 0
	}
	return recs[0].Seq
}

// pinModel sets the task's ModelPin and returns what it was. The restore is
// the caller's: a pin that outlived its step would change every later step
// on the thread.
func (c *Coordinator) pinModel(ctx context.Context, id executor.TaskID, model string) string {
	if sink := c.sink(id); sink != nil {
		return sink.setPin(ctx, model)
	}
	st, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return ""
	}
	prev := st.ModelPin
	if st.ModelPin != model {
		st.ModelPin = model
		_ = c.Store.PutTask(ctx, st)
	}
	return prev
}

// awaitGone waits for a cancelled task to let go of the thread.
func (c *Coordinator) awaitGone(th transport.ThreadID, id executor.TaskID, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cur, ok := c.lookup(th); !ok || cur != id {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.Log.Warn("workflow: task did not stop in time", "task", id, "thread", th)
	return false
}

// agentLabel names who a step runs, for the progress line.
func agentLabel(step *workflow.Step) string {
	if step.Agent != "" {
		return "*" + step.Agent + "*"
	}
	return "the thread's agent"
}

// carriedPR is the number a merge step's confirmation has to agree with,
// from the overview the merge was built on.
func carriedPR(w *work.State) int {
	if w == nil || w.PR == nil {
		return 0
	}
	return w.PR.Number
}

// stepName names the run's current step, for records.
func stepName(st *workflow.State) string {
	if cs := st.CurrentState(); cs != nil {
		return cs.Name
	}
	return ""
}
