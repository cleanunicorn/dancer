package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	// Replace: the old block (with its comment) goes, the rest stays byte for byte.
	d := Definition{Name: "reviewer", Kind: "claude", Model: "haiku", PermissionMode: "manual"}
	d.Environment.Kind = "local"
	if err := ReplaceDefinition(path, d); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	head := orig[:strings.Index(orig, "\n# added from chat")]
	if !strings.HasPrefix(got, head) {
		t.Fatalf("content before the replaced block changed:\n%s", got)
	}
	if strings.Contains(got, "ghcr.io") || strings.Contains(got, "FOO") || strings.Contains(got, "# added from chat") {
		t.Fatalf("old block survived:\n%s", got)
	}
	if !strings.Contains(got, "name = 'tester'") || !strings.Contains(got, "# updated from chat on") {
		t.Fatalf("rewritten file:\n%s", got)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Definitions) != 3 || cfg.Definitions[2].Name != "reviewer" || cfg.Definitions[2].Model != "haiku" ||
		cfg.Definitions[2].Environment.Kind != "local" || cfg.Definitions[1].Name != "tester" {
		t.Fatalf("loaded = %+v", cfg.Definitions)
	}

	// Replace of an unknown name is refused before writing.
	before, _ := os.ReadFile(path)
	if err := ReplaceDefinition(path, Definition{Name: "nosuch", Environment: EnvironmentConfig{Kind: "local"}}); err == nil {
		t.Fatal("unknown definition replaced")
	}
	// Remove: the global default and an effective channel default are refused;
	// an overridden [[channels]] block does not count.
	if err := RemoveDefinition(path, "coder"); err == nil || !strings.Contains(err.Error(), "default_agent") {
		t.Fatalf("removing the default agent: %v", err)
	}
	if err := RemoveDefinition(path, "nosuch"); err == nil {
		t.Fatal("unknown definition removed")
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
	if !strings.HasPrefix(got, head) || strings.Contains(got, "name = \"reviewer\"") || strings.Contains(got, "tester") || strings.Contains(got, "updated from chat") {
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

func TestCutDefinition(t *testing.T) {
	src := "[[definitions]]\nname = \"a\"\n[definitions.environment]\nkind = \"local\"\n\n[[definitions]]\nname = \"b\"\n[definitions.environment]\nkind = \"local\"\n[[channels]]\nid = \"C\"\nagent = \"a\"\n"
	out, ok := cutDefinition([]byte(src), "b")
	if !ok || string(out) != "[[definitions]]\nname = \"a\"\n[definitions.environment]\nkind = \"local\"\n[[channels]]\nid = \"C\"\nagent = \"a\"\n" {
		t.Fatalf("cut b: ok=%v\n%s", ok, out)
	}
	out, ok = cutDefinition([]byte(src), "a")
	if !ok || !strings.HasPrefix(string(out), "[[definitions]]\nname = \"b\"\n") {
		t.Fatalf("cut a: ok=%v\n%s", ok, out)
	}
	// Comments and blank lines above the next block stay with it.
	out, ok = cutDefinition([]byte("[[definitions]]\nname = \"a\"\n\n# b is next\n[[definitions]]\nname = \"b\"\n"), "a")
	if !ok || string(out) != "# b is next\n[[definitions]]\nname = \"b\"\n" {
		t.Fatalf("cut a before a commented block: ok=%v\n%q", ok, out)
	}
	// A sub-table key named "name" does not identify the block.
	if _, ok := cutDefinition([]byte("[[definitions]]\nname = \"x\"\n[definitions.sub_agents.y]\nname = \"z\"\n"), "z"); ok {
		t.Fatal("matched a sub-table name")
	}
	if _, ok := cutDefinition([]byte("definitions = [{name = \"inline\"}]\n"), "inline"); ok {
		t.Fatal("matched an inline table")
	}
}
