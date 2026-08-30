package chat

import (
	"testing"

	"github.com/cleanunicorn/dispatch/internal/work"
)

// TestOverview: what a thread is working on, rendered for a human who is
// deciding whether to open a browser.
func TestOverview(t *testing.T) {
	pr := &work.Ref{Repo: "o/r", Kind: work.KindPR, Number: 51, URL: "https://github.com/o/r/pull/51"}
	issue := &work.Ref{Repo: "o/r", Kind: work.KindIssue, Number: 47, URL: "https://github.com/o/r/issues/47"}

	for _, tc := range []struct {
		name string
		w    *work.State
		want string
	}{
		{"nothing", nil, ""},
		{"empty", &work.State{}, ""},
		{
			// Both references are clickable and both read as their
			// number; the branch was only ever created here, so it has
			// nowhere to point and stays a code span.
			"pull request with its issue",
			&work.State{Repo: "o/r", Branch: "fix", PR: pr, Issue: issue},
			"🔀 <https://github.com/o/r/pull/51|#51> · for <https://github.com/o/r/issues/47|#47>\n🌿 `fix`",
		},
		{
			"issue only, so the repository needs saying",
			&work.State{Repo: "o/r", Issue: issue},
			"🎯 <https://github.com/o/r/issues/47|#47>",
		},
		{
			"branch only",
			&work.State{Repo: "o/r", Branch: "spike"},
			"🌿 `spike` · <https://github.com/o/r|o/r>",
		},
		{
			// A repository with no branch is still somewhere to point at,
			// so it keeps the leaf; 💬 is for a line that is only talk.
			"repository only",
			&work.State{Repo: "o/r"},
			"🌿 <https://github.com/o/r|o/r>",
		},
		{
			"nothing but talk",
			&work.State{Also: []work.Ref{{Kind: work.KindIssue, Number: 4}}},
			"💬 also #4",
		},
		{
			// A branch the log saw pushed is somewhere to go, so it
			// becomes the link and loses the backticks a link label
			// would show as themselves.
			"a pushed branch is clickable",
			&work.State{Repo: "o/r", Branch: "spike", Pushed: true},
			"🌿 <https://github.com/o/r/tree/spike|spike> · <https://github.com/o/r|o/r>",
		},
		{
			"a reference from somewhere else keeps its repository",
			&work.State{Repo: "o/r", PR: pr, Also: []work.Ref{
				{Repo: "o/r", Kind: work.KindIssue, Number: 12, URL: "https://github.com/o/r/issues/12"},
				{Repo: "other/lib", Kind: work.KindIssue, Number: 9, URL: "https://github.com/other/lib/issues/9"},
			}},
			"🔀 <https://github.com/o/r/pull/51|#51>\n💬 also <https://github.com/o/r/issues/12|#12>, <https://github.com/other/lib/issues/9|other/lib#9>",
		},
	} {
		if got := Overview(tc.w); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// TestWithOverview: a thread that never touched a repository reads exactly
// as it did before the overview existed.
func TestWithOverview(t *testing.T) {
	if got := WithOverview("✅ done · 4s", nil); got != "✅ done · 4s" {
		t.Errorf("got %q", got)
	}
	want := "✅ done · 4s\n🌿 `spike` · <https://github.com/o/r|o/r>"
	if got := WithOverview("✅ done · 4s", &work.State{Repo: "o/r", Branch: "spike"}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
