package claude

import "github.com/cleanunicorn/dancer/internal/agent"

// background tracks the sub-agents the CLI runs behind the main session,
// because they change what a result line means.
//
// The Agent tool returns at once ("launched") and the model goes on; its
// turn often ends, with a result line, while the sub-agent still runs.
// When the sub-agent finishes, the CLI queues a notice for the model and
// delivers it in the next request it builds: inside the current turn if
// one is still going, otherwise by starting a new turn of its own (a fresh
// system/init, then another result). So a result line is only the end of
// the turn the human is waiting on when no such task is running and no
// finished task is waiting to be told. Verified against claude 2.1.240;
// testdata/background.jsonl is a captured session.
//
// Only sub-agents (task_type local_agent) are tracked. A shell command
// started with run_in_background gets the same treatment from the CLI
// when it finishes — but a dev server or a watcher never finishes, and a
// result held for it would never be released: no closing line, a process
// the executor never idles, a turn that looks cut short at every restart.
// The CLI reports such a task ended only when its stdin closes. A
// finished command therefore still costs a second turn (init, result) in
// the thread; a sub-agent always ends, so holding for it is safe.
type background struct {
	running map[string]bool // this session's backgrounded sub-agents, by task id, until the CLI reports them finished
	unread  bool            // a tracked task finished and the model has not had a request built since
}

// observe updates the tracker from one translated line.
func (b *background) observe(p parsed) {
	if t := p.Task; t != nil {
		switch {
		case t.Started && t.Background && !t.Owned && t.Kind == "local_agent":
			if b.running == nil {
				b.running = map[string]bool{}
			}
			b.running[t.ID] = true
		case t.Ended && b.running[t.ID]:
			delete(b.running, t.ID)
			b.unread = true
		}
	}
	for _, ev := range p.Events {
		if ev.ParentID != "" {
			continue // a sub-agent's own traffic says nothing about the main session
		}
		switch ev.Type {
		case agent.EventInit, agent.EventToolResult:
			// A new turn, or the next request of this one: the CLI builds it
			// with every queued notice included.
			b.unread = false
		}
	}
}

// settled reports whether a result line now would end the human's turn:
// nothing runs in the background and nothing finished is left to deliver.
func (b *background) settled() bool {
	return len(b.running) == 0 && !b.unread
}
