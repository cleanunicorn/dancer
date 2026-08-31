package coordinator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cleanunicorn/dispatch/internal/gh"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/work"
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
func (c *Coordinator) reviewPR(ctx context.Context, s surface.Surface, it surface.ReviewPR) {
	w := c.overview(ctx, it.Thread)
	if w == nil || w.PR == nil {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: noPullRequest(w, "review")}, s)
		return
	}
	host := c.taskTransport(ctx, s, it.Thread)
	opener, ok := c.transports[host].(transport.ThreadOpener)
	if !ok {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: fmt.Sprintf("the %s transport cannot open a thread — start the review thread yourself", host)}, s)
		return
	}
	prompt := reviewPrompt(*w)
	newTh, err := opener.OpenThread(ctx, channelOf(it.Thread), transport.Outbound{Text: prompt})
	if err != nil {
		c.Log.Error("review: open thread", "transport", host, "thread", it.Thread, "err", err)
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: "review: could not open a thread — " + err.Error()}, s)
		return
	}
	c.setHost(newTh, host)
	if tt, ok := c.transports[host].(transport.ThreadTracker); ok {
		tt.Remember(newTh)
	}
	c.Log.Info("review thread opened", "from", it.Thread, "thread", newTh, "pr", w.PR.Number)
	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "🔍 reviewing " + prLink(*w) + " in a new thread"}, s)
	c.runTask(ctx, s, surface.RunTask{Thread: newTh, Agent: c.agentOf(ctx, s, it.Thread), Prompt: prompt, User: it.User})
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

// prepTimeout bounds the turn that makes a branch mergeable. Resolving a
// conflict is real work and a permission prompt in the middle of it waits
// for a human, so this is generous; past it the merge is abandoned rather
// than run against a branch nobody vouched for.
const prepTimeout = 20 * time.Minute

// mergePR gets the thread's pull request merged, then closes the thread.
//
// Two steps, in this order, and the order is the whole design.
//
// First the agent on the thread is asked to make the branch mergeable:
// commit and push whatever is still sitting in the working tree, and — if
// GitHub says the pull request conflicts — merge the base branch in and
// resolve it. That has to be the agent's job: it is the one with the
// checkout, and a conflict needs someone who knows what the change meant.
//
// Then dispatch merges, itself, with `gh pr merge` (internal/gh). That has
// to *not* be the agent's job, for one reason: something has to know
// whether the merge happened. An agent asked to merge answers in prose,
// and "the required checks are still running" reads much like "merged" to
// anything downstream — while gh has an exit code. The thread is closed on
// that exit code and on nothing else.
//
// What it will not do is route around a refusal. A conflict is a mechanical
// obstacle and gets fixed; a red check, a missing approval or a branch
// protection rule is somebody's decision, and it is reported as gh gave it
// with the thread left open.
//
// It runs off the inbox goroutine — a prep turn can take minutes and
// dispatch has to keep hearing everything else — and one at a time per
// thread.
func (c *Coordinator) mergePR(ctx context.Context, s surface.Surface, it surface.MergePR) {
	method, ok := gh.ParseMethod(it.Method)
	if !ok {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "usage: `merge` · `merge squash` · `merge merge` · `merge rebase`"}, s)
		return
	}
	w := c.overview(ctx, it.Thread)
	if w == nil || w.PR == nil {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: noPullRequest(w, "merge")}, s)
		return
	}
	if _, busy := c.lookup(it.Thread); busy {
		// The prep turn would queue behind whatever is running and the
		// wait below would settle on the wrong turn's end.
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "a task is running on this thread — let it finish, or `cancel` it, then `merge`"}, s)
		return
	}
	if !c.startMerge(it.Thread) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "already merging this thread's pull request"}, s)
		return
	}
	go c.runMerge(context.WithoutCancel(ctx), s, it, *w, method)
}

// runMerge is mergePR off the inbox goroutine: prepare, merge, close.
func (c *Coordinator) runMerge(ctx context.Context, s surface.Surface, it surface.MergePR, w work.State, method gh.Method) {
	defer c.endMerge(it.Thread)
	th, url := it.Thread, prURL(w)

	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: "🚢 " + prLink(w) + " — committing and pushing what is outstanding first…"}, s)
	c.prepare(ctx, s, it, w)

	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: fmt.Sprintf("merging %s (%s)…", prLink(w), method)}, s)
	out, err := gh.Merge(ctx, url, method)
	id, _ := c.lookup(th)
	c.append(ctx, id, th, "merge", map[string]any{"pr": url, "method": string(method), "ok": err == nil, "output": out})
	if err != nil {
		c.Log.Warn("merge failed", "thread", th, "pr", url, "err", err)
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: th, Text: "merge refused — the thread stays open\n" + quote(out)}, s)
		return
	}
	c.Log.Info("merged", "thread", th, "pr", url, "method", method)
	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: "✅ merged\n" + quote(out)}, s)
	c.closeThread(ctx, s, surface.CloseThread{Thread: th})
}

// prepare runs the turn that makes the branch mergeable and waits for it.
//
// A thread whose session is long gone cannot prepare anything, and that is
// not a failure: what is on the branch is what was pushed, which is what
// the merge is about anyway. It says so and carries on.
func (c *Coordinator) prepare(ctx context.Context, s surface.Surface, it surface.MergePR, w work.State) {
	done := c.awaitTurnEnd(it.Thread)
	defer c.dropTurnWaiter(it.Thread, done)
	if !c.followUp(ctx, s, surface.FollowUp{Thread: it.Thread, Text: preparePrompt(w), User: it.User}) {
		c.Log.Info("merge: nothing to prepare with", "thread", it.Thread)
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "· no agent session here to commit with — merging what is already pushed"}, s)
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(prepTimeout):
		c.Log.Warn("merge: the preparing turn never ended", "thread", it.Thread, "after", prepTimeout)
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "· the preparing turn is still going — merging what is pushed so far"}, s)
	}
}

// preparePrompt is what the agent is asked before the merge. It is a
// tidy-up and a conflict fix, and it says so twice: an agent that merged
// the pull request itself would take the exit code dispatch decides on
// away from it.
func preparePrompt(w work.State) string {
	var b strings.Builder
	b.WriteString("I am about to merge " + prURL(w) + ". Get the branch ready, and do not merge it yourself.\n\n")
	b.WriteString("1. If anything belonging to this change is uncommitted, commit it and push. ")
	b.WriteString("Leave scratch files and debris out of the commit, and say what you left out.\n")
	b.WriteString("2. Ask GitHub whether it can merge (`gh pr view --json mergeStateStatus,mergeable`). ")
	b.WriteString("If it conflicts with the base branch, merge the base branch in, resolve the conflicts and push.\n")
	b.WriteString("3. Stop and report. Do not run `gh pr merge`, and do not try to get past a failing check, ")
	b.WriteString("a missing review or a branch protection rule — those are mine to report, not yours to work around.")
	return b.String()
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

// awaitTurnEnd returns a channel closed when the thread's next agent turn
// ends, well or badly (taskSink.OnEvent calls turnEnded).
func (c *Coordinator) awaitTurnEnd(th transport.ThreadID) chan struct{} {
	ch := make(chan struct{})
	c.mu.Lock()
	c.turnEnds[th] = append(c.turnEnds[th], ch)
	c.mu.Unlock()
	return ch
}

// dropTurnWaiter removes a waiter that gave up, so a thread nobody is
// waiting on keeps no channels.
func (c *Coordinator) dropTurnWaiter(th transport.ThreadID, ch chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.turnEnds[th][:0]
	for _, w := range c.turnEnds[th] {
		if w != ch {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		delete(c.turnEnds, th)
		return
	}
	c.turnEnds[th] = kept
}

// turnEnded wakes everything waiting on this thread's turn.
func (c *Coordinator) turnEnded(th transport.ThreadID) {
	c.mu.Lock()
	waiters := c.turnEnds[th]
	delete(c.turnEnds, th)
	c.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
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

// quote indents gh's own output so it reads as the tool's words and not
// dispatch's, and keeps a long failure from filling the thread.
func quote(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return "_(gh said nothing)_"
	}
	lines := strings.Split(out, "\n")
	if len(lines) > 10 {
		lines = append(lines[:10], "…")
	}
	return "```\n" + strings.Join(lines, "\n") + "\n```"
}
