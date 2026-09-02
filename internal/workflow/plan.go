package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A workflow does not have to be written down in advance. Someone can say
// what they want and how they want it done, and dispatch can compose the
// steps out of the agents that exist — an *on-demand* workflow.
//
// One rule makes that safe, and it is the rule the decider already
// established: the generated thing takes the same path as the written
// one. A plan is a Definition, it goes through the same Validate, it runs
// on the same runner. There is no second, looser road for a workflow a
// model wrote, so everything this package refuses in config it refuses
// here — a step naming an agent that does not exist, an expect nobody
// implements, two steps with one name, a workflow forty steps long.
//
// Schema is the other half of it. The vocabulary handed to the planner is
// generated from the same constants Validate checks against, so the prompt
// cannot drift away from what the validator will accept: adding an expect
// adds it to both at once, and TestSchemaNamesEveryConstant keeps a new
// one from being added to only one.

// PlanName is what an on-demand workflow is called. It is not a name
// anybody types — the workflow is started by the message that planned it —
// but Validate insists on one, and a run says it in every line it posts.
const PlanName = "plan"

// Schema is the vocabulary a planner is given: every field of a step, and
// every value each of them accepts. It is built from this package's own
// constants, so it says exactly what Validate will accept and cannot fall
// behind it.
func Schema() string {
	var b strings.Builder
	b.WriteString(`A workflow is {"name": string, "description": string, "steps": [step, ...]}.

A step is one of three shapes, and it must be exactly one:

  a prompt step   {"name": ..., "prompt": "what to ask the agent", ...}
  a builtin step  {"name": ..., "builtin": "` + strings.Join(Builtins(), `" | "`) + `"}
  a gate step     {"name": ..., "gate": "the question to put to a human"}

Fields:

  "name"        required. Letters, digits, - and _. Later steps refer to
                this one by it: {{.Steps.<name>.Report}} is what it said.
  "agent"       which agent definition runs the step. Omit for the one the
                thread is already using. Naming a different definition is
                how a step runs on a different model or a different agent.
  "model"       override that definition's model for this step only.
  "thread"      "` + strings.Join(Threads(), `" | "`) + `". "` + ThreadNew + `" gives the step a session that
                has never seen the work, which is the only kind of reviewer
                worth having. "` + ThreadSame + `" continues the conversation.
  "prompt"      a Go template. See the data below.
  "check"       a shell command dispatch runs itself in the step's
                environment when the turn ends. Exit 0 or the step failed.
                This is how "the tests pass" is known — the agent is not
                asked.
  "expect"      what the log must show for the step to count as done:
`)
	for _, e := range Expects() {
		fmt.Fprintf(&b, "                  %-8s %s\n", `"`+e+`"`, expectHelp(e))
	}
	b.WriteString(`  "on_fail"     what happens when the step is not done:
`)
	for _, f := range OnFails() {
		fmt.Fprintf(&b, "                  %-8s %s\n", `"`+f+`"`, onFailHelp(f))
	}
	b.WriteString(`  "max_retries" bounds "` + OnFailRetry + `".

A prompt is rendered against:

  {{.Ask}}                  what the human asked for, verbatim
  {{.Repo}} {{.Branch}}     what the thread is working on right now
  {{.PR}} {{.Issue}}        written so they can be pasted into a command
  {{.Steps.<name>.Report}}  what an earlier step said
  {{.Steps.<name>.OK}}      whether it was judged done

`)
	fmt.Fprintf(&b, "At most %d steps.\n", MaxSteps)
	return b.String()
}

func expectHelp(e string) string {
	switch e {
	case ExpectNone:
		return "nothing beyond the turn ending. The default."
	case ExpectReport:
		return "the turn said something, for a later step to read."
	case ExpectPR:
		return "this step opened a pull request (gh returned the URL)."
	case ExpectPush:
		return "this step pushed a branch and the remote answered."
	case ExpectMerged:
		return "gh confirmed this step merged the pull request."
	case ExpectJudge:
		return "somebody has to say. Without a decider this asks a human."
	}
	return ""
}

func onFailHelp(f string) string {
	switch f {
	case OnFailAsk:
		return "put it to the thread: retry, skip, stop. The default."
	case OnFailRetry:
		return "say what was missing and ask the same agent again."
	case OnFailStop:
		return "the workflow halts where it is."
	}
	return ""
}

// Agents is the list of agent definitions a planner may choose from,
// written for the prompt. It names only what exists, which is the same
// set Validate checks a step's agent against.
func Agents(names []string, describe func(string) string) string {
	if len(names) == 0 {
		return "(none — every step must leave \"agent\" out)"
	}
	var b strings.Builder
	for _, n := range names {
		b.WriteString("  " + n)
		if describe != nil {
			if d := describe(n); d != "" {
				b.WriteString(" — " + d)
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// MaxPlan bounds the reply a planner may be believed. A plan is a page of
// JSON; anything past this is a model that has started writing an essay,
// and reading megabytes of it to find that out helps nobody.
const MaxPlan = 32 << 10

// ParsePlan reads a planner's reply into a Definition.
//
// It tolerates prose around the object, the way a model writes one, for
// the reason the decider does: asking again costs a turn, and the first
// "{" to the last "}" is the object in every reply that has one. What it
// does not tolerate is the plan naming itself something else — the name
// is dispatch's to give — or steps arriving as anything but a list.
func ParsePlan(text string) (Definition, error) {
	if len(text) > MaxPlan {
		return Definition{}, fmt.Errorf("workflow: the planner wrote %d bytes; a plan is at most %d", len(text), MaxPlan)
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return Definition{}, fmt.Errorf("workflow: no JSON object in the plan: %s", clip(strings.TrimSpace(text), 200))
	}
	var d Definition
	if err := json.Unmarshal([]byte(text[start:end+1]), &d); err != nil {
		return Definition{}, fmt.Errorf("workflow: parse plan: %w", err)
	}
	// The name is dispatch's, not the planner's: a plan runs on the
	// thread that asked for it and is never looked up by name, and one
	// that called itself "merge" would read as the built-in word in every
	// line the run posts.
	d.Name = PlanName
	if len(d.Steps) == 0 {
		return Definition{}, fmt.Errorf("workflow: the plan has no steps")
	}
	return d, nil
}

// Describe writes a plan out for a human to approve: one line per step,
// saying who runs it, where, and what will make it count as done. It is
// what stands between a model's idea and five real agent turns, so it says
// what each step *is* rather than quoting its prompt back.
func Describe(d Definition) string {
	var b strings.Builder
	if d.Description != "" {
		b.WriteString("_" + d.Description + "_\n")
	}
	for i := range d.Steps {
		s := &d.Steps[i]
		fmt.Fprintf(&b, "%d. *%s* — %s\n", i+1, s.Name, describeStep(s))
	}
	return strings.TrimRight(b.String(), "\n")
}

func describeStep(s *Step) string {
	if s.Gate != "" {
		return "asks a human: " + oneLine(s.Gate, 120)
	}
	var parts []string
	switch {
	case s.Builtin != "":
		parts = append(parts, "`"+s.Builtin+"`")
	default:
		parts = append(parts, oneLine(s.Prompt, 100))
	}
	who := s.Agent
	if s.Model != "" {
		if who == "" {
			who = "the thread's agent"
		}
		who += " on " + s.Model
	}
	if who != "" {
		parts = append(parts, "with "+who)
	}
	if s.Where() == ThreadNew {
		parts = append(parts, "in a thread of its own")
	}
	if s.Check != "" {
		parts = append(parts, "checked by `"+oneLine(s.Check, 60)+"`")
	}
	if e := s.Judged(); e != ExpectNone {
		parts = append(parts, "done when: "+expectShort(e))
	}
	if s.Failure() != OnFailAsk {
		parts = append(parts, "on failure: "+s.Failure())
	}
	return strings.Join(parts, " · ")
}

func expectShort(e string) string {
	switch e {
	case ExpectReport:
		return "it reported"
	case ExpectPR:
		return "a pull request was opened"
	case ExpectPush:
		return "a branch was pushed"
	case ExpectMerged:
		return "gh confirmed the merge"
	case ExpectJudge:
		return "somebody says so"
	}
	return e
}

// oneLine flattens a prompt into something that fits on a line of chat.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, max)
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
