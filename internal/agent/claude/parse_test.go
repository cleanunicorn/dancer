package claude

import (
	"bufio"
	"os"
	"strings"
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
				if ev.Model != "claude-haiku-4-5-20251001" {
					t.Errorf("init: model = %q", ev.Model)
				}
				if ev.Mode != agent.PermissionManual {
					t.Errorf("init: mode = %q, want manual (permissionMode default)", ev.Mode)
				}
				if ev.Version != "2.1.239" {
					t.Errorf("init: version = %q", ev.Version)
				}
				if ev.Workdir != "/work" {
					t.Errorf("init: workdir = %q", ev.Workdir)
				}
				if ev.Billing != agent.BillingSubscription {
					t.Errorf("init: billing = %q, want subscription (apiKeySource none)", ev.Billing)
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

func TestTranslateQuestion(t *testing.T) {
	raw := []byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"t9","input":{"questions":[{"question":"Apple or Banana?","header":"Fruit","multiSelect":false,"options":[{"label":"Apple","description":"round"},{"label":"Banana","description":"long"}]}]}}}`)
	p, err := translate(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Permission == nil || p.Permission.Event.Type != agent.EventQuestion {
		t.Fatalf("parsed = %+v", p)
	}
	qs := p.Permission.Event.Questions
	if len(qs) != 1 || qs[0].Header != "Fruit" || qs[0].Text != "Apple or Banana?" || len(qs[0].Options) != 2 || qs[0].Options[1].Label != "Banana" {
		t.Fatalf("questions = %+v", qs)
	}
}

// A tool call refused by the CLI itself (here the auto-mode classifier) is a
// system line whose "message" is a string, not a Messages API object, and the
// refusal then reaches the agent as an is_error tool_result on the next line.
// The system line becomes EventToolDenied (policy said no, not the tool) and
// the pair shares the tool id.
func TestTranslatePermissionDenied(t *testing.T) {
	denied := []byte(`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_012ppbtEBUBgHeZtpDBraDSa","decision_reason_type":"classifier","decision_reason":"Blocked by classifier","message":"Permission for this action was denied by the Claude Code auto mode classifier. Reason: Blocked by classifier.","uuid":"3dc0c389-07cc-42dd-978a-69ff7522818d","session_id":"81c8abc4-f8ad-46d1-a026-9a7f966a105a"}`)
	result := []byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_012ppbtEBUBgHeZtpDBraDSa","type":"tool_result","content":"Permission for this action was denied by the Claude Code auto mode classifier. Reason: Blocked by classifier.","is_error":true}]},"parent_tool_use_id":null,"session_id":"81c8abc4-f8ad-46d1-a026-9a7f966a105a","uuid":"5d1f0b2e-2c8a-4e0b-9a1c-7f3d2b6e8c41"}`)

	p, err := translate(denied, time.Now())
	if err != nil {
		t.Fatalf("translate denied: %v", err)
	}
	if len(p.Events) != 1 || p.Permission != nil || p.Control != nil {
		t.Fatalf("denied parsed = %+v, want one event", p)
	}
	ev := p.Events[0]
	if ev.Type != agent.EventToolDenied || ev.Tool != "Bash" || ev.ToolID != "toolu_012ppbtEBUBgHeZtpDBraDSa" {
		t.Errorf("denied event = %+v", ev)
	}
	if !strings.Contains(ev.Text, "Blocked by classifier") {
		t.Errorf("denied text = %q, want the CLI's reason", ev.Text)
	}

	p, err = translate(result, time.Now())
	if err != nil {
		t.Fatalf("translate result: %v", err)
	}
	if len(p.Events) != 1 {
		t.Fatalf("result parsed = %+v, want one event", p)
	}
	if got := p.Events[0]; got.Type != agent.EventToolResult || got.Tool != "error" || got.ToolID != ev.ToolID {
		t.Errorf("result event = %+v, want is_error tool_result for the same tool id", got)
	}
}

// The leniency is for system lines only. On an assistant/user line the
// message must be a well-formed Messages API object: a malformed object or a
// bare string is still a bad line, so a protocol change does not go dark.
func TestTranslateMalformedMessage(t *testing.T) {
	for _, raw := range []string{
		`{"type":"assistant","message":{"role":"assistant","content":"not-a-list"}}`,
		`{"type":"assistant","message":"Done."}`,
		`{"type":"user","message":[]}`,
	} {
		if _, err := translate([]byte(raw), time.Now()); err == nil {
			t.Errorf("translate(%s): want error", raw)
		}
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
