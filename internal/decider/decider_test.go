package decider

import (
	"context"
	"strings"
	"testing"
)

func TestStaticAnswersTheRules(t *testing.T) {
	q := Question{Kind: "resume", Options: []string{"continue", "wait"},
		Static: Verdict{Action: "continue", Prompt: "carry on"}}
	v, err := Static{}.Decide(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if v.Action != "continue" || v.Prompt != "carry on" || v.By != "static" {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestValidateRejectsActionsOutsideTheOptions(t *testing.T) {
	q := Question{Options: []string{"continue", "wait"}}
	for _, action := range []string{"abandon", "", "CONTINUE", "continue everything"} {
		if _, err := Validate(q, Verdict{Action: action}); err == nil {
			t.Fatalf("action %q was accepted", action)
		}
	}
	v, err := Validate(q, Verdict{Action: " wait ", Reason: "line\nbreak"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Action != "wait" || v.Reason != "line break" {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestValidateCapsWhatAVerdictCarries(t *testing.T) {
	q := Question{Options: []string{"continue"}}
	v, err := Validate(q, Verdict{Action: "continue",
		Prompt: strings.Repeat("p", MaxPromptLen+500),
		Reason: strings.Repeat("r", MaxReasonLen+500)})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Prompt) > MaxPromptLen+3 || len(v.Reason) > MaxReasonLen+3 {
		t.Fatalf("prompt %d, reason %d", len(v.Prompt), len(v.Reason))
	}
}

func TestParseVerdictFindsTheObject(t *testing.T) {
	for _, reply := range []string{
		`{"action":"wait","reason":"stale"}`,
		"```json\n{\"action\":\"wait\",\"reason\":\"stale\"}\n```",
		"Here is my answer:\n{\"action\": \"wait\", \"reason\": \"stale\"}\nHope that helps.",
	} {
		v, err := parseVerdict(reply)
		if err != nil {
			t.Fatalf("%q: %v", reply, err)
		}
		if v.Action != "wait" || v.Reason != "stale" {
			t.Fatalf("%q → %+v", reply, v)
		}
	}
	if _, err := parseVerdict("I would rather not answer."); err == nil {
		t.Fatal("prose was accepted as a verdict")
	}
}
