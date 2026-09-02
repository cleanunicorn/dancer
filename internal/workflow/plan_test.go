package workflow

import (
	"strings"
	"testing"
)

// The vocabulary the planner is given is generated from the constants
// Validate checks against, so the two cannot drift: adding an expect that
// the schema never mentions would let a planner be refused for a value it
// was never offered, and adding one to the schema alone would have it
// refused for a value it was told to use.
func TestSchemaNamesEveryConstant(t *testing.T) {
	schema := Schema()
	for _, group := range [][]string{Threads(), Builtins(), Expects(), OnFails()} {
		for _, v := range group {
			if !strings.Contains(schema, `"`+v+`"`) {
				t.Errorf("Schema never mentions %q", v)
			}
		}
	}
	for _, field := range []string{"name", "agent", "model", "thread", "prompt", "check", "expect", "on_fail", "max_retries", "gate", "builtin"} {
		if !strings.Contains(schema, `"`+field+`"`) {
			t.Errorf("Schema never mentions the %q field", field)
		}
	}
	// Every expect and on_fail is explained, not just listed.
	for _, e := range Expects() {
		if expectHelp(e) == "" {
			t.Errorf("expect %q has no explanation in the schema", e)
		}
	}
	for _, f := range OnFails() {
		if onFailHelp(f) == "" {
			t.Errorf("on_fail %q has no explanation in the schema", f)
		}
	}
}

// A plan arrives the way a model writes one: prose around one object.
func TestParsePlanReadsPastTheProse(t *testing.T) {
	d, err := ParsePlan("Sure! Here is the workflow:\n\n```json\n" +
		`{"name":"whatever","description":"d","steps":[{"name":"a","prompt":"go"}]}` +
		"\n```\nLet me know if you want changes.")
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(d.Steps) != 1 || d.Steps[0].Name != "a" {
		t.Fatalf("ParsePlan = %+v", d)
	}
	// The name is dispatch's to give: a plan that called itself "merge"
	// would read as the built-in word in every line the run posts.
	if d.Name != PlanName {
		t.Errorf("ParsePlan kept the planner's name %q", d.Name)
	}
}

func TestParsePlanRefuses(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"no object", "I could not work out any steps.", "no JSON object"},
		{"not JSON", "{not json at all}", "parse plan"},
		{"no steps", `{"name":"x","steps":[]}`, "no steps"},
		{"too long", "{" + strings.Repeat("x", MaxPlan) + "}", "at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePlan(tc.text); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParsePlan = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A plan that parses still goes through the gate config workflows go
// through. This is the whole safety argument for on-demand workflows, so
// it is worth a test of its own rather than trusting the caller.
func TestAPlannedWorkflowIsValidatedLikeAWrittenOne(t *testing.T) {
	d, err := ParsePlan(`{"steps":[{"name":"a","agent":"ghost","prompt":"go"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(d, func(string) bool { return false }); err == nil {
		t.Fatal("Validate accepted a planned step naming an agent that does not exist")
	}
	if err := Validate(d, func(string) bool { return true }); err != nil {
		t.Fatalf("Validate refused a good plan: %v", err)
	}
}

// Describe is what stands between a model's idea and five real agent
// turns, so it says what each step *is*: who runs it, where, and what
// will make it count as done.
func TestDescribe(t *testing.T) {
	d := Definition{Name: PlanName, Description: "implement then review", Steps: []Step{
		{Name: "implement", Agent: "coder", Model: "opus", Prompt: "{{.Ask}}", Expect: ExpectPR, Check: "make test"},
		{Name: "review", Agent: "reviewer", Thread: ThreadNew, Prompt: "Review it", Expect: ExpectReport},
		{Name: "fix", Prompt: "Fix it", Expect: ExpectPush, OnFail: OnFailRetry},
		{Name: "approve", Gate: "Ship it?"},
		{Name: "merge", Builtin: BuiltinMerge},
	}}
	got := Describe(d)
	for _, want := range []string{
		"implement then review",
		"1. *implement*", "with coder on opus", "a pull request was opened", "checked by `make test`",
		"2. *review*", "in a thread of its own", "it reported",
		"3. *fix*", "on failure: retry",
		"4. *approve*", "asks a human: Ship it?",
		"5. *merge*", "`merge`", "gh confirmed the merge",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe never says %q:\n%s", want, got)
		}
	}
}

// A long prompt is flattened onto one line rather than pouring a page of
// template into a chat message somebody has to read before saying yes.
func TestDescribeFlattensAPrompt(t *testing.T) {
	long := "line one\n\nline two " + strings.Repeat("word ", 200)
	got := Describe(Definition{Name: PlanName, Steps: []Step{{Name: "a", Prompt: long}}})
	if strings.Count(got, "\n") != 0 {
		t.Errorf("Describe wrapped a prompt over lines:\n%s", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("Describe did not clip a long prompt: %q", got)
	}
}

func TestAgentsListsOnlyWhatExists(t *testing.T) {
	if got := Agents(nil, nil); !strings.Contains(got, "none") {
		t.Errorf("Agents(nil) = %q", got)
	}
	got := Agents([]string{"coder", "reviewer"}, func(n string) string {
		if n == "coder" {
			return "claude/opus"
		}
		return ""
	})
	if !strings.Contains(got, "coder — claude/opus") || !strings.Contains(got, "reviewer") {
		t.Errorf("Agents = %q", got)
	}
}
