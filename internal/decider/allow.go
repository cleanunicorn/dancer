package decider

import (
	"fmt"
	"strings"
)

// Allow is one parsed auto-allow pattern, in the syntax definitions use for
// allowed_tools:
//
//	Read              any Read call                (Any)
//	Bash(*)           any Bash call                (Any)
//	Bash(go test:*)   a Bash call starting "go test …"
//	Read(/repo/*)     a Read call under /repo
//
// The parser lives here, next to the policy, so config validation and the
// coordinator's matcher read a pattern the same way: a pattern the
// coordinator would match is one config accepted, and nothing else.
type Allow struct {
	Tool string
	Arg  string // the prefix; "" when Any
	Any  bool   // every call of the tool
}

// ParseAllow parses one pattern. A pattern that does not close its
// parenthesis, or closes it on nothing ("Bash()", "Bash(:*)"), is an
// error rather than a match-all: a truncated edit to config must not widen
// the ceiling to every call of a tool.
func ParseAllow(pattern string) (Allow, error) {
	pattern = strings.TrimSpace(pattern)
	name, rest, hasArgs := strings.Cut(pattern, "(")
	name = strings.TrimSpace(name)
	if name == "" {
		return Allow{}, fmt.Errorf("auto_allow %q: no tool name", pattern)
	}
	if !hasArgs {
		return Allow{Tool: name, Any: true}, nil
	}
	inner, closed := strings.CutSuffix(strings.TrimSpace(rest), ")")
	if !closed {
		return Allow{}, fmt.Errorf("auto_allow %q: missing \")\"", pattern)
	}
	inner = strings.TrimSpace(inner)
	if inner == "*" {
		return Allow{Tool: name, Any: true}, nil
	}
	arg := strings.TrimSuffix(inner, ":*")
	arg = strings.TrimSpace(strings.TrimRight(arg, "*"))
	if arg == "" {
		return Allow{}, fmt.Errorf("auto_allow %q: empty prefix — write %s or %s(*) for every call", pattern, name, name)
	}
	return Allow{Tool: name, Arg: arg}, nil
}
