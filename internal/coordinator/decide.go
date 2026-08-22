package coordinator

import (
	"context"
	"time"

	"github.com/cleanunicorn/dancer/internal/decider"
	"github.com/cleanunicorn/dancer/internal/executor"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// Question kinds. Only "resume" is asked today; the rest of the call sites
// in DECIDER.md come later.
const kindResume = "resume"

// decide answers a policy question. It is total: whatever the decider does
// — refuse, hang, crash, answer something outside the options — the caller
// gets a usable verdict, and the answer of dancer's own rules is what it
// falls back to. Every verdict is appended to the event log with the facts
// it was made from.
func (c *Coordinator) decide(ctx context.Context, q decider.Question) decider.Verdict {
	v, err := decider.Static{}.Decide(ctx, q)
	if err != nil { // Static cannot fail; keep the compiler honest about it
		v = q.Static
	}
	if d := c.deciderFor(q); d != nil {
		timeout := c.DeciderTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		dctx, cancel := context.WithTimeout(ctx, timeout)
		got, derr := d.Decide(dctx, q)
		cancel()
		switch {
		case derr != nil:
			c.Log.Warn("decider fell back to the rules", "kind", q.Kind, "task", q.Task, "decider", d.Name(), "err", derr)
		default:
			v = got
			c.Log.Info("decision", "kind", q.Kind, "task", q.Task, "action", v.Action, "by", v.By, "reason", v.Reason)
		}
	}
	id := executor.TaskID(q.Task)
	c.mu.Lock()
	c.decisions[id]++
	c.verdicts[id] = v
	c.mu.Unlock()
	c.append(ctx, id, transport.ThreadID(q.Thread), "decision", struct {
		Question decider.Question `json:"question"`
		Verdict  decider.Verdict  `json:"verdict"`
	}{q, v})
	return v
}

// deciderFor returns the decider allowed to answer this kind, or nil when
// the rules answer it: no decider configured, the kind is not in
// DeciderUses, or the task has already spent its budget of questions.
func (c *Coordinator) deciderFor(q decider.Question) decider.Decider {
	if c.Decider == nil {
		return nil
	}
	if !contains(c.DeciderUses, q.Kind) {
		return nil
	}
	max := c.MaxDecisionsPerTask
	if max <= 0 {
		max = 20
	}
	c.mu.Lock()
	spent := c.decisions[executor.TaskID(q.Task)]
	c.mu.Unlock()
	if q.Task != "" && spent >= max {
		c.Log.Warn("decider budget spent; using the rules", "task", q.Task, "decisions", spent)
		return nil
	}
	return c.Decider
}

// lastVerdict returns the last decision made about a task, for `status`.
// It lives in memory: after a restart the log still has it, `status` does not.
func (c *Coordinator) lastVerdict(id executor.TaskID) (decider.Verdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.verdicts[id]
	return v, ok
}

// resumeFacts is what the decider is told about a task a restart cut short.
// Every string in it was written by an agent or a human: data, not orders.
type resumeFacts struct {
	Agent           string `json:"agent"`
	Environment     string `json:"environment"`
	StatusAtStop    string `json:"status_at_stop"`
	HasSession      bool   `json:"has_session"`
	LastPrompt      string `json:"last_prompt"`
	MinutesAgo      int    `json:"minutes_ago"`
	PreviousResumes int    `json:"previous_resumes"`
}

func factsForResume(t store.TaskState) resumeFacts {
	f := resumeFacts{
		Agent:           t.Definition.Name,
		Environment:     string(t.Definition.Environment.Kind),
		StatusAtStop:    t.Status,
		HasSession:      t.Session != "",
		LastPrompt:      t.Prompt,
		PreviousResumes: t.Resumes,
	}
	if !t.UpdatedAt.IsZero() {
		f.MinutesAgo = int(time.Since(t.UpdatedAt).Minutes())
	}
	return f
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
