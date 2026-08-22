package coordinator

import (
	"context"
	"encoding/json"
	"slices"
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

// Question kinds. "resume" is asked in recover(), "permission" in
// AwaitDecision (permission.go); the rest of the call sites in DECIDER.md
// come later.
const kindResume = "resume"

// recordVerdict is the log kind of a decider's answer; "decision" is a
// human's button click, and the two must not be told apart by sniffing.
const recordVerdict = "verdict"

// verdictRecord is what a verdict record holds: the question, so a reader
// knows what was asked and from which facts, and the answer.
type verdictRecord struct {
	Question decider.Question `json:"question"`
	Verdict  decider.Verdict  `json:"verdict"`
}

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
		// The question outlives whole turns: someone may have replied hours
		// ago, run the task again and left it idle with a newer session. The
		// snapshot recover() took is then stale, and writing it back would
		// undo that turn — so re-read, and only act if nothing moved.
		st, err := c.Store.GetTask(ctx, t.ID)
		if err != nil {
			c.Log.Warn("resume answer dropped: task is gone", "task", t.ID, "err", err)
			return
		}
		if st.Session != t.Session || st.LastSeq != t.LastSeq || st.Status == store.StatusRunning {
			c.Log.Info("resume answer ignored: the task moved on", "task", t.ID, "choice", d.Choice,
				"status", st.Status, "was_seq", t.LastSeq, "now_seq", st.LastSeq)
			return
		}
		if d.Choice != actionContinue {
			st.Status = store.StatusCancelled
			_ = c.Store.PutTask(ctx, st)
			c.emitTo(ctx, st.Transport, surface.Event{Kind: surface.EventReply, Thread: st.Thread, TaskID: st.ID, Task: &st,
				Text: "⏹️ dropped — " + pickUpHint(st)})
			return
		}
		st.Resumes++
		_ = c.Store.PutTask(ctx, st)
		c.autoResume(ctx, st, "")
	}()
}

// pickUpHint says how a task can still be picked up by hand. followUp
// resumes a session; a task that never got one has to be started again,
// so the two cases must not be promised the same thing.
func pickUpHint(t store.TaskState) string {
	if t.Session == "" {
		return "say `run` to start it again"
	}
	return "reply in this thread if you want it picked up after all"
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
	d := c.deciderFor(q)
	if d == nil {
		return v // the rules answer it; nothing was spent and nothing to record
	}
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
		// The interface cannot promise this — only Validate can. A verdict
		// that leaves the options, or arrives oversized, is a decider bug
		// and ends where every other failure does: at the rules.
		valid, verr := decider.Validate(q, got)
		if verr != nil {
			c.Log.Warn("decider answered outside the question", "kind", q.Kind, "task", q.Task, "decider", d.Name(), "err", verr)
			break
		}
		v = valid
		c.Log.Info("decision", "kind", q.Kind, "task", q.Task, "action", v.Action, "by", v.By, "reason", v.Reason)
	}
	// Only questions a decider actually saw are on the record, and the
	// record is the budget: what was asked about a task is counted from
	// the log, so neither a restart nor a finished run forgets it.
	c.append(ctx, executor.TaskID(q.Task), transport.ThreadID(q.Thread), recordVerdict, verdictRecord{q, v})
	return v
}

// deciderFor returns the decider allowed to answer this kind, or nil when
// the rules answer it: no decider configured, the kind is not in
// DeciderUses, or the task has already spent its budget of questions.
func (c *Coordinator) deciderFor(q decider.Question) decider.Decider {
	if c.Decider == nil {
		return nil
	}
	if !slices.Contains(c.DeciderUses, q.Kind) {
		return nil
	}
	if q.Task == "" {
		return c.Decider
	}
	max := c.maxDecisions()
	if spent := len(c.taskVerdicts(context.Background(), executor.TaskID(q.Task), max)); spent >= max {
		c.Log.Warn("decider budget spent; using the rules", "task", q.Task, "decisions", spent)
		return nil
	}
	return c.Decider
}

func (c *Coordinator) maxDecisions() int {
	if c.MaxDecisionsPerTask > 0 {
		return c.MaxDecisionsPerTask
	}
	return 20
}

// taskVerdicts reads back up to limit of the verdicts given about a task,
// oldest first. A log that cannot be read reads as no verdicts: the rules
// then answer, which is the safe side.
func (c *Coordinator) taskVerdicts(ctx context.Context, id executor.TaskID, limit int) []verdictRecord {
	records, err := c.Store.TaskRecords(ctx, id, recordVerdict, limit)
	if err != nil {
		c.Log.Warn("reading verdicts from the log", "task", id, "err", err)
		return nil
	}
	out := make([]verdictRecord, 0, len(records))
	for _, r := range records {
		var rec verdictRecord
		if json.Unmarshal(r.Payload, &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// lastVerdict returns the last decision made about a task, for `status`.
func (c *Coordinator) lastVerdict(ctx context.Context, t store.TaskState) (decider.Verdict, bool) {
	vs := c.taskVerdicts(ctx, t.ID, 1)
	if len(vs) == 0 {
		return decider.Verdict{}, false
	}
	return vs[len(vs)-1].Verdict, true
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
func summarizeInput(in map[string]any) string { return oneLine(rawInput(in)) }

// rawInput is the same field, unflattened. Whoever has to read a command
// the way a shell would needs its newlines: they separate commands.
func rawInput(in map[string]any) string {
	for _, k := range []string{"command", "file_path", "path", "pattern", "url"} {
		if v, ok := in[k].(string); ok && v != "" {
			return v
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
