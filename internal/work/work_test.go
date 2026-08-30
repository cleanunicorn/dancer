package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// log builds a thread's records the way the coordinator writes them.
type log struct {
	recs []store.Record
	at   time.Time
}

func (l *log) add(kind string, v any) {
	l.at = l.at.Add(time.Second)
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	l.recs = append(l.recs, store.Record{At: l.at, Thread: "C/1", Kind: kind, Payload: b})
}

func (l *log) says(text string) { l.add("inbound", transport.Inbound{Thread: "C/1", Text: text}) }

func (l *log) bash(id, cmd, out string) {
	l.add("agent", agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: id,
		ToolInput: map[string]any{"command": cmd}})
	l.add("agent", agent.Event{Type: agent.EventToolResult, ToolID: id, Text: out})
}

// TestScanTypicalThread walks the shape of a real piece of work: a human
// asks for an issue, the agent branches, pushes and opens a pull request.
// The pull request it opened here must outrank every number that merely
// went past, and the issue the body closes must be the one it names.
func TestScanTypicalThread(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.says("run coder please fix #47, it's the status line one")
	l.bash("u1", "git remote -v", "origin\tgit@github.com:cleanunicorn/dancer.git (fetch)")
	l.bash("u2", "git switch -c status-overview", "Switched to a new branch 'status-overview'")
	l.add("agent", agent.Event{Type: agent.EventText, Text: "Looking at #12 and #38 for prior art."})
	l.bash("u3", "git push -u origin status-overview", "remote: Create a pull request for 'status-overview' on GitHub by visiting:\nremote:      https://github.com/cleanunicorn/dancer/pull/new/status-overview")
	l.bash("u4", `gh pr create --title "status overview" --body "Closes #47"`,
		"https://github.com/cleanunicorn/dancer/pull/51")

	st := Scan(l.recs)
	if st.Repo != "cleanunicorn/dancer" {
		t.Errorf("Repo = %q", st.Repo)
	}
	if st.Branch != "status-overview" {
		t.Errorf("Branch = %q", st.Branch)
	}
	if st.PR == nil || st.PR.Number != 51 || st.PR.Seen != SeenCreated {
		t.Fatalf("PR = %+v", st.PR)
	}
	if st.PR.URL != "https://github.com/cleanunicorn/dancer/pull/51" {
		t.Errorf("PR.URL = %q", st.PR.URL)
	}
	if st.Issue == nil || st.Issue.Number != 47 || st.Issue.Seen != SeenWorked {
		t.Fatalf("Issue = %+v", st.Issue)
	}
	// A bare "#12" never said which repository it was in; it inherits the
	// one in hand, and gets a URL from it.
	if st.Issue.URL != "https://github.com/cleanunicorn/dancer/issues/47" {
		t.Errorf("Issue.URL = %q", st.Issue.URL)
	}
	if len(st.Also) != 2 {
		t.Fatalf("Also = %+v", st.Also)
	}
	for _, r := range st.Also {
		if r.Number != 12 && r.Number != 38 {
			t.Errorf("Also carries %+v", r)
		}
		if r.Seen != SeenMentioned {
			t.Errorf("passing mention graded %v: %+v", r.Seen, r)
		}
	}
}

// TestScanWorkedBeatsMentioned: acting on a pull request outranks talking
// about one, whatever order they were seen in.
func TestScanWorkedBeatsMentioned(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.says("run coder review https://github.com/o/r/pull/9 then take over #3")
	l.bash("u1", "gh pr checkout 3", "Switched to branch 'fix-3'")
	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 3 || st.PR.Seen != SeenWorked {
		t.Fatalf("PR = %+v", st.PR)
	}
	if st.PR.Repo != "o/r" {
		t.Errorf("PR.Repo = %q, want the repo in hand", st.PR.Repo)
	}
	// #3 was recorded as an issue from the human's message and as a pull
	// request from the checkout; one number is one thing.
	if st.Issue != nil && st.Issue.Number == 3 {
		t.Errorf("the same number is both a PR and an issue: %+v", st.Issue)
	}
}

// TestScanIgnoresOwnOutput: dancer's own overview lines carry the very
// references they were mined from. Reading them back would keep
// confirming them, so outbound records are not scanned at all.
func TestScanIgnoresOwnOutput(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.add("outbound", transport.Outbound{Thread: "C/1", Text: "🔀 https://github.com/o/r/pull/7"})
	if st := Scan(l.recs); !st.Empty() {
		t.Errorf("outbound was scanned: %+v", st)
	}
}

// TestScanNoise: a thread that never touches GitHub says so, and text
// that only looks like a reference is not one.
func TestScanNoise(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.says("run coder make the header #1a2b3c and bump the h1")
	l.bash("u1", "sed -i 's/#fff/#000/' style.css", "")
	l.add("agent", agent.Event{Type: agent.EventResult, Text: "Done."})
	if st := Scan(l.recs); !st.Empty() {
		t.Errorf("noise read as work: %+v", st)
	}
}

// TestScanSurvivesJunk: an unparseable payload is skipped, not fatal.
func TestScanSurvivesJunk(t *testing.T) {
	recs := []store.Record{
		{Kind: "agent", Payload: []byte("{not json")},
		{Kind: "inbound", Payload: []byte("]")},
		{Kind: "verdict", Payload: []byte(`{"action":"allow"}`)},
	}
	if st := Scan(recs); !st.Empty() {
		t.Errorf("junk produced %+v", st)
	}
}

// TestScanOneNumberOneReference: the same pull request seen three ways —
// a command that names only the number, the URL in its output, and a
// human's bare "#49" — is one reference, not three.
func TestScanOneNumberOneReference(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.says("run coder have a look at #49")
	l.bash("u1", "git remote -v", "origin\thttps://github.com/cleanunicorn/dancer.git (fetch)")
	l.bash("u2", "gh pr view 49", "title:\tGitHub CLI\nurl:\thttps://github.com/cleanunicorn/dancer/pull/49")

	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 49 {
		t.Fatalf("PR = %+v", st.PR)
	}
	if st.PR.URL != "https://github.com/cleanunicorn/dancer/pull/49" {
		t.Errorf("PR.URL = %q", st.PR.URL)
	}
	if st.Issue != nil {
		t.Errorf("the same number came back as an issue too: %+v", st.Issue)
	}
	if len(st.Also) != 0 {
		t.Errorf("Also = %+v, want the one reference not repeated", st.Also)
	}
}

// TestScanBranchNames: each of the three ways the log names a branch, on
// its own — a thread that only pushed, one that only said --head, and one
// where git's own "create a pull request" advice is all there is. Together
// in one thread they mask each other, and a broken one would not show.
func TestScanBranchNames(t *testing.T) {
	for _, tc := range []struct{ name, cmd, out, want string }{
		{"created", "git switch -c only-switched", "Switched to a new branch 'only-switched'", "only-switched"},
		{"pushed", "git push -u origin only-pushed", "", "only-pushed"},
		{"named to gh", `gh pr create --head only-headed --title x`, "", "only-headed"},
		{"git's own advice", "git push", "remote: Create a pull request for 'only-advised' on GitHub by visiting:\nremote:      https://github.com/o/r/pull/new/only-advised", "only-advised"},
		{"nothing to name", "go test ./...", "ok", ""},
	} {
		l := &log{at: time.Unix(0, 0)}
		l.bash("u1", tc.cmd, tc.out)
		if got := Scan(l.recs).Branch; got != tc.want {
			t.Errorf("%s: Branch = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestScanTwoRepositories: a visitor from another repository keeps its own
// name and does not become what the thread is about, while a bare number
// takes the repository in hand.
func TestScanTwoRepositories(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "git remote -v", "origin\tgit@github.com:cleanunicorn/dancer.git (fetch)")
	l.says("run coder same bug as https://github.com/other/lib/issues/9 — see #12 here")

	st := Scan(l.recs)
	if st.Repo != "cleanunicorn/dancer" {
		t.Fatalf("Repo = %q", st.Repo)
	}
	if st.Issue == nil || st.Issue.Number != 12 || st.Issue.Repo != "cleanunicorn/dancer" {
		t.Fatalf("Issue = %+v, want #12 in the repository in hand", st.Issue)
	}
	if len(st.Also) != 1 || st.Also[0].Repo != "other/lib" || st.Also[0].Number != 9 {
		t.Fatalf("Also = %+v, want the visitor with its own repository", st.Also)
	}
	if st.Also[0].URL != "https://github.com/other/lib/issues/9" {
		t.Errorf("the visitor's URL was rewritten: %q", st.Also[0].URL)
	}
}

// TestScanSameNumberInTwoRepositories: one conversation that worked in two
// clones can say "#3" in each and mean different things. Neither sighting
// named a repository, so only the one in hand at the time tells them apart.
func TestScanSameNumberInTwoRepositories(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "git clone https://github.com/org/a && cd a", "Cloning into 'a'...")
	l.bash("u2", "gh pr view 3", "title:\tthe one in a")
	l.bash("u3", "git clone https://github.com/org/b && cd b", "Cloning into 'b'...")
	l.bash("u4", "gh pr view 3", "title:\tthe one in b")

	st := Scan(l.recs)
	if st.PR == nil || st.PR.Repo != "org/b" {
		t.Fatalf("PR = %+v, want the one most recently worked on", st.PR)
	}
	if len(st.Also) != 1 || st.Also[0].Repo != "org/a" || st.Also[0].Number != 3 {
		t.Fatalf("Also = %+v, want org/a#3 kept apart from org/b#3", st.Also)
	}
}
