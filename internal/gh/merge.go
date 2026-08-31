package gh

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Method is how a pull request is merged. It is the word a human types
// after `ship`, and the flag `gh pr merge` is given.
type Method string

const (
	MethodSquash Method = "squash"
	MethodMerge  Method = "merge"
	MethodRebase Method = "rebase"
)

// DefaultMethod is what `ship` merges with when nobody said otherwise.
// Squash is the one that leaves the branch's noise behind.
const DefaultMethod = MethodSquash

// ParseMethod reads the word a human typed. The empty string is the
// default; "it" is what people write after `ship` and means nothing.
func ParseMethod(s string) (Method, bool) {
	switch Method(strings.ToLower(strings.TrimSpace(s))) {
	case "", "it":
		return DefaultMethod, true
	case MethodSquash:
		return MethodSquash, true
	case MethodMerge:
		return MethodMerge, true
	case MethodRebase:
		return MethodRebase, true
	}
	return "", false
}

// mergeTimeout bounds one merge. GitHub can sit on a merge while it waits
// for a check; past this the human is better told than left watching.
const mergeTimeout = 2 * time.Minute

// Merge merges a pull request on GitHub with the host's own gh login, and
// deletes the branch behind it.
//
// dispatch runs this itself rather than telling the agent to, for one
// reason: it has to know whether the merge actually happened. An agent
// asked to merge answers in prose, and "the required checks are still
// running" reads much like "merged" to anything downstream — while
// `gh pr merge` has an exit code. The thread a `ship` closes is closed on
// that exit code and on nothing else.
//
// It runs on the host rather than in the thread's environment: a per-task
// container is usually gone by the time anyone says `ship`, and a merge
// needs no checkout — only the pull request's URL and a login, which is
// exactly what the host has. It is the same login Lend borrows the other
// way, so an agent that opened the pull request and the dispatch that
// merges it are the same GitHub account.
//
// The returned string is gh's own output, success or failure, which is
// what gets posted to the thread: it says "Merged pull request #51" or it
// says which check is still red, and either is more use than anything
// dispatch could write about it.
func Merge(ctx context.Context, url string, method Method) (string, error) {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return "", fmt.Errorf("gh: no GitHub CLI on this host to merge with")
	}
	if strings.TrimSpace(url) == "" {
		return "", fmt.Errorf("gh: no pull request to merge")
	}
	if method == "" {
		method = DefaultMethod
	}
	ctx, cancel := context.WithTimeout(ctx, mergeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "pr", "merge", url, "--"+string(method), "--delete-branch")
	// gh asks questions when it is unsure and there is a terminal; there
	// is no one here to answer, so it must fail instead of hanging.
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	text := strings.TrimSpace(out.String())
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("gh pr merge: %w", err)
	}
	return text, nil
}
