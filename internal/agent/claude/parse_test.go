package claude

import (
	"bufio"
	"os"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
)

func TestTranslateFixture(t *testing.T) {
	f, err := os.Open("testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var types []agent.EventType
	var perm *permissionReq
	var session string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		p, err := translate(sc.Bytes(), time.Now())
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		for _, ev := range p.Events {
			types = append(types, ev.Type)
			if ev.Type == agent.EventInit {
				session = ev.Session
				if ev.Text == "" {
					t.Error("init: model empty")
				}
			}
			if ev.Type == agent.EventToolUse && ev.Tool != "Bash" {
				t.Errorf("tool_use tool = %q", ev.Tool)
			}
			if ev.Type == agent.EventResult && ev.Cost <= 0 {
				t.Errorf("result cost = %v", ev.Cost)
			}
		}
		if p.Permission != nil {
			perm = p.Permission
		}
	}
	want := []agent.EventType{agent.EventInit, agent.EventToolUse, agent.EventToolResult, agent.EventText, agent.EventResult}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
	if session == "" {
		t.Fatal("no session id")
	}
	if perm == nil {
		t.Fatal("no permission request parsed")
	}
	if perm.RequestID == "" || perm.Event.Tool != "Bash" || perm.Event.ToolInput["command"] != "touch probe-created.txt" {
		t.Fatalf("permission = %+v", perm)
	}
	if perm.Event.Type != agent.EventNeedsPermission {
		t.Fatalf("permission event type = %v", perm.Event.Type)
	}
}

func TestArgs(t *testing.T) {
	got, err := args(agent.Definition{Model: "haiku", AllowedTools: []string{"Read", "Edit"}, PermissionMode: agent.PermissionAcceptEdits}, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, a := range got {
		joined += a + " "
	}
	for _, want := range []string{"--permission-prompt-tool stdio", "--permission-mode acceptEdits", "--model haiku", "--allowedTools Read,Edit", "--resume sess-1"} {
		if !contains(joined, want) {
			t.Errorf("args missing %q in %q", want, joined)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
