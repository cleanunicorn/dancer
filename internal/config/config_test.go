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
