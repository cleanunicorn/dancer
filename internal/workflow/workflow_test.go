package workflow

import (
	"strings"
	"testing"
	"time"
)

func feature() Definition {
	return Definition{Name: "feature", Steps: []Step{
		{Name: "implement", Agent: "coder", Prompt: "{{.Ask}}\n\nOpen a pull request.", Expect: ExpectPR},
		{Name: "review", Agent: "reviewer", Thread: ThreadNew, Prompt: "Review {{.PR}}.", Expect: ExpectReport},
		{Name: "fix", Prompt: "The reviewer said:\n{{.Steps.review.Report}}", Expect: ExpectPush},
		{Name: "approve", Gate: "Merge {{.PR}}?"},
		{Name: "merge", Builtin: BuiltinMerge},
	}}
}

func TestValidateAcceptsTheWholeExample(t *testing.T) {
	known := func(s string) bool { return s == "coder" || s == "reviewer" }
	if err := Validate(feature(), known); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRefuses(t *testing.T) {
	cases := []struct {
		name string
		def  Definition
		want string
	}{
		{"no name", Definition{Steps: []Step{{Name: "a", Prompt: "x"}}}, "workflow name"},
		{"no steps", Definition{Name: "w"}, "no steps"},
		{"unnamed step", Definition{Name: "w", Steps: []Step{{Prompt: "x"}}}, "name must be"},
		{"duplicate step", Definition{Name: "w", Steps: []Step{{Name: "a", Prompt: "x"}, {Name: "a", Prompt: "y"}}}, "two steps with that name"},
		{"empty step", Definition{Name: "w", Steps: []Step{{Name: "a"}}}, "needs a prompt"},
		{"two shapes", Definition{Name: "w", Steps: []Step{{Name: "a", Prompt: "x", Builtin: BuiltinMerge}}}, "a step does one thing"},
		{"unknown builtin", Definition{Name: "w", Steps: []Step{{Name: "a", Builtin: "deploy"}}}, "unknown builtin"},
		{"unknown expect", Definition{Name: "w", Steps: []Step{{Name: "a", Prompt: "x", Expect: "green"}}}, "unknown expect"},
		{"unknown on_fail", Definition{Name: "w", Steps: []Step{{Name: "a", Prompt: "x", OnFail: "shrug"}}}, "unknown on_fail"},
		{"unknown thread", Definition{Name: "w", Steps: []Step{{Name: "a", Prompt: "x", Thread: "beside"}}}, "thread is"},
		{"review on the same thread", Definition{Name: "w", Steps: []Step{{Name: "a", Builtin: BuiltinReview, Thread: ThreadSame}}}, "thread of its own"},
		{"gate with an agent", Definition{Name: "w", Steps: []Step{{Name: "a", Gate: "ok?", Agent: "coder"}}}, "runs no turn"},
		{"broken template", Definition{Name: "w", Steps: []Step{{Name: "a", Prompt: "{{.Ask"}}}, "prompt:"},
		{"broken check", Definition{Name: "w", Steps: []Step{{Name: "a", Prompt: "x", Check: "{{.Branch"}}}, "check:"},
		{"negative retries", Definition{Name: "w", Steps: []Step{{Name: "a", Prompt: "x", MaxRetries: -1}}}, "max_retries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.def, nil)
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The planner's output goes through the same gate as config, so a step
// naming an agent that does not exist is refused whoever wrote it.
func TestValidateRefusesAnAgentThatDoesNotExist(t *testing.T) {
	def := Definition{Name: "w", Steps: []Step{{Name: "a", Agent: "ghost", Prompt: "x"}}}
	err := Validate(def, func(string) bool { return false })
	if err == nil || !strings.Contains(err.Error(), `no agent called "ghost"`) {
		t.Fatalf("Validate = %v, want it to refuse the agent", err)
	}
}

func TestValidateBoundsTheStepCount(t *testing.T) {
	def := Definition{Name: "w"}
	for i := 0; i < MaxSteps+1; i++ {
		def.Steps = append(def.Steps, Step{Name: "s" + string(rune('a'+i)), Prompt: "x"})
	}
	if err := Validate(def, nil); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("Validate = %v, want it to bound the steps", err)
	}
}

func TestDefaults(t *testing.T) {
	review := Step{Name: "r", Builtin: BuiltinReview}
	if review.Where() != ThreadNew {
		t.Errorf("review runs on %q, want a thread of its own", review.Where())
	}
	if review.Judged() != ExpectReport {
		t.Errorf("review is judged on %q, want %q", review.Judged(), ExpectReport)
	}
	merge := Step{Name: "m", Builtin: BuiltinMerge}
	if merge.Judged() != ExpectMerged {
		t.Errorf("merge is judged on %q, want %q", merge.Judged(), ExpectMerged)
	}
	if merge.Where() != ThreadSame {
		t.Errorf("merge runs on %q, want the thread it is on", merge.Where())
	}
	plain := Step{Name: "p", Prompt: "x"}
	if plain.Judged() != ExpectNone || plain.Failure() != OnFailAsk || plain.Tries() != 1 {
		t.Errorf("a plain step defaults wrong: %q %q %d", plain.Judged(), plain.Failure(), plain.Tries())
	}
	retry := Step{Name: "p", Prompt: "x", OnFail: OnFailRetry}
	if retry.Tries() != DefaultMaxRetries+1 {
		t.Errorf("retry gets %d tries, want %d", retry.Tries(), DefaultMaxRetries+1)
	}
	if (Step{Name: "p", Prompt: "x", OnFail: OnFailRetry, MaxRetries: 5}).Tries() != 6 {
		t.Error("max_retries is not respected")
	}
}

func TestJudge(t *testing.T) {
	cases := []struct {
		name string
		step Step
		ev   Evidence
		ok   bool
		why  string
	}{
		{"a failed turn fails whatever else is true", Step{Expect: ExpectNone}, Evidence{Failed: true}, false, "did not finish"},
		{"none asks nothing", Step{Expect: ExpectNone}, Evidence{}, true, ""},
		{"report wants text", Step{Expect: ExpectReport}, Evidence{}, false, "without saying anything"},
		{"report is happy with text", Step{Expect: ExpectReport}, Evidence{Report: "looks fine"}, true, ""},
		{"pr wants one opened here", Step{Expect: ExpectPR}, Evidence{Opened: 51}, true, ""},
		{"pr refuses one merely read", Step{Expect: ExpectPR}, Evidence{Saw: 51}, false, "looked at, not opened"},
		{"pr refuses nothing at all", Step{Expect: ExpectPR}, Evidence{}, false, "no pull request"},
		{"push wants the remote to have answered", Step{Expect: ExpectPush}, Evidence{}, false, "not a push"},
		{"push is happy once pushed", Step{Expect: ExpectPush}, Evidence{Pushed: true}, true, ""},
		{"merged wants gh's own word", Step{Expect: ExpectMerged}, Evidence{Opened: 51}, false, "does not show a merge"},
		{"merged is happy on the confirmation", Step{Expect: ExpectMerged}, Evidence{Merged: 51, Carried: 51}, true, ""},
		{"judge waits for somebody", Step{Expect: ExpectJudge}, Evidence{}, false, "nobody has judged"},
		{"judge passes", Step{Expect: ExpectJudge}, Evidence{Judged: JudgePass}, true, ""},
		{"judge fails", Step{Expect: ExpectJudge}, Evidence{Judged: JudgeFail}, false, "judged not done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := Judge(tc.step, tc.ev)
			if ok != tc.ok {
				t.Fatalf("Judge = %v (%s), want %v", ok, why, tc.ok)
			}
			if tc.why != "" && !strings.Contains(why, tc.why) {
				t.Errorf("Judge said %q, want it to mention %q", why, tc.why)
			}
		})
	}
}

// A merge gh confirmed for a different pull request is not this
// workflow's merge, however loudly the agent reports success.
func TestJudgeRefusesAMergeOfSomethingElse(t *testing.T) {
	ok, why := Judge(Step{Expect: ExpectMerged}, Evidence{Merged: 52, Carried: 51})
	if ok {
		t.Fatal("Judge accepted a merge of #52 for a workflow on #51")
	}
	if !strings.Contains(why, "#52") || !strings.Contains(why, "#51") {
		t.Errorf("Judge said %q, want both numbers in it", why)
	}
}

// A thread that only ever knew a bare number has nothing to compare
// against, which is exactly the thread `merge` writes a bare number for.
func TestJudgeAcceptsAMergeAThreadCouldNotQualify(t *testing.T) {
	if ok, why := Judge(Step{Expect: ExpectMerged}, Evidence{Merged: 51}); !ok {
		t.Fatalf("Judge refused a merge with nothing to contradict it: %s", why)
	}
}

func TestJudgeRunsTheCheckBeforeTheExpectation(t *testing.T) {
	step := Step{Expect: ExpectNone, Check: "make test"}
	if ok, _ := Judge(step, Evidence{}); ok {
		t.Error("Judge passed a step whose check never ran")
	}
	ok, why := Judge(step, Evidence{Check: &Check{OK: false, Output: "a\nb\nFAIL ./internal/x"}})
	if ok {
		t.Fatal("Judge passed a failing check")
	}
	if !strings.Contains(why, "FAIL ./internal/x") {
		t.Errorf("Judge said %q, want the tail of the output in it", why)
	}
	if ok, why := Judge(step, Evidence{Check: &Check{OK: true}}); !ok {
		t.Errorf("Judge refused a passing check: %s", why)
	}
}

func TestRender(t *testing.T) {
	st := Start(feature(), "C/1.0", "slack", "chat-slack", "add a health endpoint", "U1", time.Now())
	st.Step = 2
	st.Steps[0].Status, st.Steps[1].Status = StepPassed, StepPassed
	st.Steps[1].Report = "one bug: the handler ignores ctx"
	d := st.Data("o/r", "health", "https://github.com/o/r/pull/51", "")
	got, err := Render(st.Def.Steps[2].Prompt, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "the handler ignores ctx") {
		t.Errorf("Render = %q, want the review's report in it", got)
	}
	pr, err := Render(st.Def.Steps[1].Prompt, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pr, "https://github.com/o/r/pull/51") {
		t.Errorf("Render = %q, want the pull request URL", pr)
	}
}

// A prompt that reaches for a step which has not run yet gets nothing,
// not a crash: it is a mistake worth reading in the thread.
func TestRenderOfAStepThatHasNotRun(t *testing.T) {
	st := Start(feature(), "C/1.0", "slack", "chat", "do it", "U1", time.Now())
	got, err := Render("before: {{.Steps.review.Report}}!", st.Data("", "", "", ""))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "before: !" {
		t.Errorf("Render = %q, want an empty report", got)
	}
}

func TestTrimSaysThatItTrimmed(t *testing.T) {
	long := strings.Repeat("é", MaxReport)
	got := Trim(long)
	if !strings.Contains(got, "trimmed by dispatch") {
		t.Error("Trim said nothing about trimming")
	}
	if !strings.HasPrefix(got, "é") || strings.Contains(got, "�") {
		t.Error("Trim cut in the middle of a rune")
	}
	if Trim("short") != "short" {
		t.Error("Trim touched something that fits")
	}
}

func TestSummary(t *testing.T) {
	st := Start(feature(), "C/1.0", "slack", "chat", "do it", "U1", time.Now())
	st.Steps[0].Status = StepPassed
	st.Step = 1
	if got := st.Summary(); !strings.Contains(got, "step 2 of 5: review") || !strings.HasPrefix(got, "✅") {
		t.Errorf("Summary = %q", got)
	}
	st.Status = RunDone
	if got := st.Summary(); !strings.Contains(got, "done") {
		t.Errorf("Summary = %q, want it to say done", got)
	}
}
