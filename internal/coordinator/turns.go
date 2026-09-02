package coordinator

import (
	"context"
	"time"

	"github.com/cleanunicorn/dispatch/internal/executor"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// Waiting for one agent turn to end.
//
// A task id does not name a turn. A finished turn keeps its process warm
// for idle_timeout, so the turn whose closing line a human just read and
// the turn their next word starts are the *same* task. `merge` used to
// tell those apart by draining its waiter after posting a line — a timing
// argument rather than an identity, and one a workflow running several
// turns on one thread loses on its first step.
//
// So a turn is named by where it ended in the log. A waiter carries a
// floor — the seq of the record it wrote before asking for the turn — and
// settles on the first end past it. Ends of everything that came before
// are below the floor and go by. The floor is the same number the step's
// evidence window is scanned from (internal/work), which is the point: a
// turn is the records it produced.

// turnEnd is one turn ending on a thread.
type turnEnd struct {
	Task executor.TaskID
	Seq  int64 // log seq of the record the turn ended on
	// Done says the task's process is gone. A turn that never reached the
	// agent at all — an environment that would not come up — writes no
	// record to outrank a floor, and its waiter would otherwise sit until
	// its own timeout for a turn that can no longer end.
	Done bool
}

// turnWait is how a wait ended.
type turnWait int

const (
	turnDone turnWait = iota // the turn ended
	turnGone                 // dispatch is going down
	turnSlow                 // the turn outlasted the timeout
)

// turnEndBuffer is how many ends one waiter can fall behind by. A waiter
// is interested in one turn and hears about at most a handful before it;
// past this the oldest are ones it has already skipped.
const turnEndBuffer = 8

// awaitTurnEnd returns a channel every turn ending on the thread is
// announced on, well or badly (taskSink.OnEvent on a result, and drive on
// the way out for a turn that never reached the agent). It is buffered so
// an end is never lost while the caller is between selects, and turnEnded
// never blocks on a waiter that has gone away.
//
// Register before writing the floor record, not after: an end announced
// into a waiter that does not exist yet is an end nobody hears.
func (c *Coordinator) awaitTurnEnd(th transport.ThreadID) chan turnEnd {
	ch := make(chan turnEnd, turnEndBuffer)
	c.mu.Lock()
	c.turnEnds[th] = append(c.turnEnds[th], ch)
	c.mu.Unlock()
	return ch
}

// dropTurnWaiter removes a waiter that gave up, so a thread nobody is
// waiting on keeps no channels.
func (c *Coordinator) dropTurnWaiter(th transport.ThreadID, ch chan turnEnd) {
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

// turnEnded tells everything waiting on this thread that a turn is over.
// Waiters are left registered — the one that cares removes itself — and a
// full one is dropped rather than blocking the agent's event loop behind
// a reader that is not there.
func (c *Coordinator) turnEnded(th transport.ThreadID, end turnEnd) {
	c.mu.Lock()
	waiters := append([]chan turnEnd(nil), c.turnEnds[th]...)
	c.mu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- end:
		default:
		}
	}
}

// waitForTurn blocks until task's turn past since ends on the thread, or
// until the process running it is gone. Everything below the floor goes by,
// and so does every other task's end: a workflow shares its thread with
// whoever types in it, and a human's turn ending past the floor must not
// settle a wait that was only ever about the step's own turn.
//
// task may be empty: then only the floor decides, which is what a caller
// waiting for *whatever* is running (a step queued behind a human's turn)
// wants — and a process going for a gone task settles it too, because on a
// thread with a turn in flight that process is the turn.
func (c *Coordinator) waitForTurn(ctx context.Context, task executor.TaskID, since int64, ends chan turnEnd, timeout time.Duration) turnWait {
	deadline := time.After(timeout)
	for {
		select {
		case end := <-ends:
			switch {
			case task != "" && end.Task == task:
				if end.Seq > since || end.Done {
					return turnDone
				}
			case task == "" && (end.Seq > since || end.Done):
				return turnDone
			}
		case <-ctx.Done():
			return turnGone
		case <-deadline:
			return turnSlow
		}
	}
}
