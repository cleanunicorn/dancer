// Package workflow is a piece of work broken into steps that dispatch runs
// one after another: implement it, have a second model review it, fix what
// the review found, ask a human, merge.
//
// A step is one agent turn. What makes it a step rather than a message is
// that dispatch decides for itself whether it happened — and never by
// asking the agent, which just did the work and is the worst available
// witness. `merge` established the rule (internal/coordinator/finish.go):
// an agent that reports success the log cannot confirm closes nothing.
// Every step is judged the same way, by the strongest evidence it asked
// for:
//
//	the turn ended       free, and the floor: a failed or cancelled turn
//	                     fails the step whatever else is true
//	the log says so      internal/work over the records *this step*
//	                     produced — a pull request it opened, a branch it
//	                     pushed, a merge it performed
//	a command we ran     dispatch execs Step.Check in the step's own
//	                     environment; exit 0 or the step failed. Not
//	                     mined from what the agent said, observed by
//	                     dispatch doing it
//	a judgement          "did the review find anything blocking?" is
//	                     neither mined nor exec'd: it is the decider's
//	                     `step` question, whose static answer is `ask`,
//	                     so without a decider it becomes a human gate
//
// The window is the whole trick. A step's evidence is scanned from the
// seq of the record written when the step began, so step three asking for
// a pull request is not satisfied by the one step one opened. That is the
// same number the turn itself is recognised by, because a turn *is* the
// records it produced (internal/coordinator/turns.go).
//
// This package is pure: it holds the shape of a workflow, what a step
// means, what makes one done and what a prompt renders to, and it runs
// nothing. The coordinator drives it (internal/coordinator/workflow.go),
// which is the same split internal/work and internal/decider have — the
// judgement is testable without an agent CLI, the goroutines and locks
// live with the tasks.
package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/cleanunicorn/dispatch/internal/executor"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// Definition is a workflow as it is written down: in config.toml, or by
// the planner for one thread (see the coordinator's plan.go).
type Definition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Steps       []Step `json:"steps"`
}

// Step is one thing dispatch asks for and then checks.
//
// A step is exactly one of three shapes, and Validate insists on it: a
// prompt for an agent, one of the built-in words, or a gate that asks a
// human and runs no turn at all.
type Step struct {
	// Name is how the step is referred to in a later step's prompt
	// ({{.Steps.review.Report}}) and in the status line.
	Name string `json:"name"`
	// Agent is the definition to run this step; empty is the one the
	// thread is already using. Naming a different definition is how a
	// step runs on a different model, or a different agent kind.
	Agent string `json:"agent,omitempty"`
	// Model overrides that definition's model for this step only. It is
	// carried as the task's ModelPin, so a resume asks for it again.
	Model string `json:"model,omitempty"`
	// Thread is ThreadSame (a follow-up in the thread the workflow runs
	// on) or ThreadNew (a thread beside it, with a session that has never
	// seen the work — which is the only kind of reviewer worth having).
	Thread string `json:"thread,omitempty"`
	// Prompt is a Go template over Data.
	Prompt string `json:"prompt,omitempty"`
	// Builtin runs one of dispatch's own end-of-thread words instead of a
	// prompt of its own: BuiltinReview, BuiltinMerge.
	Builtin string `json:"builtin,omitempty"`
	// Expect is what the log must show for the step to count as done.
	Expect string `json:"expect,omitempty"`
	// Check is a command dispatch runs itself in the step's environment
	// when the turn ends. Exit 0 or the step failed. It is a template too,
	// so a check can name the branch or the pull request.
	Check string `json:"check,omitempty"`
	// Gate is a question for a human. A gate step runs no turn: the
	// workflow stops on it until somebody answers.
	Gate string `json:"gate,omitempty"`
	// OnFail is what happens when the step is not done: OnFailAsk (put it
	// to the thread), OnFailRetry, OnFailStop.
	OnFail string `json:"on_fail,omitempty"`
	// MaxRetries bounds OnFailRetry. A step that keeps failing must not
	// loop forever, for the reason max_auto_resumes exists.
	MaxRetries int `json:"max_retries,omitempty"`
}

// Where a step runs.
const (
	ThreadSame = "same"
	ThreadNew  = "new"
)

// The built-in words a step may be.
const (
	BuiltinReview = "review"
	BuiltinMerge  = "merge"
)

// What a step is judged on.
const (
	ExpectNone   = "none"   // the turn ended, and nothing else is claimed
	ExpectReport = "report" // the turn produced text for a later step; not verification
	ExpectPR     = "pr"     // this step opened a pull request and the log caught the URL
	ExpectPush   = "push"   // this step pushed a branch and the remote answered
	ExpectMerged = "merged" // this step merged the pull request the workflow carries
	ExpectJudge  = "judge"  // somebody — a decider or a human — has to say
)

// What happens to a step that is not done.
const (
	OnFailAsk   = "ask"   // put it to the thread: retry, skip, stop
	OnFailRetry = "retry" // say what was missing and ask again, up to MaxRetries
	OnFailStop  = "stop"  // the workflow halts where it is
)

// DefaultMaxRetries bounds OnFailRetry when a step names no bound.
const DefaultMaxRetries = 2

// Threads lists where a step may run, in display order.
func Threads() []string { return []string{ThreadSame, ThreadNew} }

// Builtins lists the words a step may be, in display order.
func Builtins() []string { return []string{BuiltinReview, BuiltinMerge} }

// Expects lists what a step may be judged on, in display order.
func Expects() []string {
	return []string{ExpectNone, ExpectReport, ExpectPR, ExpectPush, ExpectMerged, ExpectJudge}
}

// OnFails lists what may happen to a failed step, in display order.
func OnFails() []string { return []string{OnFailAsk, OnFailRetry, OnFailStop} }

// nameRE is what a workflow or a step may be called: a word a human types
// after `workflow`, and an identifier a template can reach through
// {{.Steps.<name>.Report}}.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// MaxSteps bounds a workflow. It is a guard on the planner more than on a
// human writing config: a plan with forty steps is a plan that went wrong,
// and every step is a real agent turn somebody pays for.
const MaxSteps = 12

// Validate checks a definition on its own terms and against the agents
// that exist. known reports whether a definition name is real; a nil
// known skips that check, which is what config validation does before the
// store has been seeded.
//
// It is the one gate both halves go through: a workflow read from
// config.toml and a workflow the planner wrote for one thread are the same
// struct and are checked here, so there is no second, looser path for the
// generated one.
func Validate(d Definition, known func(string) bool) error {
	if !nameRE.MatchString(d.Name) {
		return fmt.Errorf("workflow name %q: letters, digits, - and _, starting with a letter or digit", d.Name)
	}
	if len(d.Steps) == 0 {
		return fmt.Errorf("workflow %q has no steps", d.Name)
	}
	if len(d.Steps) > MaxSteps {
		return fmt.Errorf("workflow %q has %d steps; at most %d", d.Name, len(d.Steps), MaxSteps)
	}
	seen := map[string]bool{}
	for i := range d.Steps {
		s := &d.Steps[i]
		where := fmt.Sprintf("workflow %q step %d", d.Name, i+1)
		if s.Name != "" {
			where = fmt.Sprintf("workflow %q step %q", d.Name, s.Name)
		}
		if !nameRE.MatchString(s.Name) {
			return fmt.Errorf("%s: name must be letters, digits, - and _", where)
		}
		if seen[s.Name] {
			return fmt.Errorf("%s: two steps with that name; a prompt could not tell them apart", where)
		}
		seen[s.Name] = true
		if err := validateStep(s, where, known); err != nil {
			return err
		}
	}
	return nil
}

func validateStep(s *Step, where string, known func(string) bool) error {
	shapes := 0
	for _, set := range []bool{strings.TrimSpace(s.Prompt) != "", s.Builtin != "", strings.TrimSpace(s.Gate) != ""} {
		if set {
			shapes++
		}
	}
	switch {
	case shapes == 0:
		return fmt.Errorf("%s: needs a prompt, a builtin (%s) or a gate", where, strings.Join(Builtins(), ", "))
	case shapes > 1:
		return fmt.Errorf("%s: pick one of prompt, builtin and gate — a step does one thing", where)
	}
	if s.Gate != "" {
		// A gate asks a human and runs no turn, so there is nothing for
		// the rest of the fields to be about. Saying so is better than
		// quietly ignoring them.
		if s.Agent != "" || s.Model != "" || s.Thread != "" || s.Check != "" ||
			(s.Expect != "" && s.Expect != ExpectNone) {
			return fmt.Errorf("%s: a gate asks a human and runs no turn, so it takes none of agent, model, thread, check or expect", where)
		}
		if _, err := template.New("gate").Parse(s.Gate); err != nil {
			return fmt.Errorf("%s: gate: %w", where, err)
		}
		return nil
	}
	if s.Builtin != "" && !contains(Builtins(), s.Builtin) {
		return fmt.Errorf("%s: unknown builtin %q (%s)", where, s.Builtin, strings.Join(Builtins(), ", "))
	}
	if s.Thread != "" && !contains(Threads(), s.Thread) {
		return fmt.Errorf("%s: thread is %s", where, strings.Join(Threads(), " or "))
	}
	if s.Builtin == BuiltinReview && s.Thread == ThreadSame {
		// review opens a thread beside this one by definition: a review
		// from the session that wrote the code is worth nothing.
		return fmt.Errorf("%s: the review builtin always opens a thread of its own", where)
	}
	if s.Expect != "" && !contains(Expects(), s.Expect) {
		return fmt.Errorf("%s: unknown expect %q (%s)", where, s.Expect, strings.Join(Expects(), ", "))
	}
	if s.OnFail != "" && !contains(OnFails(), s.OnFail) {
		return fmt.Errorf("%s: unknown on_fail %q (%s)", where, s.OnFail, strings.Join(OnFails(), ", "))
	}
	if s.MaxRetries < 0 {
		return fmt.Errorf("%s: max_retries cannot be negative", where)
	}
	if s.Agent != "" && known != nil && !known(s.Agent) {
		return fmt.Errorf("%s: no agent called %q", where, s.Agent)
	}
	for what, tmpl := range map[string]string{"prompt": s.Prompt, "check": s.Check} {
		if strings.TrimSpace(tmpl) == "" {
			continue
		}
		if _, err := template.New(what).Parse(tmpl); err != nil {
			return fmt.Errorf("%s: %s: %w", where, what, err)
		}
	}
	return nil
}

func contains(all []string, s string) bool {
	for _, v := range all {
		if v == s {
			return true
		}
	}
	return false
}

// Where says where a step runs, filling in the default: a step of its own
// is the exception, and `review` is the one built-in that insists on it.
func (s Step) Where() string {
	if s.Thread != "" {
		return s.Thread
	}
	if s.Builtin == BuiltinReview {
		return ThreadNew
	}
	return ThreadSame
}

// Judged says what the step is checked against, filling in the default.
// A step that claims nothing gets ExpectNone, which claims nothing.
func (s Step) Judged() string {
	if s.Expect != "" {
		return s.Expect
	}
	switch s.Builtin {
	case BuiltinMerge:
		return ExpectMerged
	case BuiltinReview:
		return ExpectReport
	}
	return ExpectNone
}

// Failure says what happens when the step is not done, filling in the
// default: put it to the thread, which is the only answer that is never
// wrong.
func (s Step) Failure() string {
	if s.OnFail != "" {
		return s.OnFail
	}
	return OnFailAsk
}

// Tries is how many attempts the step gets, at least one.
func (s Step) Tries() int {
	if s.Failure() != OnFailRetry {
		return 1
	}
	if s.MaxRetries > 0 {
		return s.MaxRetries + 1
	}
	return DefaultMaxRetries + 1
}

// Data is what a step's prompt and check are rendered against.
type Data struct {
	// Ask is what the human asked for when the workflow started.
	Ask string
	// Repo, Branch, PR and Issue are what the thread is working on right
	// now (internal/work), re-read before every step and never cached:
	// the pull request only exists once a step has opened one.
	//
	// PR is written the way a command line can take it — the URL when the
	// log ever saw one, the bare number when it only ever saw "#51" —
	// because a prompt's whole job here is to be pasted into `gh`. "#51"
	// on a command line is the start of a shell comment.
	Repo   string
	Branch string
	PR     string
	Issue  string
	// Steps is what each finished step left behind, by name.
	Steps map[string]StepData
	// Workflow and Step name the workflow and the step being rendered.
	Workflow string
	Step     string
}

// StepData is one finished step, as a later step's prompt sees it.
type StepData struct {
	// Report is the agent's last text on that step's turn, trimmed to
	// MaxReport. It is the whole point of ExpectReport.
	Report string
	// Thread is the thread the step ran on, which is the same as the
	// workflow's unless the step asked for one of its own.
	Thread string
	// OK is whether the step was judged done.
	OK bool
}

// MaxReport bounds a report on its way into the next step's prompt. A
// review is prose and prose has no bound; the next prompt does.
const MaxReport = 8 << 10

// Trim cuts a report to MaxReport at a rune boundary and says that it did
// — a prompt that was silently truncated reads as a report that stopped
// mid-sentence for no reason, and the agent has no way to know.
func Trim(s string) string {
	if len(s) <= MaxReport {
		return s
	}
	cut := MaxReport
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "\n\n[…trimmed by dispatch: the report was longer than this step's prompt can carry]"
}

// ErrNoTemplate is returned by Render for a step with nothing to render.
var ErrNoTemplate = errors.New("workflow: the step has no prompt")

// Render fills in a step's prompt. A template that reaches for a step that
// has not run yet gets an empty string rather than an error: a workflow
// whose author wrote {{.Steps.review.Report}} into step one has made a
// mistake worth reading in the thread, not a crash.
func Render(tmpl string, d Data) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		return "", ErrNoTemplate
	}
	t, err := template.New("step").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Check is what a step's own command came back with.
type Check struct {
	OK     bool
	Output string // the tail of what it printed, for the failure message
}

// A judgement, for ExpectJudge.
const (
	JudgePass = "pass"
	JudgeFail = "fail"
	JudgeAsk  = "ask" // nobody could say: the static answer, which becomes a gate
)

// Judgements lists what a judge may answer, in the order a decider is
// offered them.
func Judgements() []string { return []string{JudgePass, JudgeFail, JudgeAsk} }

// Evidence is everything known about a step that has just run, in facts
// rather than in the shape internal/work mines them in. The translation
// happens at the boundary (the coordinator) on purpose: Judge's job is
// "given these, is the step done", not "understand how a sighting is
// graded", and it keeps this package depending on nothing.
type Evidence struct {
	// Failed says the turn did not finish on its own: it errored, was
	// cancelled, or never reached the agent at all.
	Failed bool
	// Opened is a pull request *this step* opened — the log caught the
	// URL coming back from `gh pr create`. 0 when it opened none.
	Opened int
	// Saw is a pull request this step named at all, however weakly. It
	// only ever improves the failure message: a step that was asked to
	// open one and merely looked at one should be told so.
	Saw int
	// Pushed says this step's log saw a branch reach the remote. A
	// `git switch -c` is not a push.
	Pushed bool
	// Merged is the pull request gh confirmed merged in this step, by
	// its own "Merged pull request" and nothing else. 0 for none.
	Merged int
	// Carried is the pull request the workflow is on, from an earlier
	// step; 0 before any step named one. A merge is only this workflow's
	// merge when the two agree — and when the thread never had a number
	// to compare against, there is one pull request in play and nothing
	// to contradict.
	Carried int
	// Report is the agent's last text this turn.
	Report string
	// Check is what Step.Check came back with, nil when the step asked
	// for none.
	Check *Check
	// Judged is JudgePass or JudgeFail for ExpectJudge, empty when
	// nobody has said yet.
	Judged string
}

// Judge says whether a step is done, and why not when it is not. The
// reason is shown in the thread and handed to a retry, so it says what was
// expected and what the log actually showed.
func Judge(s Step, ev Evidence) (bool, string) {
	if ev.Failed {
		return false, "the turn did not finish"
	}
	if strings.TrimSpace(s.Check) != "" {
		switch {
		case ev.Check == nil:
			return false, "the check never ran"
		case !ev.Check.OK:
			return false, "the check failed" + tail(ev.Check.Output)
		}
	}
	switch s.Judged() {
	case ExpectNone:
		return true, ""
	case ExpectReport:
		if strings.TrimSpace(ev.Report) == "" {
			return false, "the turn ended without saying anything"
		}
		return true, ""
	case ExpectPR:
		switch {
		case ev.Opened != 0:
			return true, ""
		case ev.Saw != 0:
			return false, fmt.Sprintf("#%d was looked at, not opened, in this step", ev.Saw)
		}
		return false, "no pull request: nothing in this step's log opened one"
	case ExpectPush:
		if !ev.Pushed {
			return false, "nothing in this step's log reached the remote — `git switch -c` is not a push"
		}
		return true, ""
	case ExpectMerged:
		if ev.Merged == 0 {
			return false, "the log does not show a merge gh confirmed"
		}
		if ev.Carried != 0 && ev.Merged != ev.Carried {
			return false, fmt.Sprintf("gh confirmed #%d merged, but this workflow is on #%d", ev.Merged, ev.Carried)
		}
		return true, ""
	case ExpectJudge:
		switch ev.Judged {
		case JudgePass:
			return true, ""
		case JudgeFail:
			return false, "judged not done"
		}
		return false, "nobody has judged this step"
	}
	return true, ""
}

// tail is the last of a check's output, for a one-line failure.
func tail(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if n := len(lines); n > 3 {
		lines = lines[n-3:]
	}
	return ": " + strings.TrimSpace(strings.Join(lines, " · "))
}

// Run statuses.
const (
	RunRunning = "running" // a step is in flight
	RunWaiting = "waiting" // a gate, or a failed step, is waiting for a human
	RunDone    = "done"    // every step passed
	RunFailed  = "failed"  // a step was not done and nobody rescued it
	RunStopped = "stopped" // a human stopped it
)

// Step statuses.
const (
	StepPending = "pending"
	StepRunning = "running"
	StepPassed  = "passed"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// State is one workflow running on one thread. It is a projection: the
// coordinator writes it after every transition and replays it on a
// restart, so a workflow survives dispatch stopping in the middle of it.
type State struct {
	// Thread is the workflow's home: where it was started, where its
	// progress is posted, and where a step that did not ask for a thread
	// of its own runs.
	Thread    transport.ThreadID `json:"thread"`
	Transport string             `json:"transport"`
	Surface   string             `json:"surface"`
	// Def is the steps as they will run. A workflow from config carries a
	// copy rather than the name alone, so editing config.toml cannot
	// change what a run already under way is doing.
	Def  Definition `json:"def"`
	Ask  string     `json:"ask"`
	User string     `json:"user"`
	// Step is the index of the step being run or waited on.
	Step   int         `json:"step"`
	Status string      `json:"status"`
	Steps  []StepState `json:"steps"`
	// PR is the pull request the workflow is carrying, once a step has
	// named one. It is what ExpectMerged is checked against.
	PR        int       `json:"pr,omitempty"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StepState is what happened to one step.
type StepState struct {
	Name   string             `json:"name"`
	Status string             `json:"status"`
	Thread transport.ThreadID `json:"thread,omitempty"`
	Task   executor.TaskID    `json:"task,omitempty"`
	// Since is the log seq the step began at: the floor its turn is
	// recognised by and the window its evidence is scanned from.
	Since  int64  `json:"since,omitempty"`
	Report string `json:"report,omitempty"`
	Detail string `json:"detail,omitempty"` // why it failed
	Tries  int    `json:"tries,omitempty"`
}

// Start makes the state a run begins with.
func Start(def Definition, th transport.ThreadID, transportName, surfaceName, ask, user string, now time.Time) *State {
	st := &State{
		Thread: th, Transport: transportName, Surface: surfaceName,
		Def: def, Ask: ask, User: user,
		Status: RunRunning, StartedAt: now, UpdatedAt: now,
	}
	for _, s := range def.Steps {
		st.Steps = append(st.Steps, StepState{Name: s.Name, Status: StepPending})
	}
	return st
}

// Live says the workflow still has somewhere to go.
func (s *State) Live() bool { return s.Status == RunRunning || s.Status == RunWaiting }

// Current is the step being run or waited on, nil past the end.
func (s *State) Current() *Step {
	if s.Step < 0 || s.Step >= len(s.Def.Steps) {
		return nil
	}
	return &s.Def.Steps[s.Step]
}

// CurrentState is the record of the step being run, nil past the end.
func (s *State) CurrentState() *StepState {
	if s.Step < 0 || s.Step >= len(s.Steps) {
		return nil
	}
	return &s.Steps[s.Step]
}

// Data is what the next prompt is rendered against. The caller passes
// what the thread is working on as plain strings — the coordinator reads
// them out of internal/work — so this package does no mining of its own.
//
// pr is written the way a command line can take it, because a prompt's
// whole job here is to be pasted into `gh`.
func (s *State) Data(repo, branch, pr, issue string) Data {
	d := Data{
		Ask: s.Ask, Repo: repo, Branch: branch, PR: pr, Issue: issue,
		Workflow: s.Def.Name, Steps: map[string]StepData{},
	}
	if cur := s.Current(); cur != nil {
		d.Step = cur.Name
	}
	for i, ss := range s.Steps {
		if i >= s.Step && ss.Status == StepPending {
			continue // has not run: {{.Steps.later.Report}} is empty, not an error
		}
		d.Steps[ss.Name] = StepData{
			Report: Trim(ss.Report),
			Thread: string(ss.Thread),
			OK:     ss.Status == StepPassed,
		}
	}
	return d
}

// Summary is the progress line: one glyph per step, the current one named.
func (s *State) Summary() string {
	var b strings.Builder
	for i, ss := range s.Steps {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(glyph(ss.Status))
	}
	b.WriteString("  ")
	switch s.Status {
	case RunDone:
		b.WriteString(fmt.Sprintf("done — %d steps", len(s.Steps)))
	case RunFailed, RunStopped:
		name := "?"
		if cs := s.CurrentState(); cs != nil {
			name = cs.Name
		}
		b.WriteString(s.Status + " at " + name)
	default:
		name := "?"
		if cs := s.CurrentState(); cs != nil {
			name = cs.Name
		}
		b.WriteString(fmt.Sprintf("step %d of %d: %s", s.Step+1, len(s.Steps), name))
	}
	return b.String()
}

func glyph(status string) string {
	switch status {
	case StepPassed:
		return "✅"
	case StepFailed:
		return "❌"
	case StepSkipped:
		return "⏭️"
	case StepRunning:
		return "⏳"
	}
	return "•"
}
