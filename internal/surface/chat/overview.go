package chat

import (
	"strconv"
	"strings"

	"github.com/cleanunicorn/dancer/internal/work"
)

// overview is dancer's word on what a thread is working on, in at most two
// lines: the pull request to go and look at with the issue behind it, then
// where the work lives and what else went by.
//
// It is appended to the lines a human reads when they are deciding whether
// to open a browser — the closing line of a turn and the answer to
// `status` — and is empty when the thread has touched no repository, so a
// thread that is not about code reads exactly as it did before.
func overview(w *work.State) string {
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

// headline is the pull request, or the issue when there is no pull request
// yet — the thing to click. The other one rides along as a bare number:
// two URLs on one line is a wall, and the issue is one hop from the PR.
func headline(w *work.State) string {
	switch {
	case w.PR != nil:
		s := "🔀 " + link(w.PR, w.Repo)
		if w.Issue != nil {
			s += " · for " + ref(w.Issue, w.Repo)
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
		parts = append(parts, "`"+w.Branch+"`")
	}
	if withRepo && w.Repo != "" {
		parts = append(parts, "`"+w.Repo+"`")
	}
	leader := "🌿 "
	if len(parts) == 0 {
		leader = "💬 "
	}
	if len(w.Also) > 0 {
		also := make([]string, 0, len(w.Also))
		for i := range w.Also {
			also = append(also, ref(&w.Also[i], w.Repo))
		}
		parts = append(parts, "also "+strings.Join(also, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return leader + strings.Join(parts, " · ")
}

// link is a reference as something to click: the number, then the URL when
// one is known.
func link(r *work.Ref, repo string) string {
	if r.URL == "" {
		return ref(r, repo)
	}
	return ref(r, repo) + " " + r.URL
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

// withOverview appends the overview to a line, when there is one.
func withOverview(line string, w *work.State) string {
	if o := overview(w); o != "" {
		return line + "\n" + o
	}
	return line
}
