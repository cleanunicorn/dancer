package coordinator

import (
	"context"
	"fmt"
	"strings"

	"github.com/cleanunicorn/dispatch/internal/gh"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/work"
)

// The end of a piece of work is three moves a human makes by hand: open a
// second thread and ask it to review the pull request, come back and push
// what the review found, then merge and close. `review` and `ship` are the
// first and the last of those, in one word each.
//
// Both read the thread's own log for what it is working on (internal/work,
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
// from its first record, which is what lets `ship` work from there too.
func reviewPrompt(w work.State) string {
	var b strings.Builder
	b.WriteString("Review pull request " + prURL(w) + ".\n\n")
	b.WriteString("Check it out, read the whole diff, and report what you find: correctness bugs first, ")
	b.WriteString("then anything that does not match this repository's conventions, then what is missing ")
	b.WriteString("(tests, docs). Say plainly when something is fine — do not invent findings.\n\n")
	b.WriteString("Report only. Do not push, commit or merge anything.")
	return b.String()
}

// ship merges the pull request the thread is working on and closes the
// thread — but only in that order and only on GitHub's answer. The merge
// is dispatch's own `gh pr merge` (internal/gh), so a merge that did not
// happen leaves the thread open with gh's reason in it, rather than
// closing on an agent's prose about what it thinks it did.
//
// It runs off the inbox goroutine: a merge waiting on GitHub must not stop
// dispatch from hearing anything else. One at a time per thread, so a
// second `ship` typed while the first is in flight is told to wait rather
// than racing it.
func (c *Coordinator) ship(ctx context.Context, s surface.Surface, it surface.Ship) {
	method, ok := gh.ParseMethod(it.Method)
	if !ok {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "usage: `ship` · `ship squash` · `ship merge` · `ship rebase`"}, s)
		return
	}
	w := c.overview(ctx, it.Thread)
	if w == nil || w.PR == nil {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: noPullRequest(w, "ship")}, s)
		return
	}
	if !c.startShip(it.Thread) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "already merging this thread's pull request"}, s)
		return
	}
	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: fmt.Sprintf("🚢 merging %s (%s)…", prLink(*w), method)}, s)
	url, th := prURL(*w), it.Thread
	go func() {
		defer c.endShip(th)
		// The merge outlives the inbound that asked for it, but not
		// dispatch: a shutdown mid-merge leaves GitHub to finish and the
		// thread open, which is the safe way round.
		out, err := gh.Merge(context.WithoutCancel(ctx), url, method)
		id, _ := c.lookup(th)
		c.append(ctx, id, th, "ship", map[string]any{"pr": url, "method": string(method), "ok": err == nil, "output": out})
		if err != nil {
			c.Log.Warn("ship: merge failed", "thread", th, "pr", url, "err", err)
			c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: th, Text: "merge failed — the thread stays open\n" + quote(out)}, s)
			return
		}
		c.Log.Info("shipped", "thread", th, "pr", url, "method", method)
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: "✅ merged\n" + quote(out)}, s)
		c.closeThread(ctx, s, surface.CloseThread{Thread: th})
	}()
}

// startShip claims the thread for one merge, false when another has it.
func (c *Coordinator) startShip(th transport.ThreadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shipping[th] {
		return false
	}
	c.shipping[th] = true
	return true
}

func (c *Coordinator) endShip(th transport.ThreadID) {
	c.mu.Lock()
	delete(c.shipping, th)
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
