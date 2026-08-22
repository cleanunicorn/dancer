package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
)

func TestAppendDefinitionKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := `# my dancer config
[server]
default_agent = "coder" # keep

[[definitions]]
name = "coder"
model = "sonnet"
[definitions.environment]
kind = "local"
`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	d := Definition{Name: "reviewer", Kind: "claude", Model: "opus", PermissionMode: "acceptEdits",
		AllowedTools: []string{"Read", "Bash(git:*)"}, SystemPrompt: "Review.\nCarefully."}
	d.Environment.Kind = "docker"
	d.Environment.Image = "ghcr.io/x/claude:latest"
	if err := AppendDefinition(path, d); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.HasPrefix(got, orig) {
		t.Fatalf("original content changed:\n%s", got)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Definitions) != 2 || cfg.Server.DefaultAgent != "coder" {
		t.Fatalf("loaded = %+v", cfg)
	}
	r := cfg.Definitions[1]
	if r.Name != "reviewer" || r.Model != "opus" || r.Environment.Kind != "docker" || r.Environment.Image != "ghcr.io/x/claude:latest" ||
		r.PermissionMode != "acceptEdits" || strings.Join(r.AllowedTools, ",") != "Read,Bash(git:*)" || r.SystemPrompt != "Review.\nCarefully." {
		t.Fatalf("appended definition = %+v", r)
	}

	// Duplicate names and invalid definitions are rejected before writing.
	before, _ := os.ReadFile(path)
	if err := AppendDefinition(path, d); err == nil {
		t.Fatal("duplicate accepted")
	}
	bad := Definition{Name: "nohost"}
	bad.Environment.Kind = "ssh"
	if err := AppendDefinition(path, bad); err == nil {
		t.Fatal("ssh without host accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("file changed by rejected appends:\n%s", after)
	}
}

func TestAppendChannelLastWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := `[server]
default_agent = "coder"

[[channels]]
id = "C1"
agent = "coder"

[[definitions]]
name = "coder"
[definitions.environment]
kind = "local"

[[definitions]]
name = "reviewer"
[definitions.environment]
kind = "local"
`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendChannel(path, Channel{Transport: "slack", ID: "C1", Agent: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendChannel(path, Channel{ID: "C2", Agent: "coder"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), orig) {
		t.Fatalf("original content changed:\n%s", b)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.ChannelAgents()
	if got["slack/C1"] != "reviewer" || got["slack/C2"] != "coder" || len(got) != 2 {
		t.Fatalf("channel agents = %v", got)
	}

	before, _ := os.ReadFile(path)
	if err := AppendChannel(path, Channel{ID: "C3", Agent: "nope"}); err == nil {
		t.Fatal("unknown agent accepted")
	}
	if err := AppendChannel(path, Channel{Agent: "coder"}); err == nil {
		t.Fatal("channel without id accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("file changed by rejected appends:\n%s", after)
	}
}

func TestReplaceAndRemoveDefinition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := `# my dancer config
[server]
default_agent = "coder" # keep

[[channels]]
id = "C1"
agent = "reviewer"

[[channels]]
id = "C1"
agent = "coder" # overrides the block above

# the main agent
[[definitions]]
name = "coder"
model = "sonnet"
[definitions.environment]
kind = "local"

# added from chat on 2026-08-22
[[definitions]]
name = "reviewer"
model = "opus"
allowed_tools = ["Read"]
[definitions.environment]
kind = "docker"
image = "ghcr.io/x/claude:latest"
[definitions.environment.env]
FOO = "bar"

[[definitions]]
name = 'tester'
[definitions.environment]
kind = "local"
`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	// Replace: the block is rewritten in place, its comment and position
	// (and so the implicit default) stay, the rest stays byte for byte.
	d := Definition{Name: "reviewer", Kind: "claude", Model: "haiku", PermissionMode: "manual"}
	d.Environment.Kind = "local"
	if err := ReplaceDefinition(path, d); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	head := orig[:strings.Index(orig, "[[definitions]]\nname = \"reviewer\"")]
	tail := orig[strings.Index(orig, "\n[[definitions]]\nname = 'tester'"):]
	if !strings.HasPrefix(got, head) || !strings.HasSuffix(got, tail) {
		t.Fatalf("content around the replaced block changed:\n%s", got)
	}
	if strings.Contains(got, "ghcr.io") || strings.Contains(got, "FOO") || !strings.Contains(got, "# added from chat") {
		t.Fatalf("old block survived or its comment was lost:\n%s", got)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Definitions) != 3 || cfg.Definitions[1].Name != "reviewer" || cfg.Definitions[1].Model != "haiku" ||
		cfg.Definitions[1].Environment.Kind != "local" || cfg.Definitions[2].Name != "tester" {
		t.Fatalf("loaded = %+v", cfg.Definitions)
	}

	// Replace of a name the file lacks appends it.
	if err := ReplaceDefinition(path, Definition{Name: "extra", Environment: EnvironmentConfig{Kind: "local"}}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(path); len(cfg.Definitions) != 4 || cfg.Definitions[3].Name != "extra" {
		t.Fatalf("loaded = %+v", cfg.Definitions)
	}
	if err := RemoveDefinition(path, "extra"); err != nil {
		t.Fatal(err)
	}
	// Remove: the default agent and an effective channel default are refused;
	// an overridden [[channels]] block does not count; a missing name is
	// ErrNoDefinition.
	before, _ := os.ReadFile(path)
	if err := RemoveDefinition(path, "coder"); err == nil || !strings.Contains(err.Error(), "default agent") {
		t.Fatalf("removing the default agent: %v", err)
	}
	if err := RemoveDefinition(path, "nosuch"); !errors.Is(err, ErrNoDefinition) {
		t.Fatalf("unknown definition: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("file changed by rejected edits:\n%s", after)
	}
	if err := RemoveDefinition(path, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDefinition(path, "tester"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	got = string(b)
	if !strings.HasPrefix(got, orig[:strings.Index(orig, "\n# added from chat")]) || strings.Contains(got, "name = \"reviewer\"") || strings.Contains(got, "tester") || strings.Contains(got, "added from chat") {
		t.Fatalf("after removals:\n%s", got)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Definitions) != 1 || cfg.Definitions[0].Name != "coder" || cfg.Definitions[0].Model != "sonnet" {
		t.Fatalf("loaded = %+v", cfg.Definitions)
	}
}

func TestSpliceDefinition(t *testing.T) {
	src := "[[definitions]]\nname = \"a\"\n[definitions.environment]\nkind = \"local\"\n\n[[definitions]]\nname = \"b\"\n[definitions.environment]\nkind = \"local\"\n[[channels]]\nid = \"C\"\nagent = \"a\"\n"
	out, ok := spliceDefinition([]byte(src), "b", nil)
	if !ok || string(out) != "[[definitions]]\nname = \"a\"\n[definitions.environment]\nkind = \"local\"\n[[channels]]\nid = \"C\"\nagent = \"a\"\n" {
		t.Fatalf("cut b: ok=%v\n%s", ok, out)
	}
	out, ok = spliceDefinition([]byte(src), "a", nil)
	if !ok || !strings.HasPrefix(string(out), "[[definitions]]\nname = \"b\"\n") {
		t.Fatalf("cut a: ok=%v\n%s", ok, out)
	}
	// Replacement keeps the comment above and the blank line after.
	out, ok = spliceDefinition([]byte("# a\n[[definitions]]\nname = \"a\"\n\n[[definitions]]\nname = \"b\"\n"), "a", []byte("[[definitions]]\nname = \"a\"\nmodel = \"x\"\n"))
	if !ok || string(out) != "# a\n[[definitions]]\nname = \"a\"\nmodel = \"x\"\n\n[[definitions]]\nname = \"b\"\n" {
		t.Fatalf("replace a: ok=%v\n%q", ok, out)
	}
	// Comments and blank lines above the next block stay with it.
	out, ok = spliceDefinition([]byte("[[definitions]]\nname = \"a\"\n\n# b is next\n[[definitions]]\nname = \"b\"\n"), "a", nil)
	if !ok || string(out) != "# b is next\n[[definitions]]\nname = \"b\"\n" {
		t.Fatalf("cut a before a commented block: ok=%v\n%q", ok, out)
	}
	// Sub-tables, array sub-tables and lines starting with "[" inside
	// multi-line strings are part of the block.
	src = "[[definitions]]\nname = \"a\"\nsystem_prompt = \"\"\"\n[IMPORTANT]\nbe nice\n\"\"\"\n[[definitions.sub_agents.x]]\nname = \"z\"\n[[definitions]]\nname = \"b\"\n"
	out, ok = spliceDefinition([]byte(src), "a", nil)
	if !ok || string(out) != "[[definitions]]\nname = \"b\"\n" {
		t.Fatalf("cut a with string and sub-array: ok=%v\n%q", ok, out)
	}
	// A sub-table key named "name" does not identify the block.
	if _, ok := spliceDefinition([]byte("[[definitions]]\nname = \"x\"\n[definitions.sub_agents.y]\nname = \"z\"\n"), "z", nil); ok {
		t.Fatal("matched a sub-table name")
	}
	if _, ok := spliceDefinition([]byte("definitions = [{name = \"inline\"}]\n"), "inline", nil); ok {
		t.Fatal("matched an inline table")
	}
}

// TestDockerProvisionDefaults: a docker definition provisions its own image
// unless it says otherwise, and the agent to install comes from the
// definition's kind rather than being spelled out in the file.
func TestDockerProvisionDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	src := `[server]
db = "/tmp/x.db"

[[definitions]]
name = "sandbox"
kind = "claude"
[definitions.environment]
kind = "docker"
image = "ubuntu:24.04"
reuse = "thread"
packages = ["jq"]
setup = ["echo hi"]

[[definitions]]
name = "prebuilt"
kind = "codex"
[definitions.environment]
kind = "docker"
image = "ghcr.io/x/agent:latest"
provision = "none"

[[definitions]]
name = "here"
kind = "claude"
[definitions.environment]
kind = "local"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defs := map[string]agent.Definition{}
	for _, d := range cfg.AgentDefinitions() {
		defs[d.Name] = d
	}

	sandbox := defs["sandbox"].Environment
	if sandbox.Provision == nil {
		t.Fatal("docker without an explicit provision should provision")
	}
	if got := sandbox.Provision.Agents; len(got) != 1 || got[0] != "claude" {
		t.Errorf("agents = %v, want [claude]", got)
	}
	if got := sandbox.Provision.Packages; len(got) != 1 || got[0] != "jq" {
		t.Errorf("packages = %v", got)
	}
	if got := sandbox.Provision.Setup; len(got) != 1 || got[0] != "echo hi" {
		t.Errorf("setup = %v", got)
	}
	if sandbox.Reuse != environment.ReuseThread {
		t.Errorf("reuse = %q, want thread", sandbox.Reuse)
	}

	if p := defs["prebuilt"].Environment.Provision; p != nil {
		t.Errorf(`provision = "none" still provisions: %+v`, p)
	}
	if p := defs["here"].Environment.Provision; p != nil {
		t.Errorf("a local environment has nothing to provision: %+v", p)
	}
	if r := defs["here"].Environment.Reuse; r != "" {
		t.Errorf("reuse = %q on a local environment", r)
	}
}

func TestDockerProvisionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[server]\ndb = \"/tmp/x.db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := agent.Definition{Name: "sandbox", Kind: agent.KindClaude, Environment: environment.Spec{
		Kind:      environment.KindDocker,
		Image:     "ubuntu:24.04",
		Reuse:     environment.ReuseThread,
		Provision: &environment.Provision{Agents: []string{"claude"}, Packages: []string{"jq"}, Setup: []string{"echo hi"}},
	}}
	if err := AppendDefinition(path, DefinitionFromAgent(d)); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.AgentDefinitions()[0].Environment
	if got.Reuse != environment.ReuseThread {
		t.Errorf("reuse = %q", got.Reuse)
	}
	if got.Provision == nil || len(got.Provision.Packages) != 1 || got.Provision.Packages[0] != "jq" {
		t.Fatalf("packages did not survive the round trip: %+v", got.Provision)
	}
	if len(got.Provision.Setup) != 1 || got.Provision.Setup[0] != "echo hi" {
		t.Fatalf("setup did not survive the round trip: %+v", got.Provision)
	}

	// A definition that opted out must still be opted out after a rewrite.
	d.Environment.Provision = nil
	if err := ReplaceDefinition(path, DefinitionFromAgent(d)); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.AgentDefinitions()[0].Environment.Provision; p != nil {
		t.Fatalf(`provision = "none" was lost on rewrite: %+v`, p)
	}
}

func TestDockerConfigValidation(t *testing.T) {
	for name, env := range map[string]string{
		"bad provision": "kind = \"docker\"\nimage = \"x\"\nprovision = \"yes-please\"\n",
		"bad reuse":     "kind = \"docker\"\nimage = \"x\"\nreuse = \"forever\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			src := "[server]\ndb = \"/tmp/x.db\"\n\n[[definitions]]\nname = \"a\"\nkind = \"claude\"\n[definitions.environment]\n" + env
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("bad value was accepted")
			}
		})
	}
}

func TestDeciderConfigValidation(t *testing.T) {
	load := func(t *testing.T, decider string) (*Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.toml")
		src := "[server]\ndb = \"/tmp/x.db\"\n\n[[definitions]]\nname = \"a\"\nkind = \"claude\"\n[definitions.environment]\nkind = \"local\"\n\n[decider]\n" + decider
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	for name, decider := range map[string]string{
		"unknown kind":              "kind = \"gemini\"\n",
		"openai without model":      "kind = \"openai\"\n",
		"openai bad url":            "kind = \"openai\"\nmodel = \"m\"\n[decider.openai]\nbase_url = \"api.openai.com/v1\"\n",
		"openai ftp url":            "kind = \"openai\"\nmodel = \"m\"\n[decider.openai]\nbase_url = \"ftp://x/v1\"\n",
		"kind that cannot be asked": "kind = \"openai\"\nmodel = \"m\"\nuses = [\"route\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, decider); err == nil {
				t.Fatal("bad value was accepted")
			}
		})
	}

	cfg, err := load(t, "kind = \"openai\"\nmodel = \"deepseek-chat\"\n[decider.openai]\nbase_url = \"https://api.deepseek.com/v1/\"\napi_key = \"sk-x\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Decider.Model != "deepseek-chat" || cfg.Decider.OpenAI.BaseURL != "https://api.deepseek.com/v1/" || cfg.Decider.OpenAI.APIKey != "sk-x" {
		t.Fatalf("decider = %+v", cfg.Decider)
	}

	// Defaults: claude gets haiku; openai gets the public endpoint and no
	// key, but never a model it did not ask for.
	cfg, err = load(t, "kind = \"claude\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Decider.Model != "haiku" || cfg.Decider.OpenAI.BaseURL != "https://api.openai.com/v1" || cfg.Decider.OpenAI.APIKey != "" {
		t.Fatalf("defaults = %+v", cfg.Decider)
	}
	if _, err := load(t, "kind = \"openai\"\n[decider.openai]\nbase_url = \"http://localhost:11434/v1\"\n"); err == nil {
		t.Fatal("openai without a model was accepted because of a default")
	}
}
