package coordinator

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/decider"
	"github.com/cleanunicorn/dancer/internal/executor"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// Question kinds. Only "resume" is asked today; the rest of the call sites
// in DECIDER.md come later.
const kindResume = "resume"

// What a resume decision may come to. The rules only ever answer
// actionContinue or actionWait; a decider may also ask the thread or leave
// the task alone.
const (
	actionContinue = "continue" // resume the session now
	actionWait     = "wait"     // leave it idle; a reply resumes it
	actionAsk      = "ask"      // put the choice to the thread
	actionAbandon  = "abandon"  // stop offering to resume it by itself
)

// askTimeout is how long an unanswered resume question stays live. After
// it the task is simply idle, which is where "wait" would have left it.
const askTimeout = 6 * time.Hour

// askAboutResume puts the choice to the thread instead of taking it. The
// question is the decider's; the buttons answer it, and so does a plain
// reply — that goes through followUp and resumes the task with the human's
// own words, which is a better answer than either button.
func (c *Coordinator) askAboutResume(ctx context.Context, t store.TaskState, v decider.Verdict) {
	base := string(t.ID) + ":resume"
	ch := make(chan transport.Decision, 1)
	c.mu.Lock()
	c.pending[base] = ch
	c.mu.Unlock()

	text := v.Prompt
	if strings.TrimSpace(text) == "" {
		text = "This task was cut short by a restart" + reasonSuffix(v.Reason) + ". Continue it?"
	}
	q := agent.Question{Header: "dancer is back", Text: text,
		Options: []agent.Option{
			{Label: actionContinue, Description: "resume where the agent left off"},
			{Label: "drop", Description: "leave it; you can still reply to pick it up"},
		}}
	tt := t
	c.emitTo(ctx, t.Transport, surface.Event{Kind: surface.EventQuestion, Thread: t.Thread, TaskID: t.ID, Task: &tt,
		Question: &q, PromptID: base})

	go func() {
		var d transport.Decision
		select {
		case d = <-ch:
		case <-time.After(askTimeout):
			c.Log.Info("resume question expired", "task", t.ID)
		case <-ctx.Done():
		}
		c.mu.Lock()
		delete(c.pending, base)
		c.mu.Unlock()
		if d.Choice == "" || ctx.Err() != nil {
			return
		}
		c.append(ctx, t.ID, t.Thread, "decision", d)
		if _, busy := c.lookup(t.Thread); busy {
			// A reply already restarted this thread; the click is stale.
			c.Log.Info("resume answer ignored: the thread moved on", "task", t.ID, "choice", d.Choice)
			return
		}
		if d.Choice != actionContinue {
			st := t
			st.Status = store.StatusCancelled
			_ = c.Store.PutTask(ctx, st)
			c.emitTo(ctx, t.Transport, surface.Event{Kind: surface.EventReply, Thread: t.Thread, TaskID: t.ID, Task: &st,
				Text: "⏹️ dropped — reply in this thread if you want it picked up after all"})
			return
		}
		st := t
		st.Resumes++
		_ = c.Store.PutTask(ctx, st)
		c.autoResume(ctx, st, "")
	}()
}

func reasonSuffix(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return " — " + reason
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
}

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
	// Read back from the event log: what the thread was actually doing.
	LastHumanMessage string   `json:"last_human_message,omitempty"`
	AgentLastWords   string   `json:"agent_last_words,omitempty"`
	RecentEvents     []string `json:"recent_events,omitempty"`
	FilesTouched     []string `json:"files_touched,omitempty"`
	ToolInFlight     string   `json:"tool_in_flight,omitempty"`
}

// How much of the log a decision is worth reading, and how much of it the
// decider is shown. The caps matter twice over: they keep the question
// small, and they bound what a chatty (or hostile) agent can put in front
// of the decider.
const (
	factRecords   = 60
	factEvents    = 20
	factFiles     = 10
	factLine      = 160
	factParagraph = 400
)

// factsForResume reads the thread's tail out of the event log and turns it
// into the facts of a resume question. A log that cannot be read costs
// detail, not the decision: the task projection alone still answers it.
func (c *Coordinator) factsForResume(ctx context.Context, t store.TaskState) resumeFacts {
	f := resumeFacts{
		Agent:           t.Definition.Name,
		Environment:     string(t.Definition.Environment.Kind),
		StatusAtStop:    t.Status,
		HasSession:      t.Session != "",
		LastPrompt:      truncate(strings.TrimSpace(t.Prompt), factParagraph),
		PreviousResumes: t.Resumes,
	}
	if !t.UpdatedAt.IsZero() {
		f.MinutesAgo = int(time.Since(t.UpdatedAt).Minutes())
	}
	records, err := c.Store.ThreadRecords(ctx, t.Thread, factRecords)
	if err != nil {
		c.Log.Warn("reading the thread log for a decision", "task", t.ID, "err", err)
		return f
	}
	files := map[string]bool{}
	inflight := map[string]string{} // tool_use id -> what it is doing
	for _, r := range records {
		switch r.Kind {
		case "inbound":
			var in transport.Inbound
			if json.Unmarshal(r.Payload, &in) == nil && strings.TrimSpace(in.Text) != "" {
				f.LastHumanMessage = truncate(strings.TrimSpace(in.Text), factParagraph)
			}
		case "agent":
			var ev agent.Event
			if json.Unmarshal(r.Payload, &ev) != nil || ev.Partial {
				continue
			}
			if line := describeEvent(ev); line != "" {
				f.RecentEvents = append(f.RecentEvents, line)
			}
			switch ev.Type {
			case agent.EventText, agent.EventResult:
				if strings.TrimSpace(ev.Text) != "" {
					f.AgentLastWords = truncate(strings.TrimSpace(ev.Text), factParagraph)
				}
			case agent.EventToolUse, agent.EventNeedsPermission:
				inflight[ev.ToolID] = describeEvent(ev)
				if p := touchedFile(ev); p != "" {
					files[p] = true
				}
			case agent.EventToolResult:
				delete(inflight, ev.ToolID)
			}
		}
	}
	if n := len(f.RecentEvents); n > factEvents {
		f.RecentEvents = f.RecentEvents[n-factEvents:]
	}
	for p := range files {
		if len(f.FilesTouched) == factFiles {
			break
		}
		f.FilesTouched = append(f.FilesTouched, p)
	}
	sort.Strings(f.FilesTouched)
	for _, what := range inflight {
		f.ToolInFlight = what // a stopped turn has at most a couple; any one says enough
		break
	}
	return f
}

// describeEvent is one line of history for the decider: what happened, and
// to what. Tool inputs are summarized, never passed through whole.
func describeEvent(ev agent.Event) string {
	switch ev.Type {
	case agent.EventToolUse, agent.EventNeedsPermission:
		s := string(ev.Type) + " " + ev.Tool
		if d := summarizeInput(ev.ToolInput); d != "" {
			s += " " + d
		}
		return truncate(s, factLine)
	case agent.EventText, agent.EventResult, agent.EventError:
		if strings.TrimSpace(ev.Text) == "" {
			return string(ev.Type)
		}
		return truncate(string(ev.Type)+" "+oneLine(ev.Text), factLine)
	case agent.EventQuestion:
		if len(ev.Questions) > 0 {
			return truncate("question "+oneLine(ev.Questions[0].Text), factLine)
		}
		return "question"
	default:
		return ""
	}
}

// summarizeInput picks the one field of a tool call that says what it did.
func summarizeInput(in map[string]any) string {
	for _, k := range []string{"command", "file_path", "path", "pattern", "url"} {
		if v, ok := in[k].(string); ok && v != "" {
			return oneLine(v)
		}
	}
	return ""
}

// touchedFile returns the file a tool call writes to, if it writes to one.
func touchedFile(ev agent.Event) string {
	switch ev.Tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		if p, ok := ev.ToolInput["file_path"].(string); ok {
			return p
		}
	}
	return ""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
