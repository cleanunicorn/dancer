package coordinator

import (
	"context"
	"time"

	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/work"
)

// overviewRecords bounds how far back an overview reads. A thread that
// opens a pull request names it within a few hundred records of doing the
// work; reading the whole of a long-lived thread on every turn would cost
// more than the answer is worth.
const overviewRecords = 1500

// overview is what a thread is working on, projected from its own records
// (internal/work). It is nil when the thread has touched no repository —
// which is most threads — so nothing is added to the ordinary case.
//
// It reads the log rather than remembering: a restart, a new container and
// a task that ended all leave the references exactly where they were.
func (c *Coordinator) overview(ctx context.Context, th transport.ThreadID) *work.State {
	return c.overviewSince(ctx, th, 0)
}

// overviewSince is overview over the records a thread wrote *after* since,
// which is how one step of a workflow — or one `merge` — is graded on its
// own evidence rather than on the whole thread's.
//
// Without the floor a workflow walks to its end on one real result: step
// three asks for a pull request, the scan finds the one step one opened,
// and nothing was checked. since is the seq of the record the caller
// wrote before asking for the turn, which is the same number the turn
// itself is recognised by (turns.go).
func (c *Coordinator) overviewSince(ctx context.Context, th transport.ThreadID, since int64) *work.State {
	if th == "" {
		return nil
	}
	// The turn's closing result is delivered with the executor's own
	// context, which is already cancelled when the turn ended because
	// dispatch is shutting down — and the last thing a human sees before a
	// restart is exactly when the overview is worth having. Read with a
	// context that survives the cancellation, the way persist does.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	recs, err := c.Store.ThreadRecords(rctx, th, overviewRecords)
	if err != nil {
		c.Log.Debug("overview: reading the thread back failed", "thread", th, "err", err)
		return nil
	}
	if since > 0 {
		kept := recs[:0]
		for _, r := range recs {
			if r.Seq > since {
				kept = append(kept, r)
			}
		}
		recs = kept
	}
	st := work.Scan(recs)
	if st.Empty() {
		return nil
	}
	return &st
}
