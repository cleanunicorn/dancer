// Package decider answers dancer's policy questions — "should this task be
// picked up?", "is this tool call worth waking a human for?" — with a small
// model, and with dancer's own rules as the answer it falls back to.
//
// The contract that keeps this safe: a decider narrows, it never widens. The
// caller works out which actions are acceptable and what the rules alone
// would answer, and passes both in the question; a verdict is only ever a
// choice among Options. A decider that is off, slow, broken or talked into
// something by an agent's output leaves dancer exactly as it is without one.
package decider

import (
	"context"
	"fmt"
	"strings"
)

// Question is one decision. Facts is marshalled to JSON for the model and
// for the event log, so it must be a plain struct or map.
type Question struct {
	Kind    string   `json:"kind"`    // "resume", "permission", "route", …
	Task    string   `json:"task"`    // task id, for the log ("" if none)
	Thread  string   `json:"thread"`  // thread id, for the log
	Options []string `json:"options"` // the only acceptable actions
	Facts   any      `json:"facts"`   // what dancer knows; untrusted content
	Static  Verdict  `json:"static"`  // what dancer's rules alone answer
}

// Verdict is the answer. Prompt is only meaningful for kinds that hand text
// to an agent (today: "resume").
type Verdict struct {
	Action string `json:"action"`
	Prompt string `json:"prompt,omitempty"`
	Reason string `json:"reason,omitempty"`
	By     string `json:"by,omitempty"` // "static", "claude"
}

// Caps on what a verdict may carry. A prompt reaches an agent, so it is
// bounded; a reason reaches a chat thread.
const (
	MaxPromptLen = 2000
	MaxReasonLen = 240
)

// Decider answers questions. Decide must return an error rather than a
// half-trusted verdict; the caller then uses Question.Static.
type Decider interface {
	Name() string
	Decide(ctx context.Context, q Question) (Verdict, error)
}

// Static is the decider that always answers with dancer's own rules. It is
// what runs when no decider is configured, and what every other decider
// falls back to.
type Static struct{}

func (Static) Name() string { return "static" }

func (Static) Decide(_ context.Context, q Question) (Verdict, error) {
	v := q.Static
	v.By = "static"
	return v, nil
}

// Validate checks a verdict against its question and trims it to the caps.
// An action outside Options is an error: that is the line a decider must
// not cross, so the caller falls back instead of guessing what was meant.
func Validate(q Question, v Verdict) (Verdict, error) {
	v.Action = strings.TrimSpace(v.Action)
	if !allowed(q.Options, v.Action) {
		return Verdict{}, fmt.Errorf("decider: action %q is not one of %v", v.Action, q.Options)
	}
	v.Prompt = truncate(strings.TrimSpace(v.Prompt), MaxPromptLen)
	v.Reason = truncate(strings.TrimSpace(strings.ReplaceAll(v.Reason, "\n", " ")), MaxReasonLen)
	return v, nil
}

func allowed(options []string, action string) bool {
	for _, o := range options {
		if o == action {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}
