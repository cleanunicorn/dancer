package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// loadConfig writes text to a config file and loads it.
func loadConfig(t *testing.T, text string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

const workflowGood = `
[server]
db = "x.db"

[[definitions]]
name = "coder"
kind = "claude"

[[workflow]]
name = "feature"
description = "implement then approve"

  [[workflow.step]]
  name = "implement"
  agent = "coder"
  prompt = "{{.Ask}}\n\nOpen a pull request when the tests pass."
  expect = "pr"

  [[workflow.step]]
  name = "approve"
  gate = "{{.PR}} opened. Merge it?"
`

// TestWorkflowConfigParsesAndValidates: a workflow in config is the same
// struct the runner runs, checked by the same workflow.Validate — including
// that every agent it names is a definition the config has.
func TestWorkflowConfigParsesAndValidates(t *testing.T) {
	cfg, err := loadConfig(t, workflowGood)
	if err != nil {
		t.Fatal(err)
	}
	defs := cfg.WorkflowDefinitions()
	if len(defs) != 1 || defs[0].Name != "feature" || len(defs[0].Steps) != 2 {
		t.Fatalf("workflows = %+v", defs)
	}
	s := defs[0].Steps[0]
	if s.Agent != "coder" || s.Expect != workflow.ExpectPR || !strings.Contains(s.Prompt, "{{.Ask}}") {
		t.Fatalf("step one = %+v", s)
	}
	if err := workflow.Validate(defs[0], nil); err != nil {
		t.Fatalf("validate: %v", err)
	}

	for name, text := range map[string]string{
		"unknown agent": workflowGood + `
[[workflow]]
name = "broken"
  [[workflow.step]]
  name = "work"
  agent = "nosuch"
  prompt = "do it"
`,
		"duplicate name": workflowGood + `
[[workflow]]
name = "feature"
  [[workflow.step]]
  name = "work"
  prompt = "do it"
`,
		"step without a shape": `
[[workflow]]
name = "bare"
  [[workflow.step]]
  name = "work"
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(t, text); err == nil {
				t.Fatal("invalid workflow was accepted")
			}
		})
	}
}

// TestWorkflowFromDefinitionRoundTrips: the planner's shape and the file's
// shape translate both ways without losing a step.
func TestWorkflowFromDefinitionRoundTrips(t *testing.T) {
	d := workflow.Definition{Name: "feature", Description: "d", Steps: []workflow.Step{
		{Name: "implement", Agent: "coder", Model: "opus", Prompt: "{{.Ask}}", Expect: workflow.ExpectPR, OnFail: workflow.OnFailRetry, MaxRetries: 1},
		{Name: "review", Agent: "reviewer", Thread: workflow.ThreadNew, Builtin: "", Prompt: "Review {{.PR}}", Expect: workflow.ExpectReport},
		{Name: "merge", Builtin: workflow.BuiltinMerge},
		{Name: "approve", Gate: "Go?"},
	}}
	w := WorkflowFromDefinition(d)
	if w.Name != "feature" || len(w.Steps) != 4 {
		t.Fatalf("workflow = %+v", w)
	}
	if w.Steps[0].Model != "opus" || w.Steps[0].MaxRetries != 1 || w.Steps[1].Thread != workflow.ThreadNew || w.Steps[2].Builtin != workflow.BuiltinMerge || w.Steps[3].Gate != "Go?" {
		t.Fatalf("steps = %+v", w.Steps)
	}
	if got := w.definition(); len(got.Steps) != len(d.Steps) || got.Steps[2].Builtin != workflow.BuiltinMerge {
		t.Fatalf("round trip = %+v", got)
	}
}

// server.planner_agent has to be an agent that exists, for the reason a
// workflow step's agent does: `plan` would otherwise fail at the moment
// somebody used it rather than at the moment it was configured.
func TestPlannerAgentMustExist(t *testing.T) {
	_, err := loadConfig(t, strings.Replace(workflowGood, `db = "x.db"`, "db = \"x.db\"\nplanner_agent = \"ghost\"", 1))
	if err == nil || !strings.Contains(err.Error(), "planner_agent") {
		t.Fatalf("Load = %v, want it to refuse an unknown planner agent", err)
	}
	cfg, err := loadConfig(t, strings.Replace(workflowGood, `db = "x.db"`, "db = \"x.db\"\nplanner_agent = \"coder\"", 1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.PlannerAgent != "coder" {
		t.Errorf("planner_agent = %q", cfg.Server.PlannerAgent)
	}
}

// AppendWorkflow writes a plan back into config.toml, preserving the file
// and refusing anything the whole config would not load with — the same
// contract AppendDefinition has.
func TestAppendWorkflowKeepsTheFileAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := workflowGood + "\n# a comment somebody wrote\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := WorkflowFromDefinition(workflow.Definition{
		Name:        "shipit",
		Description: "planned in a thread",
		Steps: []workflow.Step{
			{Name: "implement", Agent: "coder", Prompt: "{{.Ask}}", Expect: workflow.ExpectPR},
			{Name: "merge", Builtin: workflow.BuiltinMerge},
		},
	})
	if err := AppendWorkflow(path, saved); err != nil {
		t.Fatalf("AppendWorkflow: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the config no longer loads: %v", err)
	}
	if len(cfg.Workflows) != 2 || cfg.Workflows[1].Name != "shipit" {
		t.Fatalf("workflows = %+v", cfg.Workflows)
	}
	if got := cfg.WorkflowDefinitions()[1]; len(got.Steps) != 2 || got.Steps[1].Builtin != workflow.BuiltinMerge {
		t.Errorf("the saved workflow came back wrong: %+v", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "# a comment somebody wrote") {
		t.Error("AppendWorkflow lost what was already in the file")
	}

	// A workflow the whole config would not load with is refused, and the
	// file is left as it was.
	before := string(body)
	bad := WorkflowFromDefinition(workflow.Definition{Name: "broken", Steps: []workflow.Step{{Name: "a", Agent: "ghost", Prompt: "go"}}})
	if err := AppendWorkflow(path, bad); err == nil {
		t.Fatal("AppendWorkflow accepted a workflow naming an agent that does not exist")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Error("a refused AppendWorkflow changed the file")
	}
}
