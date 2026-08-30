package work

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/transport"
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
	l.bash("u1", "git remote -v", "origin\tgit@github.com:cleanunicorn/dispatch.git (fetch)")
	l.bash("u2", "git switch -c status-overview", "Switched to a new branch 'status-overview'")
	l.says("also worth a look: #12 and #38")
	l.bash("u3", "git push -u origin status-overview", "remote: Create a pull request for 'status-overview' on GitHub by visiting:\nremote:      https://github.com/cleanunicorn/dispatch/pull/new/status-overview")
	l.bash("u4", `gh pr create --title "status overview" --body "Closes #47"`,
		"https://github.com/cleanunicorn/dispatch/pull/51")

	st := Scan(l.recs)
	if st.Repo != "cleanunicorn/dispatch" {
		t.Errorf("Repo = %q", st.Repo)
	}
	if st.Branch != "status-overview" {
		t.Errorf("Branch = %q", st.Branch)
	}
	if st.PR == nil || st.PR.Number != 51 || st.PR.Seen != SeenCreated {
		t.Fatalf("PR = %+v", st.PR)
	}
	if st.PR.URL != "https://github.com/cleanunicorn/dispatch/pull/51" {
		t.Errorf("PR.URL = %q", st.PR.URL)
	}
	if st.Issue == nil || st.Issue.Number != 47 || st.Issue.Seen != SeenWorked {
		t.Fatalf("Issue = %+v", st.Issue)
	}
	// A bare "#12" never said which repository it was in; it inherits the
	// one in hand, and gets a URL from it.
	if st.Issue.URL != "https://github.com/cleanunicorn/dispatch/issues/47" {
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

// TestScanBareNumbersInTheAgentsProseAreNotReferences: when the agent
// means a pull request it links it. A "#12" in its report is a quotation
// — of a file it read, of an example it is proposing, of the overview
// dispatch wrote under the last turn — and a thread about this very
// overview mined its own rendered numbers back out of the agent's summary
// of them, turn after turn.
func TestScanBareNumbersInTheAgentsProseAreNotReferences(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "git remote -v", "origin\tgit@github.com:o/r.git (fetch)")
	l.add("agent", agent.Event{Type: agent.EventResult,
		Text: "It would render as `🔀 #54 · for #48` with `also #12, #13`."})
	if st := Scan(l.recs); st.PR != nil || st.Issue != nil || len(st.Also) != 0 {
		t.Errorf("the agent quoting numbers made them the work: %+v", st)
	}

	// A link in the same report is still believed, and so is what the
	// agent says it closed.
	l = &log{at: time.Unix(0, 0)}
	l.add("agent", agent.Event{Type: agent.EventResult,
		Text: "Opened https://github.com/o/r/pull/51, which closes #47."})
	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 51 {
		t.Fatalf("PR = %+v, want the one the report linked", st.PR)
	}
	if st.Issue == nil || st.Issue.Number != 47 {
		t.Fatalf("Issue = %+v, want the one the report says it closes", st.Issue)
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

// TestScanIgnoresOwnOutput: dispatch's own overview lines carry the very
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
	l.bash("u1", "git remote -v", "origin\thttps://github.com/cleanunicorn/dispatch.git (fetch)")
	l.bash("u2", "gh pr view 49", "title:\tGitHub CLI\nurl:\thttps://github.com/cleanunicorn/dispatch/pull/49")

	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 49 {
		t.Fatalf("PR = %+v", st.PR)
	}
	if st.PR.URL != "https://github.com/cleanunicorn/dispatch/pull/49" {
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
	l.bash("u1", "git remote -v", "origin\tgit@github.com:cleanunicorn/dispatch.git (fetch)")
	l.says("run coder same bug as https://github.com/other/lib/issues/9 — see #12 here")

	st := Scan(l.recs)
	if st.Repo != "cleanunicorn/dispatch" {
		t.Fatalf("Repo = %q", st.Repo)
	}
	if st.Issue == nil || st.Issue.Number != 12 || st.Issue.Repo != "cleanunicorn/dispatch" {
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

// TestScanMinesAPermissionRequest: a command waiting for a human's yes is
// still a command, and says what the thread is about before anyone has
// answered.
func TestScanMinesAPermissionRequest(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.add("agent", agent.Event{Type: agent.EventNeedsPermission, Tool: "Bash", ToolID: "p1",
		ToolInput: map[string]any{"command": "gh pr checkout 51"}})
	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 51 || st.PR.Seen != SeenWorked {
		t.Fatalf("PR = %+v", st.PR)
	}
}

// TestScanAlsoIsCapped: a thread that named a dozen numbers still shows an
// overview, not a list.
func TestScanAlsoIsCapped(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "git remote -v", "origin\thttps://github.com/o/r.git (fetch)")
	l.says("run coder see #1, #2, #3, #4, #5, #6 and #7")
	st := Scan(l.recs)
	if st.Issue == nil {
		t.Fatal("no issue was picked out of seven")
	}
	if len(st.Also) != maxAlso {
		t.Errorf("Also holds %d, want the cap of %d: %+v", len(st.Also), maxAlso, st.Also)
	}
}

// TestScanStopsReading: a tool can print a megabyte, and the references
// worth having are near the top of it. What is past the clip is not read.
func TestScanStopsReading(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	buried := strings.Repeat("noise ", maxText/6+1) + "https://github.com/o/r/pull/9"
	if len(buried) <= maxText {
		t.Fatalf("the reference is not past the clip: %d bytes", len(buried))
	}
	l.bash("u1", "gh pr list --limit 500", buried)
	if st := Scan(l.recs); !st.Empty() {
		t.Errorf("read past the clip: %+v", st)
	}
}

// TestScanIgnoresRepositoriesGoingPast: a thread reads other people's
// repositories all day — go.mod names three, a README links a fourth —
// and none of them is the repository it is working in. Believing the last
// one seen pointed every bare "#47" at a stranger's issue.
func TestScanIgnoresRepositoriesGoingPast(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.says("run coder please fix #47")
	l.bash("u1", "git remote -v", "origin\tgit@github.com:cleanunicorn/dispatch.git (fetch)")
	l.bash("u2", "cat go.mod", "module github.com/cleanunicorn/dispatch\n\nrequire (\n\tgithub.com/BurntSushi/toml v1.4.0\n\tgithub.com/slack-go/slack v0.15.0\n)")
	l.bash("u3", "cat README.md", "See https://github.com/anthropics/claude-code/issues/999 for the CLI.")

	st := Scan(l.recs)
	if st.Repo != "cleanunicorn/dispatch" {
		t.Fatalf("Repo = %q, want the one the remote named", st.Repo)
	}
	if st.Issue == nil || st.Issue.Number != 47 {
		t.Fatalf("Issue = %+v", st.Issue)
	}
	if st.Issue.URL != "https://github.com/cleanunicorn/dispatch/issues/47" {
		t.Errorf("Issue.URL = %q — a bare #47 was pointed at another repository", st.Issue.URL)
	}
}

// TestScanRemoteFromABareURLIsNotBelieved: the shape matters too. A
// repository spelled without a scheme is prose, not a remote, so it does
// not become the repository in hand even when a remote command printed it.
func TestScanRemoteFromABareURLIsNotBelieved(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "git remote -v", "origin\tgithub.com/only/prose (fetch)")
	if st := Scan(l.recs); st.Repo != "" {
		t.Errorf("Repo = %q, want none", st.Repo)
	}
}

// TestScanBranchIsNeverAFlag: git forbids a branch whose name starts with
// "-", so a command's own flag must never be read as one. `git branch
// --show-current` is how an agent asks which branch it is on, and it used
// to answer "--show-current" — and overwrite the real branch with it.
func TestScanBranchIsNeverAFlag(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{"git switch -c status-overview", "status-overview"},
		{"git checkout -b fix-47", "fix-47"},
		{"git branch spike", "spike"},
		{"git push -u origin status-overview", "status-overview"},
		{"git push origin HEAD:release", "release"},
		{"git branch --show-current", ""},
		{"git branch -a", ""},
		{"git branch --list", ""},
		{"git branch -vv", ""},
		{"git branch -d old-thing", ""},
		{"git push origin --delete stale", ""},
	} {
		if got := branchOf(tc.cmd); got != tc.want {
			t.Errorf("branchOf(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

// TestScanBranchSurvivesALaterListing: the branch is last-write-wins, so a
// command that names no branch must leave the one already found alone.
func TestScanBranchSurvivesALaterListing(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "git switch -c feature-x", "Switched to a new branch 'feature-x'")
	l.bash("u2", "git branch --show-current", "feature-x")
	l.bash("u3", "git branch -a", "* feature-x\n  main")
	if st := Scan(l.recs); st.Branch != "feature-x" {
		t.Errorf("Branch = %q, want feature-x", st.Branch)
	}
}

// TestScanListingIsNotWork: one command that returns every open pull
// request acted on none of them. Graded as work, the listing's highest
// number won and the thread claimed a pull request it never touched.
func TestScanListingIsNotWork(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u0", "git remote -v", "origin\tgit@github.com:o/r.git (fetch)")
	l.bash("u1", "gh pr status", "https://github.com/o/r/pull/70\nhttps://github.com/o/r/pull/71\nhttps://github.com/o/r/pull/72\nhttps://github.com/o/r/pull/73")
	l.bash("u2", "gh pr view 12", "url:\thttps://github.com/o/r/pull/12")

	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 12 {
		t.Fatalf("PR = %+v, want the one that was viewed", st.PR)
	}
	if st.PR.Seen != SeenWorked {
		t.Errorf("PR.Seen = %v, want SeenWorked", st.PR.Seen)
	}
	for _, r := range st.Also {
		if r.Seen != SeenMentioned {
			t.Errorf("a listed pull request graded %v: %+v", r.Seen, r)
		}
	}
}

// TestScanOrderIsStable: sightings live in a map, and the answer must not
// depend on the order Go walks it. Two references that tie on every other
// term settle on the repository.
func TestScanOrderIsStable(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.says("run coder compare https://github.com/a/lib/issues/5 with https://github.com/z/lib/issues/5")
	first := Scan(l.recs)
	for i := 0; i < 50; i++ {
		got := Scan(l.recs)
		if got.Issue == nil || first.Issue == nil || *got.Issue != *first.Issue {
			t.Fatalf("run %d picked %+v, first run picked %+v", i, got.Issue, first.Issue)
		}
	}
}

// TestNarrowEventMatchesAgentEvent pins the contract Scan relies on: the
// log holds a marshalled agent.Event, and the narrow struct decodes the
// four fields a scan reads out of it under exactly the same names. It also
// pins how a tool call spells its input, which mayMatter sniffs for.
func TestNarrowEventMatchesAgentEvent(t *testing.T) {
	full := agent.Event{
		Type:      agent.EventToolUse,
		Text:      "https://github.com/o/r/pull/51",
		ToolInput: map[string]any{"command": "git switch -c spike"},
		ToolID:    "u9",
		Raw:       []byte(`{"vendor":"payload"}`),
	}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var got event
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != full.Type || got.Text != full.Text || got.ToolID != full.ToolID ||
		got.ToolInput["command"] != full.ToolInput["command"] {
		t.Errorf("narrow decode = %+v, want %+v — agent.Event's field names moved", got, full)
	}
	if !mayMatter(b) {
		t.Error("a tool call carrying a command was skipped; toolInput no longer matches the log")
	}
	plain, err := json.Marshal(agent.Event{Type: agent.EventToolResult, Text: "no references here"})
	if err != nil {
		t.Fatal(err)
	}
	if mayMatter(plain) {
		t.Error("a tool result with nothing to find was decoded anyway")
	}
}

// BenchmarkScan is the latency budget: an overview is read at the end of
// every turn, over the tail of a thread, while a human waits for the
// closing line. The records are the expensive shape — a thread full of
// large tool results, each carrying the vendor message in Raw. "quiet" is
// output with nothing to find, which most of a thread is; "hashes" is
// output a byte-level skip cannot rule out, which is the worst case.
func BenchmarkScan(b *testing.B) {
	for _, tc := range []struct{ name, line string }{
		{"quiet", "package main // an ordinary source line, no references\n"},
		{"hashes", "# an ordinary comment line, in a language that has them\n"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			blob := strings.Repeat(tc.line, 500)
			recs := make([]store.Record, 0, 1500)
			for i := 0; i < 1500; i++ {
				ev := agent.Event{Type: agent.EventToolResult, ToolID: "u", Text: blob, Raw: []byte(blob)}
				p, err := json.Marshal(ev)
				if err != nil {
					b.Fatal(err)
				}
				recs = append(recs, store.Record{At: time.Unix(int64(i), 0), Kind: "agent", Payload: p})
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				Scan(recs)
			}
		})
	}
}

// TestScanReadsTheNewestItCanAfford: the byte budget is spent from the end
// of the thread, so a run of enormous tool results costs the oldest
// records, never the ones the work just happened in.
func TestScanReadsTheNewestItCanAfford(t *testing.T) {
	blob := strings.Repeat("# a line with a hash, so no byte-level skip applies\n", 20_000) // ~1MB
	l := &log{at: time.Unix(0, 0)}
	l.bash("u0", "git remote -v", "origin\tgit@github.com:o/r.git (fetch)")
	l.bash("u1", "gh pr view 1", "url:\thttps://github.com/o/r/pull/1") // oldest: must fall off
	for i := 0; i < 2+(maxScan/len(blob)); i++ {
		l.bash("f", "cat big.py", blob)
	}
	l.bash("u2", "gh pr view 2", "url:\thttps://github.com/o/r/pull/2") // newest: must survive

	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 2 {
		t.Fatalf("PR = %+v, want the newest one the budget reaches", st.PR)
	}
	for _, r := range st.Also {
		if r.Number == 1 {
			t.Errorf("a record past the budget was read anyway: %+v", r)
		}
	}
}

// TestScanSurvivesOneEnormousRecord: a record too big to afford is stepped
// over, not a wall. Everything older than it used to be discarded — so one
// `cat` of a large file late in a thread took the pull request with it and
// the overview came back empty.
func TestScanSurvivesOneEnormousRecord(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", `gh pr create --title x`, "https://github.com/o/r/pull/42")
	// One record on its own dearer than the entire budget.
	l.bash("u2", "cat big.py", strings.Repeat("# a line with a hash\n", maxScan/20))

	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 42 {
		t.Fatalf("PR = %+v, want the one named behind the enormous record", st.PR)
	}
	if st.PR.Seen != SeenCreated {
		t.Errorf("PR.Seen = %v, want the creating command still paired with its result", st.PR.Seen)
	}
}

// TestScanBelievesPushAdviceOnlyFromAPush: git's "create a pull request"
// advice names a branch, and that branch is rendered inside a code span in
// a line dispatch signs its own name to. The URL shape can appear in a file
// the agent read or a page it fetched, so it is read out of a command that
// spoke to a remote — and only as far as a branch name may go.
func TestScanBelievesPushAdviceOnlyFromAPush(t *testing.T) {
	const advice = "remote: Create a pull request for 'x' on GitHub by visiting:\n" +
		"remote:      https://github.com/o/r/pull/new/x`<https://evil.example|urgent: re-auth here>`"

	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "cat README.md", advice)
	if b := Scan(l.recs).Branch; b != "" {
		t.Errorf("Branch = %q, read out of a command that pushed nothing", b)
	}

	l = &log{at: time.Unix(0, 0)}
	l.bash("u1", "git push", advice)
	b := Scan(l.recs).Branch
	if b != "x" {
		t.Errorf("Branch = %q, want the name git advised and nothing after it", b)
	}
	if strings.ContainsAny(b, "`<>|*_&") {
		t.Errorf("Branch = %q carries markup dispatch would render as its own", b)
	}
}

// TestScanReadsBothEndsOfProse: an agent's closing report says what it did
// first and links the pull request last, so a long one had its link cut
// off. A tool's output is still read from the top only — a listing that
// runs long says nothing more by its end, and reading it would undo what
// the clip is for.
func TestScanReadsBothEndsOfProse(t *testing.T) {
	long := strings.Repeat("Did a great deal of work. ", maxText/13) // > 2*maxText

	l := &log{at: time.Unix(0, 0)}
	l.add("agent", agent.Event{Type: agent.EventResult,
		Text: long + "\nOpened https://github.com/o/r/pull/77 for review."})
	if st := Scan(l.recs); st.PR == nil || st.PR.Number != 77 {
		t.Errorf("PR = %+v, want the one the closing report ended on", st.PR)
	}

	l = &log{at: time.Unix(0, 0)}
	l.add("agent", agent.Event{Type: agent.EventResult,
		Text: "Opened https://github.com/o/r/pull/78 for review.\n" + long})
	if st := Scan(l.recs); st.PR == nil || st.PR.Number != 78 {
		t.Errorf("PR = %+v, want the one the closing report opened on", st.PR)
	}

	// A listing is still read from the top, whatever its end holds.
	l = &log{at: time.Unix(0, 0)}
	l.bash("u1", "gh pr list --limit 500", long+"\nhttps://github.com/o/r/pull/79")
	if st := Scan(l.recs); st.PR != nil {
		t.Errorf("PR = %+v, read past the clip on a listing", st.PR)
	}
}

// TestScanRemoteFromEveryCommandThatNamesOne: the repository may be named
// by any command that talks to a remote, not just `git remote`. Narrowing
// that list to `remote|clone` left the suite green.
func TestScanRemoteFromEveryCommandThatNamesOne(t *testing.T) {
	for _, tc := range []struct{ name, cmd, out string }{
		{"remote", "git remote -v", "origin\tgit@github.com:o/r.git (fetch)"},
		{"clone", "git clone https://github.com/o/r", "Cloning into 'r'..."},
		{"ls-remote", "git ls-remote --get-url", "https://github.com/o/r.git"},
		{"config", "git config --get remote.origin.url", "https://github.com/o/r.git"},
		{"fetch", "git fetch origin", "From https://github.com/o/r\n * branch  main -> FETCH_HEAD"},
		{"gh repo", "gh repo view --json url", `{"url":"https://github.com/o/r"}`},
	} {
		l := &log{at: time.Unix(0, 0)}
		l.bash("u1", tc.cmd, tc.out)
		if got := Scan(l.recs).Repo; got != "o/r" {
			t.Errorf("%s: Repo = %q, want o/r", tc.name, got)
		}
	}
	// And still not from a command that only happened to print one.
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "cat go.mod", "require github.com/slack-go/slack v0.12.0")
	if got := Scan(l.recs).Repo; got != "" {
		t.Errorf("Repo = %q, read out of a file that only mentioned one", got)
	}
}

// TestScanTellsAnIssueFromAPullRequest: `gh issue view 47` names an issue
// and `gh pr view 47` a pull request, and the bare number says neither.
// Forcing both to KindPR left the suite green.
func TestScanTellsAnIssueFromAPullRequest(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "gh issue develop 47", "created branch")
	st := Scan(l.recs)
	if st.Issue == nil || st.Issue.Number != 47 || st.Issue.Seen != SeenWorked {
		t.Fatalf("Issue = %+v", st.Issue)
	}
	if st.PR != nil {
		t.Errorf("PR = %+v, want an issue command to name no pull request", st.PR)
	}
}

// TestScanOrphanedResultIsNotRead: a tool result is read only when the
// command behind it is known and asked GitHub, looked up by tool id. When
// the budget stepped over that command's own record the lookup finds
// nothing, and a result nothing is known about is not evidence: it is the
// same output every file the agent opened arrives as. The pull request
// this thread really created is lost with it, which is the price of not
// claiming the ones it did not.
func TestScanOrphanedResultIsNotRead(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	// The creating command's own record is dearer than the whole budget,
	// so it is stepped over; its result, small, is not.
	l.add("agent", agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "u1",
		ToolInput: map[string]any{"command": `gh pr create --body "` + strings.Repeat("z", maxScan) + `"`}})
	l.add("agent", agent.Event{Type: agent.EventToolResult, ToolID: "u1",
		Text: "https://github.com/o/r/pull/13"})

	if st := Scan(l.recs); !st.Empty() {
		t.Errorf("a result with no command behind it was read: %+v", st)
	}
}

// TestScanIgnoresWhatItOnlyRead is the whole point of the gate: a thread
// working on this very package reads files that cite pull requests, greps
// a repository whose comments are full of them, and prints a log of
// commits that each name one. It is working on none of them. Every one of
// these turned up in a real overview.
func TestScanIgnoresWhatItOnlyRead(t *testing.T) {
	const source = `// ownerRepo matches a bare "owner/repo#12" and a "Closes o/r#47".
	// See #53 for why, and #52 for the rename.`

	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "sed -n 440,500p internal/work/work.go", source)
	l.bash("u2", `grep -rn "Issue" internal/surface/ --include=*.go`, source)
	l.bash("u3", "git log --oneline -5", "a62b58f agents: make kind a real choice (#48)\ne1741d7 Rename the project (#52)")
	// A file read through the Read tool: a tool call with no command at
	// all, whose result used to be mined like any other.
	l.add("agent", agent.Event{Type: agent.EventToolUse, Tool: "Read", ToolID: "u4",
		ToolInput: map[string]any{"file_path": "internal/work/work.go"}})
	l.add("agent", agent.Event{Type: agent.EventToolResult, ToolID: "u4", Text: source})

	if st := Scan(l.recs); !st.Empty() {
		t.Errorf("what the thread only read became what it works on: %+v", st)
	}
}

// TestScanIgnoresHeredocBodies: an agent writes a file by handing the
// shell a here-document, and the body of one is data. Writing this
// package's own test fixtures renamed the thread's branch to "x" and
// pointed its numbers at o/r.
func TestScanIgnoresHeredocBodies(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "git remote -v", "origin\tgit@github.com:cleanunicorn/dispatch.git (fetch)")
	l.bash("u2", `cat > internal/work/work_test.go <<'EOF'
	l.bash("u1", "git switch -c x", "Switched to a new branch 'x'")
	l.bash("u2", "git clone https://github.com/o/r", "Cloning into 'r'...")
	l.says("see #12 and https://github.com/o/r/issues/9")
EOF`, "")
	l.bash("u3", "go build ./...", "")

	st := Scan(l.recs)
	if st.Repo != "cleanunicorn/dispatch" {
		t.Errorf("Repo = %q, read out of a file being written", st.Repo)
	}
	if st.Branch != "" {
		t.Errorf("Branch = %q, read out of a file being written", st.Branch)
	}
	if st.PR != nil || st.Issue != nil || len(st.Also) != 0 {
		t.Errorf("references mined from a file being written: %+v", st)
	}
}

// TestScanReadsAPullRequestBody: the one here-document whose body is not
// just data. What a command hands GitHub is the pull request, and "Closes
// #47" in it is the strongest thing the thread ever said about an issue.
func TestScanReadsAPullRequestBody(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", `gh pr create --title "fix the thing" --body-file - <<'EOF'
Closes #47. Also mentions #12 in passing.
EOF`, "https://github.com/o/r/pull/51")

	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 51 || st.PR.Seen != SeenCreated {
		t.Fatalf("PR = %+v", st.PR)
	}
	if st.Issue == nil || st.Issue.Number != 47 || st.Issue.Seen != SeenWorked {
		t.Fatalf("Issue = %+v, want the one the body closes", st.Issue)
	}
}

// TestScanReadsBothEndsOfABody: a pull request body is prose, and a long
// one says what it closes at the bottom — under the "Progress" list, the
// validation, everything the agent had to say. Clipped from the top only,
// the issue behind the work was cut off exactly when there was most of it.
func TestScanReadsBothEndsOfABody(t *testing.T) {
	long := strings.Repeat("Did a great deal of work. ", maxText/13) // > 2*maxText

	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "gh pr create --title x --body-file - <<'EOF'\n"+long+"\nCloses #47\nEOF", "https://github.com/o/r/pull/51")

	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 51 {
		t.Fatalf("PR = %+v", st.PR)
	}
	if st.Issue == nil || st.Issue.Number != 47 {
		t.Fatalf("Issue = %+v, want the one a long body closes at its end", st.Issue)
	}
}

// TestScanCommandsAreNotQuotes: a command that quotes another is not that
// command. Searching a repository that talks about `gh` and `git clone`
// is how an agent works on one, and its output is a source file.
func TestScanCommandsAreNotQuotes(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", `grep -rn "gh pr view" internal/work/`, "work.go:42: // `gh pr view 51` names a reference\nwork.go:43: // https://github.com/o/r/pull/51")
	l.bash("u2", `grep -rn "git clone" internal/work/`, "work_test.go:9: git clone https://github.com/o/r")

	if st := Scan(l.recs); !st.Empty() {
		t.Errorf("a grep was read as the command it searched for: %+v", st)
	}

	// And a real one, run after another command, still is one.
	l = &log{at: time.Unix(0, 0)}
	l.bash("u1", "cd repo && gh pr view 51", "url:\thttps://github.com/o/r/pull/51")
	if st := Scan(l.recs); st.PR == nil || st.PR.Number != 51 {
		t.Errorf("PR = %+v, want the one viewed past an &&", st.PR)
	}
}

// TestStripHeredocs: what is left of a command once the files it writes
// are taken out of it.
func TestStripHeredocs(t *testing.T) {
	for _, tc := range []struct{ name, cmd, want string }{
		{"none", "git switch -c spike", "git switch -c spike"},
		{"body dropped", "cat > f <<'EOF'\ngit switch -c x\nEOF\ngit push", "cat > f \ngit push"},
		{"tab form", "cat <<-PY\n#12\n\tPY\necho done", "cat \necho done"},
		{"unterminated", "cat > f <<EOF\ngit switch -c x\n", "cat > f \n"},
		{"no body yet", "cat > f <<EOF", "cat > f "},
		{"two of them", "a <<ONE\nx\nONE\nb <<TWO\ny\nTWO\nc", "a \nb \nc"},
		{"a shift is not a here-document", "python3 -c 'print(1 << n)'", "python3 -c 'print(1 << n)'"},
		// A bare delimiter must be capitals, so what is not an operator
		// is left where it is rather than swallowing the line — and the
		// real command after it survives.
		{"a stream is not a here-document", `echo "a << bb" && gh pr create`, `echo "a << bb" && gh pr create`},
		{"nor is one in a grep pattern", "grep -n \"cout << endl\" src/\ngit switch -c real", "grep -n \"cout << endl\" src/\ngit switch -c real"},
		{"quoted keeps its case", "cat > f <<'eof'\n#12\neof\ngit push", "cat > f \ngit push"},
	} {
		if got := stripHeredocs(tc.cmd); got != tc.want {
			t.Errorf("%s: stripHeredocs(%q) = %q, want %q", tc.name, tc.cmd, got, tc.want)
		}
	}
}

// TestScanHeredocDoesNotReopenTheResultGate: a tool result is gated and
// graded by the command behind it, looked up by tool id — so what is
// remembered under that id must be the command *stripped*, the same
// string `command` reads. Remembering the raw one let a file written
// through a here-document say `git clone` and hand its own contents back
// as though GitHub had answered them.
func TestScanHeredocDoesNotReopenTheResultGate(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "git remote -v", "origin\tgit@github.com:cleanunicorn/dispatch.git (fetch)")
	l.bash("u2", "cat > setup.sh <<'EOF'\ngit clone https://github.com/o/r\nEOF\ncat setup.sh",
		"git clone https://github.com/o/r\nsee #12 and https://github.com/o/r/pull/99")

	st := Scan(l.recs)
	if st.Repo != "cleanunicorn/dispatch" {
		t.Errorf("Repo = %q, read out of a file being written back to us", st.Repo)
	}
	if st.PR != nil || st.Issue != nil || len(st.Also) != 0 {
		t.Errorf("references mined from the output of `cat`: %+v", st)
	}
}

// TestScanQuotedCommandsNameNothing: every command this package
// recognises is anchored where a command begins, not merely word-bounded.
// A grep for one is how an agent works on this very package, and its
// output is a source file.
func TestScanQuotedCommandsNameNothing(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", `grep -rn "gh pr view 51" internal/`, "work.go:42: // `gh pr view 51` names a reference")
	l.bash("u2", `grep -rn "git switch -c fixture" internal/`, "")
	l.bash("u3", `rg "gh pr create" internal/`, "")

	st := Scan(l.recs)
	if st.Branch != "" {
		t.Errorf("Branch = %q, read out of a search pattern", st.Branch)
	}
	if !st.Empty() {
		t.Errorf("a grep was read as the command it searched for: %+v", st)
	}
}

// TestScanReadsThroughAWrapper: `timeout 60 gh pr create` created a pull
// request, and so did the one an indented line or a `FOO=1` prefix
// carries. Anchoring at the start of a command must not mean anchoring at
// the start of the string.
func TestScanReadsThroughAWrapper(t *testing.T) {
	for _, cmd := range []string{
		`timeout 60 gh pr create --title x --body "Closes #47"`,
		`  gh pr create --title x --body "Closes #47"`,
		`GH_TOKEN=$T gh pr create --title x --body "Closes #47"`,
		`env GH_HOST=github.com gh pr create --title x --body "Closes #47"`,
	} {
		l := &log{at: time.Unix(0, 0)}
		l.bash("u1", cmd, "https://github.com/o/r/pull/51")
		st := Scan(l.recs)
		if st.PR == nil || st.PR.Number != 51 || st.PR.Seen != SeenCreated {
			t.Errorf("%s: PR = %+v, want the one it created", cmd, st.PR)
		}
		if st.Issue == nil || st.Issue.Number != 47 {
			t.Errorf("%s: Issue = %+v, want the one the body closes", cmd, st.Issue)
		}
	}
}

// TestScanHeredocNextToAGhCallIsStillAFile: what a command hands GitHub is
// read as prose, but only what it hands it. A here-document that merely
// shares a command line with a `gh` call is a file like any other.
func TestScanHeredocNextToAGhCallIsStillAFile(t *testing.T) {
	l := &log{at: time.Unix(0, 0)}
	l.bash("u1", "cat > notes.md <<'EOF'\nCloses #999, see https://github.com/o/r/pull/998\nEOF\ngh pr view 51",
		"url:\thttps://github.com/o/r/pull/51")

	st := Scan(l.recs)
	if st.PR == nil || st.PR.Number != 51 {
		t.Fatalf("PR = %+v, want the one viewed", st.PR)
	}
	if st.Issue != nil {
		t.Errorf("Issue = %+v, mined from a file being written", st.Issue)
	}
	if len(st.Also) != 0 {
		t.Errorf("Also = %+v, mined from a file being written", st.Also)
	}
}
