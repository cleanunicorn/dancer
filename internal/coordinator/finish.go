package coordinator

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/work"
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// The end of a piece of work is the same few messages every time: open a
// second thread and ask it to review the pull request, come back and push
// what it found, then merge and close. `review` and `merge` are those, in
// one word each.
//
// Both read what the thread is working on out of its own log (internal/work,
// through overview) rather than asking for a number: a thread that opened a
// pull request already said so, and having to paste the URL back is exactly
// the friction these remove.

// reviewPR opens a thread beside this one and sets the same agent to
// review the pull request this thread is working on.
//
// A new thread rather than a follow-up, because a review is worth nothing
// from the session that wrote the code: it has every reason it made each
// choice already in its context. A fresh thread is a fresh session, a
// fresh environment and — this is the point — a reader who has to get its
// opinion from the diff.
//
// The same agent definition, because it is the one with an environment
// that can check the repository out. `default <agent>` in the channel
// changes what a review runs on nothing but the channel's next new thread;
// to review with a different agent, start that thread by hand.
//
// The word itself is only the refusals and the sending off: the open, the
// wait for the review's turn and the reading of its report back are one
// quiet run of the workflow engine (workflow.go's attemptReview) — the
// same path a named workflow's review step takes, with none of a named
// run's progress lines.
func (c *Coordinator) reviewPR(ctx context.Context, s surface.Surface, it surface.ReviewPR) {
	w := c.overview(ctx, it.Thread)
	if w == nil || w.PR == nil {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: noPullRequest(w, "review")}, s)
		return
	}
	host := c.taskTransport(ctx, s, it.Thread)
	if _, ok := c.transports[host].(transport.ThreadOpener); !ok {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: fmt.Sprintf("the %s transport cannot open a thread — start the review thread yourself", host)}, s)
		return
	}
	// The review is happening, so this thread is live again: without
	// this, execute's defer puts the tombstone back and the reply below
	// lands in a thread dispatch has stopped listening to. Only here,
	// past every refusal above — `review` that came to nothing must leave
	// a closed thread closed.
	c.reopenThread(ctx, s, it.Thread)
	c.beginWorkflow(ctx, workflowStart{s: s, quiet: true, st: oneStepWorkflow("review", workflow.BuiltinReview, it.Thread, c.taskTransport(ctx, s, it.Thread), s.Name(), it.User)})
}

// oneStepWorkflow is the run a bare end-of-thread word is: one built-in
// step, quiet, over when the step is.
func oneStepWorkflow(name, builtin string, th transport.ThreadID, transportName, surfaceName, user string) *workflow.State {
	return workflow.Start(workflow.Definition{Name: name, Steps: []workflow.Step{{Name: name, Builtin: builtin}}},
		th, transportName, surfaceName, "", user, time.Now())
}

// agentOf names the agent to run beside a thread: the one the thread's own
// last task ran, else whatever the channel would start a new task with.
func (c *Coordinator) agentOf(ctx context.Context, s surface.Surface, th transport.ThreadID) string {
	if st, err := c.Store.LatestTaskForThread(ctx, th); err == nil && st.Definition.Name != "" {
		return st.Definition.Name
	}
	return c.defaultAgent(ctx, s, th)
}

// reviewPrompt is what the review thread is started with. It is also the
// message posted as that thread's root, so the thread reads as if someone
// had typed it — and so the pull request's URL is in the new thread's log
// from its first record, which is what lets `merge` work from there too.
func reviewPrompt(w work.State) string {
	var b strings.Builder
	b.WriteString("Review pull request " + prURL(w) + ".\n\n")
	b.WriteString("Check it out, read the whole diff, and report what you find: correctness bugs first, ")
	b.WriteString("then anything that does not match this repository's conventions, then what is missing ")
	b.WriteString("(tests, docs). Say plainly when something is fine — do not invent findings.\n\n")
	b.WriteString("Report only. Do not push, commit or merge anything.")
	return b.String()
}

// mergeTimeout bounds the turn that merges. Committing, resolving a
// conflict and merging is real work, and a permission prompt in the
// middle of it waits for a human; past this dispatch stops waiting and
// says so, leaving the thread exactly as the agent left it.
const mergeTimeout = 30 * time.Minute

// mergePR asks the thread's agent to get its pull request merged, and
// closes the thread if the log says it did.
//
// dispatch runs none of it. The agent commits what is outstanding, pushes,
// resolves a conflict with the base branch if GitHub reports one, and runs
// `gh pr merge` itself — it has the checkout, the login and the context to
// know what the change meant, and every one of those steps is a command,
// which is the agent's job and not dispatch's. Adding a `gh` of our own
// here would be dispatch growing a second, worse GitHub client beside the
// one already in the container.
//
// What dispatch does instead is what it already does everywhere else: it
// reads the log back (internal/work). A `gh pr merge` this thread ran,
// answered by gh's own "Merged pull request", is work.State.Merged — and
// the thread is closed on that sighting and on nothing else. An agent that
// reports success without a merge in the log closes nothing; the thread
// stays open with what the scan did and did not see.
//
// The word is the refusals, the claim and the sending off; the asking,
// waiting and reading back are one quiet run of the workflow engine
// (workflow.go's attemptMerge), the same path a named workflow's merge
// step takes.
func (c *Coordinator) mergePR(ctx context.Context, s surface.Surface, it surface.MergePR) {
	method, ok := surface.MergeMethod(it.Method)
	if !ok {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "usage: `merge` · `merge squash` · `merge merge` · `merge rebase`"}, s)
		return
	}
	w := c.overview(ctx, it.Thread)
	if w == nil || w.PR == nil {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: noPullRequest(w, "merge")}, s)
		return
	}
	if w.Merged {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: prLink(*w) + " is already merged — `close` when you are done here"}, s)
		return
	}
	if c.turnRunning(it.Thread) {
		// The merge would queue behind the running turn, and its report
		// would be read before the merge had happened. An *idle* session
		// is not busy: the process is only being kept warm, which is
		// exactly the state a thread is in when someone says `merge`.
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "a turn is running on this thread — let it finish, or `cancel` it, then `merge`"}, s)
		return
	}
	if c.wizardOpen(it.Thread) || c.answering(it.Thread) {
		// The prompt would be delivered as the answer to the open
		// question instead of reaching the agent.
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "answer the open question on this thread first, or `cancel` it, then `merge`"}, s)
		return
	}
	if !c.startMerge(it.Thread) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "already merging this thread's pull request"}, s)
		return
	}
	// Claimed: a turn is about to run here, so the thread is live again
	// and execute's defer must not tombstone it back. Only here, past
	// every refusal above — a `merge` that merged nothing must leave a
	// closed thread closed.
	c.reopenThread(ctx, s, it.Thread)
	c.beginWorkflow(ctx, workflowStart{s: s, quiet: true, mergeClaimed: true, mergeMethod: method,
		st: oneStepWorkflow("merge", workflow.BuiltinMerge, it.Thread, c.taskTransport(ctx, s, it.Thread), s.Name(), it.User)})
}

// The merge record, written twice: once when the agent is asked, which is
// also the floor the turn and its evidence are read from, and once with
// what the log said afterwards. A crash between them leaves a thread that
// says what it was in the middle of.
const recordMerge = "merge"

const (
	mergeAsked    = "asked"
	mergeFinished = "finished"
)

type mergeRecord struct {
	PR     string `json:"pr"`
	Method string `json:"method"`
	Phase  string `json:"phase"`
	Merged bool   `json:"merged,omitempty"`
}

// mergePrompt is what the agent is asked. Every step in it is a command
// the agent runs; dispatch only reads what came back.
//
// It says twice what not to do, because the difference between a conflict
// and a refusal is the whole of dispatch's opinion here: a conflict is a
// mechanical obstacle and gets fixed, while a red check, a missing review
// or a branch protection rule is somebody's decision and is a thing to
// report.
func mergePrompt(w work.State, method string) string {
	var b strings.Builder
	b.WriteString("Get " + prURL(w) + " merged.\n\n")
	b.WriteString("1. If anything belonging to this change is uncommitted, commit it and push. ")
	b.WriteString("Leave scratch files and debris out of the commit, and say what you left out.\n")
	b.WriteString("2. Ask GitHub whether it can merge (`gh pr view --json mergeStateStatus,mergeable`). ")
	b.WriteString("If it conflicts with the base branch, merge the base branch in, resolve the conflicts and push.\n")
	b.WriteString("3. Merge it: `gh pr merge " + prArg(w) + " --" + method + " --delete-branch`.\n")
	b.WriteString("4. Report what happened, and paste what gh said.\n\n")
	b.WriteString("Do not try to get past a failing check, a missing review or a branch protection rule — ")
	b.WriteString("those are decisions somebody made, so report the refusal and stop. ")
	b.WriteString("Do not close, reopen or retarget the pull request, and do not force-push over anyone.")
	return b.String()
}

// turnRunning says the thread has a turn in flight — not merely a process
// kept warm for the next message, which is what c.lookup alone reports
// for the whole of idle_timeout after a turn ends.
func (c *Coordinator) turnRunning(th transport.ThreadID) bool {
	id, ok := c.lookup(th)
	if !ok {
		return false
	}
	sink := c.sink(id)
	if sink == nil {
		return false
	}
	switch sink.snapshot().Status {
	case store.StatusRunning, store.StatusWaitingPermission, store.StatusQueued:
		return true
	}
	return false
}

// answering says a question on the thread is waiting for a typed answer,
// which the next message becomes instead of reaching the agent.
func (c *Coordinator) answering(th transport.ThreadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.askText[th]
	return ok
}

// startMerge claims the thread for one merge, false when another has it.
func (c *Coordinator) startMerge(th transport.ThreadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.merging[th] {
		return false
	}
	c.merging[th] = true
	return true
}

func (c *Coordinator) endMerge(th transport.ThreadID) {
	c.mu.Lock()
	delete(c.merging, th)
	c.mu.Unlock()
}

// noPullRequest says why there is nothing to act on, using whatever the
// thread did say: a branch with no pull request is the common case, and
// "open one first" is more use than "no pull request".
func noPullRequest(w *work.State, verb string) string {
	if w != nil && w.Branch != "" {
		return fmt.Sprintf("nothing to %s: this thread is on `%s` but the log has no pull request — open one first", verb, w.Branch)
	}
	return fmt.Sprintf("nothing to %s: this thread has not opened a pull request", verb)
}

// prArg names the pull request on a command line. It is the URL when
// there is one and the bare number when there is not — never prURL's
// "#51", which a shell reads as the start of a comment and would leave
// `gh pr merge` merging whatever branch the agent happened to be on.
func prArg(w work.State) string {
	if w.PR == nil {
		return ""
	}
	if u := prURL(w); strings.HasPrefix(u, "https://") {
		return u
	}
	return strconv.Itoa(w.PR.Number)
}

// prURL is the pull request's canonical URL, built from the repository
// when the thread only ever named a number.
func prURL(w work.State) string {
	if w.PR == nil {
		return ""
	}
	if w.PR.URL != "" {
		return w.PR.URL
	}
	if w.Repo != "" {
		return fmt.Sprintf("https://github.com/%s/pull/%d", w.Repo, w.PR.Number)
	}
	return fmt.Sprintf("#%d", w.PR.Number)
}

// prLink reads as "#51" and clicks through, the way the overview's own
// references do.
func prLink(w work.State) string {
	if w.PR == nil {
		return ""
	}
	label := fmt.Sprintf("#%d", w.PR.Number)
	if u := prURL(w); strings.HasPrefix(u, "https://") {
		return transport.Link(u, label)
	}
	return label
}
