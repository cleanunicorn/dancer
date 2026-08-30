package chat

import (
	"testing"

	"github.com/cleanunicorn/dancer/internal/work"
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
			"pull request with its issue",
			&work.State{Repo: "o/r", Branch: "fix", PR: pr, Issue: issue},
			"🔀 #51 https://github.com/o/r/pull/51 · for #47\n🌿 `fix`",
		},
		{
			"issue only, so the repository needs saying",
			&work.State{Repo: "o/r", Issue: issue},
			"🎯 #47 https://github.com/o/r/issues/47",
		},
		{
			"branch only",
			&work.State{Repo: "o/r", Branch: "spike"},
			"🌿 `spike` · `o/r`",
		},
		{
			"a reference from somewhere else keeps its repository",
			&work.State{Repo: "o/r", PR: pr, Also: []work.Ref{
				{Repo: "o/r", Kind: work.KindIssue, Number: 12},
				{Repo: "other/lib", Kind: work.KindIssue, Number: 9},
			}},
			"🔀 #51 https://github.com/o/r/pull/51\n💬 also #12, other/lib#9",
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
	want := "✅ done · 4s\n🌿 `spike` · `o/r`"
	if got := WithOverview("✅ done · 4s", &work.State{Repo: "o/r", Branch: "spike"}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
