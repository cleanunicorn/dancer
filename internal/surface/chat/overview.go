package chat

import (
	"strconv"
	"strings"

	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/work"
)

// Overview is dispatch's word on what a thread is working on, in at most two
// lines: the pull request to go and look at with the issue behind it, then
// where the work lives and what else went by.
//
// It is appended to the lines a human reads when they are deciding whether
// to open a browser — the closing line of a turn and the answer to
// `status` — and is empty when the thread has touched no repository, so a
// thread that is not about code reads exactly as it did before.
func Overview(w *work.State) string {
	if w == nil || w.Empty() {
		return ""
	}
	var lines []string
	head := headline(w)
	if head != "" {
		lines = append(lines, head)
	}
	// The repository is already spelled out in the headline's URL; it is
	// only worth naming when there is no headline to carry it.
	if where := whereLine(w, head == ""); where != "" {
		lines = append(lines, where)
	}
	return strings.Join(lines, "\n")
}

// headline is the pull request, and the issue behind it when there is
// one — or the issue alone when no pull request exists yet. Both are
// clickable and both read as "#51": the number is what a human says out
// loud, and the URL rides underneath it (transport.Link) rather than
// across the line, which is what kept the issue a bare number here
// before.
func headline(w *work.State) string {
	switch {
	case w.PR != nil:
		s := "🔀 " + link(w.PR, w.Repo)
		if w.Issue != nil {
			s += " · for " + link(w.Issue, w.Repo)
		}
		return s
	case w.Issue != nil:
		return "🎯 " + link(w.Issue, w.Repo)
	}
	return ""
}

// whereLine says where the work lives — the branch, and the repository
// when withRepo — and what else the thread named without working on it.
// It leads with 🌿 when it has somewhere to point at and 💬 when all it
// has is talk, so the line is never a bare list.
func whereLine(w *work.State, withRepo bool) string {
	var parts []string
	if w.Branch != "" {
		// A branch the thread only created locally is not on GitHub and
		// stays a code span; one the log saw pushed becomes a link. The
		// backticks come off when it does — Slack does not format the
		// label of a link, so they would show as themselves.
		parts = append(parts, code(w.BranchURL(), w.Branch))
	}
	if withRepo && w.Repo != "" {
		parts = append(parts, code(w.RepoURL(), w.Repo))
	}
	leader := "🌿 "
	if len(parts) == 0 {
		leader = "💬 "
	}
	if len(w.Also) > 0 {
		also := make([]string, 0, len(w.Also))
		for i := range w.Also {
			also = append(also, link(&w.Also[i], w.Repo))
		}
		parts = append(parts, "also "+strings.Join(also, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return leader + strings.Join(parts, " · ")
}

// link is a reference as something to click: its short form, carrying the
// URL when one is known. Every reference gets one — the overview names
// half a dozen numbers at most, and a number nobody can open is a number
// somebody has to go and search for.
func link(r *work.Ref, repo string) string {
	return transport.Link(r.URL, ref(r, repo))
}

// code renders a name that would otherwise be a code span, as a link when
// there is somewhere to point it. It is either/or on purpose: a code span
// inside a link label is rendered by neither Slack nor the web UI.
func code(url, name string) string {
	if url == "" {
		return "`" + name + "`"
	}
	return transport.Link(url, name)
}

// ref is a reference at its shortest: "#51", or "owner/repo#51" when it
// belongs to some repository other than the one being worked in.
func ref(r *work.Ref, repo string) string {
	n := "#" + strconv.Itoa(r.Number)
	if r.Repo != "" && r.Repo != repo {
		return r.Repo + n
	}
	return n
}

// WithOverview appends the overview to a line, when there is one. It is
// exported because the feed surface renders the same event into the ops
// channel and must not fall behind what chat shows (as it does for the
// cost and usage lines).
func WithOverview(line string, w *work.State) string {
	if o := Overview(w); o != "" {
		return line + "\n" + o
	}
	return line
}
