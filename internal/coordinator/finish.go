package coordinator

import (
	"context"
	"fmt"
	"strings"
	"time"

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
// It runs off the inbox goroutine, because the turn takes minutes and
// dispatch has to keep hearing everything else, and one at a time per
// thread.
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
	if _, busy := c.lookup(it.Thread); busy {
		// The merge would queue behind whatever is running and the wait
		// below would settle on the wrong turn's end.
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "a task is running on this thread — let it finish, or `cancel` it, then `merge`"}, s)
		return
	}
	if !c.startMerge(it.Thread) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "already merging this thread's pull request"}, s)
		return
	}
	go c.runMerge(context.WithoutCancel(ctx), s, it, *w, method)
}

// runMerge is mergePR off the inbox goroutine: ask, wait, read the log.
func (c *Coordinator) runMerge(ctx context.Context, s surface.Surface, it surface.MergePR, w work.State, method string) {
	defer c.endMerge(it.Thread)
	th := it.Thread

	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: fmt.Sprintf("🚢 asking the agent to merge %s (%s)…", prLink(w), method)}, s)
	done := c.awaitTurnEnd(th)
	defer c.dropTurnWaiter(th, done)
	if !c.followUp(ctx, s, surface.FollowUp{Thread: th, Text: mergePrompt(w, method), User: it.User}) {
		// followUp has already said why on the thread.
		c.Log.Info("merge: no session to ask", "thread", th)
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
		return
	case <-time.After(mergeTimeout):
		c.Log.Warn("merge: the turn never ended", "thread", th, "after", mergeTimeout)
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: "· the merging turn is still going — the thread stays open"}, s)
		return
	}

	after := c.overview(ctx, th)
	merged := after != nil && after.Merged
	c.append(ctx, "", th, "merge", map[string]any{"pr": prURL(w), "method": method, "merged": merged})
	if !merged {
		c.Log.Info("merge: the log does not say it merged", "thread", th, "pr", prURL(w))
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: "the log does not show " + prLink(w) + " merged — the thread stays open. `merge` again, or `close` if you merged it elsewhere"}, s)
		return
	}
	c.Log.Info("merged", "thread", th, "pr", prURL(w), "method", method)
	c.closeThread(ctx, s, surface.CloseThread{Thread: th})
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
	b.WriteString("3. Merge it: `gh pr merge " + prURL(w) + " --" + method + " --delete-branch`.\n")
	b.WriteString("4. Report what happened, and paste what gh said.\n\n")
	b.WriteString("Do not try to get past a failing check, a missing review or a branch protection rule — ")
	b.WriteString("those are decisions somebody made, so report the refusal and stop. ")
	b.WriteString("Do not close, reopen or retarget the pull request, and do not force-push over anyone.")
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
