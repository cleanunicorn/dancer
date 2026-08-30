// Package work answers "what is this thread actually working on?" — the
// repository, the branch, the pull request and the issue — by reading the
// event log back.
//
// Nothing here asks GitHub anything. A thread that opens a pull request
// has already said so in the log: the agent ran `gh pr create`, and the
// URL came back in the tool result; the human wrote "fix #47" in the
// message that started the task; the branch was born in a `git switch -c`.
// Scan mines those records for references and returns the projection, so
// the overview survives a restart, costs no network call, and still works
// after a per-task container is long gone. What it cannot know — the diff
// stat, the checks, whether someone merged it since — is left to a live
// probe of the environment, which is not part of this package.
//
// Every reference carries how strongly the thread is about it (Seen): a
// pull request the agent *created* here outranks one it merely looked at,
// which outranks a number that appeared in passing. That ordering is the
// whole trick — a long thread mentions many numbers and is about one.
//
// What it refuses to believe matters as much as what it reads. A
// repository is the one a remote command named, not every
// "github.com/owner/name" a go.mod or a README puts in front of it; a
// branch is never a command's own flag; and a command that came back with
// a page of pull requests acted on none of them.
//
// Above all, a thread is not working on what it merely read. Only the
// output of a command that asked GitHub or a remote is evidence at all:
// `gh pr view`, `git push`, the URL that came back from `gh pr create`.
// The bytes of a file the agent opened are not, however many numbers they
// cite — nor is `git log`, nor a grep over a repository that talks about
// pull requests, nor a result whose command a scan never saw. Commands
// are recognised where a command begins — past any indentation, any
// separator and any wrapper, so `timeout 60 gh pr create` created a pull
// request while `grep -rn "gh pr create" .` is a grep. And a
// here-document's body is a file being written rather than commands being
// run: a fixture full of `git switch -c x` named no branch, though what a
// command hands GitHub under a body flag — a pull request's body — is
// still read for what it links and what it says it closes.
//
// The agent's own prose is read the same careful way: for a link, and for
// "Closes #47", but never for a bare "#12". When the agent means a pull
// request it links it; a number in a report is a quotation — of a file it
// read, of an example it is proposing, of the overview dispatch itself
// wrote under the last turn. That last one is the loop outbound records
// are skipped to avoid, arriving by another door.
//
// When no command named a remote at all — a thread working in a checkout
// that was already there, which is most of them — the repository falls
// back to the one most often linked to by a pull request or issue URL in
// the thread. That is weaker evidence and is weighted as such
// (repoFromProse against repoFromRemote), but it is still a repository
// somebody linked into this conversation rather than one a file happened
// to name in passing: a bare "github.com/owner/name" never counts, because
// only a URL that points at a pull request or an issue is tallied.
//
// Scan is a projection over records the caller already has, not new state:
// it stores nothing and appends nothing. It runs at the end of every turn,
// while a human waits for the closing line, so it is written to skip: most
// records are ruled out on their bytes and never decoded, and what is left
// is bounded (maxScan, maxText).
package work

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// Kind tells a pull request from an issue.
type Kind string

const (
	KindPR    Kind = "pr"
	KindIssue Kind = "issue"
)

// Seen is how strongly a thread is about a reference. Higher wins when
// two references of the same kind compete to be *the* one.
type Seen int

const (
	// SeenMentioned: the number appeared in something someone wrote.
	SeenMentioned Seen = iota
	// SeenWorked: a command acted on it (`gh pr checkout 51`), or a pull
	// request body linked it ("Closes #47").
	SeenWorked
	// SeenCreated: it was opened from this thread, and the log caught the
	// URL coming back.
	SeenCreated
)

// Ref is one pull request or issue seen in a thread.
type Ref struct {
	Repo   string // "owner/repo"; empty when only "#12" was ever seen
	Kind   Kind
	Number int
	URL    string    // canonical URL, when one was seen or can be built
	Seen   Seen      // the strongest way it was seen
	At     time.Time // when it was last seen
}

// State is what a thread is working on.
type State struct {
	Repo   string // "owner/repo", when the thread ever named one
	Branch string // the branch the agent made or pushed
	PR     *Ref   // the pull request this thread is about
	Issue  *Ref   // the issue this thread is about
	Also   []Ref  // other references, strongest and most recent first
}

// Empty reports whether the scan found nothing worth showing.
func (s State) Empty() bool {
	return s.Repo == "" && s.Branch == "" && s.PR == nil && s.Issue == nil && len(s.Also) == 0
}

// maxText bounds how much of one record is searched. A tool result can be
// a whole file or the output of `gh pr list --limit 500`; the references
// worth finding are near the top of it, and the regexes should not be
// handed a megabyte. An overview is read at the end of every turn, over
// the whole tail of the thread, while a human waits for the closing line —
// so this is a latency budget, not just a safety valve.
const maxText = 4 << 10

// maxWorked is how many references one piece of text may name before it is
// read as a listing rather than as work. Past a handful, a command that
// returned them all did not act on any of them.
const maxWorked = 3

// maxScan bounds the bytes one scan decodes. The caller's record limit
// bounds the count, which is not the same thing: a thread of tiny records
// costs nothing to read whole, while a thread whose every tool result is a
// file — carried twice, in Text and in Raw — can be a hundred megabytes of
// JSON, and a human is waiting for the closing line behind it.
const maxScan = 16 << 20

// maxAlso is how many extra references the overview carries. Past a
// handful it stops being an overview.
const maxAlso = 4

// Scan projects the state of the work out of a thread's records, oldest
// first — what Store.ThreadRecords returns. Records it cannot parse are
// skipped; outbound records are ignored on purpose, because dispatch's own
// overview lines carry the very references they were mined from and would
// otherwise keep re-confirming themselves.
func Scan(recs []store.Record) State {
	sc := scanner{refs: map[string]*Ref{}, tools: map[string]string{}, repos: map[string]int{}}
	for _, r := range affordable(recs) { // already filtered and budgeted
		switch r.Kind {
		case "inbound":
			var in transport.Inbound
			if json.Unmarshal(r.Payload, &in) != nil {
				continue
			}
			sc.text(clipEnds(in.Text), r.At, SeenMentioned)
		case "agent":
			// Decoded into the four fields that can carry a reference
			// rather than the whole agent.Event: a logged event also
			// carries Raw, the entire vendor message, and base64-decoding
			// one of those per record is most of the cost of a scan.
			// TestNarrowEventMatchesAgentEvent pins the field names.
			var ev event
			if json.Unmarshal(r.Payload, &ev) != nil {
				continue
			}
			sc.event(ev, r.At)
		}
	}
	return sc.state()
}

// event is the part of an agent.Event a scan reads. Its fields are named
// exactly as agent.Event names them, because that is what the log holds.
type event struct {
	Type      agent.EventType
	Text      string
	ToolInput map[string]any
	ToolID    string
}

// scanner accumulates what the records say.
type scanner struct {
	refs   map[string]*Ref   // key → strongest sighting
	tools  map[string]string // tool id → the command it ran, for its result
	repos  map[string]int    // "owner/repo" → times named
	branch string
	repo   string // last repository a remote named
}

// event mines one agent event. A tool call is read as an intent (what the
// agent set out to do), its result as an outcome (the URL that came back),
// and the two are paired by tool id so a URL in the output of `gh pr
// create` counts as created rather than mentioned.
func (sc *scanner) event(ev event, at time.Time) {
	switch ev.Type {
	case agent.EventToolUse, agent.EventNeedsPermission:
		cmd, _ := ev.ToolInput["command"].(string)
		if cmd == "" {
			return
		}
		if ev.ToolID != "" {
			// Stripped, because this is what the result is graded and
			// gated by: a file written through a here-document must not
			// be able to say `gh pr view` and let its own contents back
			// in as an answer from GitHub.
			sc.tools[ev.ToolID] = stripHeredocs(cmd)
		}
		sc.command(cmd, at)
	case agent.EventToolResult:
		cmd := sc.tools[ev.ToolID]
		// Only what a command that asked GitHub answered is read at all.
		// Every other output is the world going past: a source file the
		// agent opened names the numbers its comments cite, `git log`
		// names the pull request of every commit it prints, and a grep
		// over a repository that talks about pull requests hands back a
		// page of them. None of that is what the thread is working on —
		// and a result nothing is known about (a file read through the
		// Read tool, or a command whose own record the budget stepped
		// over) is the same bet made blind.
		if !asksGitHubRe.MatchString(cmd) {
			return
		}
		// A tool's output is read from the top: what a command has to say
		// about a reference, it says first, and a listing that runs to a
		// megabyte says nothing more by its end.
		sc.text(clip(ev.Text), at, seenFor(cmd))
		// Of those, only the output of a command that reports a remote is
		// read for one: `gh pr view` prints whatever repository the pull
		// request's body links to, and believing the last of them would
		// point every bare "#47" in the thread at a stranger's issue.
		if namesRemoteRe.MatchString(cmd) {
			sc.remotes(ev.Text)
			sc.pushHint(ev.Text)
		}
	case agent.EventText, agent.EventResult:
		// The agent's own prose is read from both ends. A closing report
		// says what it did at the top and links the pull request at the
		// bottom, and a report long enough to be cut is exactly the one
		// whose link is furthest from the start.
		sc.prose(clipEnds(ev.Text), at)
	}
}

// command reads a shell command for what it says about the work: the
// branch it creates or pushes, and the pull request or issue it acts on.
//
// What the command *runs* is read apart from what it *carries*. An agent
// writes a file by handing the shell a here-document, and the body of one
// is data: a test fixture full of `git switch -c x` is not a branch this
// thread made, and a README full of "github.com/o/r" is not the
// repository it is in — believing them renamed a thread's branch to `x`
// and pointed its numbers at o/r. So everything is read out of the
// stripped command, and what it carries is read only when it is handed to
// GitHub under a flag that takes prose — `gh pr create --body-file - <<EOF`
// — and then as prose. A here-document merely standing next to a `gh`
// call is a file again: `cat > notes.md <<EOF … EOF; gh pr view 51` wrote
// notes, and the numbers in them are not this thread's work.
func (sc *scanner) command(cmd string, at time.Time) {
	// From both ends, because it is read as prose: a pull request body
	// long enough to be cut is exactly the one whose "Closes #47" is the
	// last line of it.
	carries := clipEnds(cmd)
	cmd = clip(stripHeredocs(cmd))
	if b := branchOf(cmd); b != "" {
		sc.branch = b
	}
	if namesRemoteRe.MatchString(cmd) {
		sc.remotes(cmd) // `git clone https://github.com/o/r`, `git remote add …`
	}
	sc.text(cmd, at, seenFor(cmd))
	// What a command hands GitHub — the body of a pull request, of an
	// issue, of a comment — is read the way the agent's own prose is,
	// because that is what it is: for its links and for what it says it
	// closes, never for a bare number, which in a body is an example or
	// an aside. "Closes #47" in a body is still the truest sighting a
	// thread has of the issue behind its work.
	if carries != cmd && asksGitHubRe.MatchString(cmd) && bodyFlagRe.MatchString(cmd) {
		sc.prose(carries, at)
	}
	// `gh pr view 51`, `gh issue develop 47`: the bare number after the
	// subcommand is a reference even without a `#`.
	if m := ghTargetRe.FindStringSubmatch(cmd); m != nil {
		k := KindPR
		if m[1] == "issue" {
			k = KindIssue
		}
		sc.add(Ref{Kind: k, Number: atoi(m[2]), Seen: SeenWorked, At: at})
	}
}

// text mines free text — a human's message, a command, a tool's output —
// for references at no more than strength max. Callers hand it a string
// already cut to size, because which end of a long one holds the answer
// depends on what wrote it: clip for a listing, clipEnds for prose.
func (sc *scanner) text(s string, at time.Time, max Seen) {
	sc.mine(s, at, max, true)
}

// prose mines what the agent itself wrote, which is read for a link but
// never for a bare number. When the agent means a pull request it links
// it; a "#12" in a report is nearly always a quotation — of a file it
// read, of an example it is proposing, of the very overview line dispatch
// wrote under the last turn. That last one is the loop outbound records
// are skipped to avoid, arriving by another door: a thread whose subject
// was this overview kept mining its own numbers back out of the agent's
// summary of them.
func (sc *scanner) prose(s string, at time.Time) {
	sc.mine(s, at, SeenMentioned, false)
}

// mine reads the references out of a string: URLs and "Closes #47"
// always, a bare "#12" only when the writer is trusted to have meant one.
func (sc *scanner) mine(s string, at time.Time, max Seen, bare bool) {
	if !mayRefer(s) {
		return
	}
	urls := urlRe.FindAllStringSubmatch(s, -1)
	// One command that returns a page of pull requests is a listing, not
	// work on any of them: `gh pr status` and `gh pr list` would otherwise
	// hand SeenWorked to everything open and let the highest number win.
	if len(urls) > maxWorked {
		max = SeenMentioned
	}
	for _, m := range urls {
		k := KindPR
		if m[3] == "issues" {
			k = KindIssue
		}
		repo := m[1] + "/" + trimGit(m[2])
		sc.noteRepo(repo, repoFromProse)
		sc.add(Ref{Repo: repo, Kind: k, Number: atoi(m[4]), URL: m[0], Seen: max, At: at})
	}
	// "Closes #47" links an issue to the work however casually it was
	// written, so it counts as worked on even in prose: it is the one
	// thing a body says that a machine reads too, and it is how the pull
	// request a thread just opened names the issue behind it.
	for _, m := range closesRe.FindAllStringSubmatch(s, -1) {
		sc.add(Ref{Repo: repoOf(m[1], m[2]), Kind: KindIssue, Number: atoi(m[3]), Seen: SeenWorked, At: at})
	}
	if !bare {
		return
	}
	// A bare "#12" says a number but not what it is; GitHub numbers pull
	// requests and issues from one series, and the log rarely says which.
	// It is recorded as an issue, which is what a human writing "#12" in
	// the message that starts a task nearly always means. It is always
	// the weakest sighting there is, whatever said it: a number in prose
	// is not work, and max cannot lower it further.
	for _, m := range bareRe.FindAllStringSubmatch(s, -1) {
		sc.add(Ref{Repo: repoOf(m[1], m[2]), Kind: KindIssue, Number: atoi(m[3]), Seen: SeenMentioned, At: at})
	}
}

// remotes notes every "owner/repo" a git remote or clone URL names. The
// last one wins: it is the repository the work most recently touched.
// Callers gate this on the command having asked about a remote; remoteRe
// only tightens that, by insisting on a URL shape a remote actually has.
func (sc *scanner) remotes(s string) {
	if !strings.Contains(s, githubHost) {
		return
	}
	for _, m := range remoteRe.FindAllStringSubmatch(clip(s), -1) {
		repo := m[1] + "/" + trimGit(m[2])
		sc.repo = repo
		sc.noteRepo(repo, repoFromRemote)
	}
}

// pushHint reads git's own "create a pull request" advice, which names the
// branch that was just pushed. Like remotes, callers gate it on the command
// having been one that talks to a remote: that URL shape can appear in a
// file the agent read or a page it fetched, and the branch it names is
// rendered into a line dispatch signs its own name to. `branchName` finishes
// the job — what git itself forbids in a branch cannot reach the line
// either, so no backtick can close the code span the branch is rendered in.
func (sc *scanner) pushHint(s string) {
	if !strings.Contains(s, githubHost) {
		return
	}
	// From the ends: git's advice comes after however much progress and
	// `remote:` output the server had to say first.
	if m := pushRe.FindStringSubmatch(clipEnds(s)); m != nil {
		sc.branch = m[1]
	}
}

// How evidence for "which repository is this thread in" is weighed by the
// fallback in state, for a thread where no command ever named a remote
// outright. A remote is worth more than prose because it is the machine
// saying where it is rather than someone saying what they read.
const (
	repoFromProse  = 1
	repoFromRemote = 2
)

// noteRepo records evidence that the thread is working in repo. Both
// places that find one come through here, so the scoring is one rule in
// one place rather than two bare numbers in two functions.
func (sc *scanner) noteRepo(repo string, weight int) {
	sc.repos[repo] += weight
}

// add records a sighting, keeping the strongest one and the latest time.
//
// A sighting that named no repository — a bare "#12", a `gh pr view 49`
// whose output echoed no URL — is stamped with the repository in hand at
// the time, when one is known by then. Two threads' worth of work in one
// conversation (a clone of org/A, then one of org/B) can each mention
// "#3" and mean different pull requests; without the stamp they would key
// alike and merge into one, and the loser would be rendered with the
// other's link. A sighting from before any repository was named keeps an
// empty one and is resolved in state, where the thread's repository is.
func (sc *scanner) add(r Ref) {
	if r.Number <= 0 {
		return
	}
	if r.Repo == "" {
		r.Repo = sc.repo
	}
	key := string(r.Kind) + "#" + strconv.Itoa(r.Number)
	if r.Repo != "" {
		key = r.Repo + "/" + key
	}
	cur, ok := sc.refs[key]
	if !ok {
		c := r
		sc.refs[key] = &c
		return
	}
	bump(cur, r)
	if cur.URL == "" {
		cur.URL = r.URL
	}
	if cur.Repo == "" {
		cur.Repo = r.Repo
	}
}

// bump folds a new sighting of a reference into the one already held: the
// strongest way it was seen, and the last time it was seen. Both places
// that merge sightings — one keyed by what was written, one by what it
// turned out to mean — agree on that rule through here.
func bump(dst *Ref, r Ref) {
	if r.Seen > dst.Seen {
		dst.Seen = r.Seen
	}
	if r.At.After(dst.At) {
		dst.At = r.At
	}
}

// state resolves the sightings into the answer: the repository, then the
// one pull request and the one issue the thread is about, then the rest.
func (sc *scanner) state() State {
	st := State{Repo: sc.repo, Branch: sc.branch}
	// No command named a remote, so fall back to the repository most
	// linked to. See the package doc for what this is willing to trust.
	if st.Repo == "" {
		best := 0
		for repo, n := range sc.repos {
			if n > best || (n == best && repo < st.Repo) {
				st.Repo, best = repo, n
			}
		}
	}
	all := make([]Ref, 0, len(sc.refs))
	for _, r := range sc.refs {
		if r.Repo == "" {
			r.Repo = st.Repo // a bare "#12" belongs to the repo in hand
		}
		all = append(all, *r)
	}
	all = collapse(all)
	// The URL comes last, once the kind has settled: a "#49" the human
	// wrote and a `gh pr view 49` are one reference, and it is a pull
	// request's URL that reference wants.
	for i := range all {
		r := &all[i]
		if r.Repo != "" && !strings.Contains(r.URL, "/"+path(r.Kind)+"/") {
			r.URL = "https://github.com/" + r.Repo + "/" + path(r.Kind) + "/" + strconv.Itoa(r.Number)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Seen != all[j].Seen {
			return all[i].Seen > all[j].Seen
		}
		if !all[i].At.Equal(all[j].At) {
			return all[i].At.After(all[j].At)
		}
		if all[i].Number != all[j].Number {
			return all[i].Number > all[j].Number
		}
		// collapse leaves one reference per repository and number, so the
		// repository settles every remaining tie — and the order of the
		// answer must not depend on a map's iteration order.
		return all[i].Repo < all[j].Repo
	})
	for i := range all {
		switch {
		case all[i].Kind == KindPR && st.PR == nil:
			st.PR = &all[i]
		case all[i].Kind == KindIssue && st.Issue == nil:
			st.Issue = &all[i]
		case len(st.Also) < maxAlso:
			st.Also = append(st.Also, all[i])
		}
	}
	return st
}

// collapse folds every sighting of one number in one repository into one
// reference. Sightings arrive keyed by what they said — a URL names the
// repository, `gh pr view 49` and a bare "#49" do not — and only here, with
// the repository in hand, can they be recognised as the same thing. Kind
// collapses with them: GitHub numbers pull requests and issues from one
// series, so "#49" written by a human and pull request 49 are one, and the
// pull request is the more specific reading.
func collapse(all []Ref) []Ref {
	at := map[string]int{} // repo#number → index in out
	out := all[:0]
	for _, r := range all {
		key := r.Repo + "#" + strconv.Itoa(r.Number)
		i, ok := at[key]
		if !ok {
			at[key] = len(out)
			out = append(out, r)
			continue
		}
		c := &out[i]
		bump(c, r)
		if r.Kind == KindPR {
			c.Kind = KindPR
		}
		if c.URL == "" || (r.URL != "" && r.Kind == c.Kind) {
			c.URL = r.URL
		}
	}
	return out
}

// ownerRepo matches "owner/repo" and captures the two halves. Four
// regexes need it — a URL, a bare "owner/repo#12", a "Closes owner/repo#12"
// and a git remote — and what counts as a repository name is one decision,
// not four.
const ownerRepo = `([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)`

// numbered are the `gh pr` and `gh issue` subcommands that act on one
// reference, and so are followed by its number. Two regexes need the
// list and would drift apart by hand: one asks whether a command worked
// on something, the other reads which number it worked on. A subcommand
// that acts on no particular number — `status`, `list` — belongs to
// neither, which is why the list is exactly the ones that do.
const numbered = `view|checkout|edit|diff|merge|ready|comment|close|reopen|develop|review`

// remoteCmds are the commands that talk to a remote, and so may say which
// repository the thread is working in. Two regexes are built on the list:
// one asks whether a command named a remote, the other whether its output
// is worth reading at all.
const remoteCmds = `git\s+(?:remote|clone|ls-remote|config|push|pull|fetch)|gh\s+repo`

// wrappers are the commands that run another command, and the variable
// assignments that prefix one. What follows one is still where a command
// begins: `timeout 60 gh pr create` created a pull request, and losing it
// costs the thread the very reference this package exists to find.
const wrappers = `(?:(?:sudo|nohup|command|time|exec)\s+|env\s+|timeout\s+(?:-\S+\s+)*\S+\s+|\w+=\S*\s+)*`

// cmdStart is where a command begins on a shell line: the start of it, or
// just past a separator that starts another one, in either case past any
// indentation and any wrapper. Every command this package recognises is
// anchored there, so a command that merely *quotes* one — `grep -rn "gh pr
// create" .`, whose output is a source file, or `rg "git clone"` — is not
// mistaken for it.
const cmdStart = `(?:^|[\n;&|(])\s*` + wrappers

var (
	urlRe    = regexp.MustCompile(`https?://github\.com/` + ownerRepo + `/(pull|issues)/(\d+)`)
	bareRe   = regexp.MustCompile(`(?:^|[^\w/#-])(?:` + ownerRepo + `)?#(\d{1,5})\b`)
	closesRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+(?:` + ownerRepo + `)?#(\d{1,5})\b`)
	// remoteRe: a repository named the way a remote names one — over ssh
	// (`git@github.com:o/r`) or by a URL with a scheme. A bare
	// "github.com/owner/name" is not enough: that is how go.mod, a README
	// and half the world's prose spell a repository nobody is working on.
	remoteRe = regexp.MustCompile(`(?:git@|(?:ssh|git|https?)://(?:git@)?)github\.com[:/]` + ownerRepo)
	// namesRemoteRe: the commands whose output is allowed to name the
	// repository being worked in. Everything else — `cat go.mod`, a README,
	// the agent's own prose — may mention a repository without dispatch
	// concluding the thread is working in it.
	namesRemoteRe = regexp.MustCompile(cmdStart + `(?:` + remoteCmds + `)\b`)
	// asksGitHubRe: the commands whose *output* is evidence at all —
	// the ones that asked GitHub or a remote, and so came back with an
	// answer about this thread's work rather than with a file's contents.
	asksGitHubRe = regexp.MustCompile(cmdStart + `(?:` + remoteCmds + `|gh\s)`)
	pushRe       = regexp.MustCompile(`github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/new/` + branchName)
	// branchRe: the commands that name a branch dispatch should believe.
	// Every capture uses branchName, which cannot start with "-" — git
	// forbids such a branch, so `git branch --show-current` and
	// `git push origin --delete stale` name no branch at all rather than
	// naming their own flag. `git branch` is only believed bare: with a
	// flag it lists (`-a`) or deletes (`-d old`) far more often than it
	// creates.
	branchRe = regexp.MustCompile(cmdStart + `git\s+(?:(?:checkout\s+-b|switch\s+-c)\s+(?:-\S+\s+)*|branch\s+)` + branchName)
	pushToRe = regexp.MustCompile(cmdStart + `git\s+push\s+(?:-\S+\s+)*origin\s+(?:HEAD:)?` + branchName)
	headRe   = regexp.MustCompile(`--head[=\s]+` + branchName)
	createRe = regexp.MustCompile(cmdStart + `gh\s+(?:pr|issue)\s+create\b`)
	// workRe: subcommands that act on one pull request or issue. `status`
	// and `list` are absent on purpose — they return everything open, and
	// grading a listing as work lets an unrelated pull request win.
	workRe = regexp.MustCompile(cmdStart + `gh\s+(?:pr|issue)\s+(?:` + numbered + `)\b`)
	// bodyFlagRe: the flags that hand a command's here-document to GitHub
	// as prose — a pull request's body, an issue's, a comment's. Without
	// one, a here-document next to a `gh` call is a file like any other:
	// `cat > notes.md <<EOF … EOF; gh pr view 51` writes notes.
	bodyFlagRe = regexp.MustCompile(`--body(?:-file)?\b|(?:^|\s)-[bF](?:\s|=)`)
	// ghTargetRe: the number a `gh pr`/`gh issue` subcommand acts on.
	ghTargetRe = regexp.MustCompile(cmdStart + `gh\s+(pr|issue)\s+(?:` + numbered + `)\s+#?(\d{1,5})\b`)
)

// githubHost is the one hostname any of this is about. Four places test
// for it — the two cheap filters that decide whether a record or a string
// is worth reading at all, and the two miners that read one — and they
// must not drift apart.
const githubHost = "github.com"

// branchName is a branch as a command spells it. The first character
// cannot be "-", so a flag is never mistaken for a branch.
const branchName = `([A-Za-z0-9._/][A-Za-z0-9._/-]*)`

// mayRefer reports whether text could possibly hold a reference, without
// running a regex over it. Every pattern text mines needs a "#" or the
// hostname, and most of what a thread logs — source files, test output,
// diffs — has neither.
func mayRefer(s string) bool {
	return strings.Contains(s, "#") || strings.Contains(s, githubHost)
}

// affordable returns the newest records a scan can pay to decode. The
// budget is spent from the end because that is where the answer is: the
// pull request a thread is about was named while the work was being done,
// not a hundred thousand lines of file-reading ago. Records mayMatter
// rules out are never decoded, so they are free and cost no budget — a
// thread of quiet output is read whole however long it is.
func affordable(recs []store.Record) []store.Record {
	n := 0
	keep := make([]store.Record, 0, len(recs))
	for i := len(recs) - 1; i >= 0; i-- {
		p := recs[i].Payload
		if !mayMatter(p) {
			continue
		}
		// A record bigger than the whole budget could never have been
		// afforded wherever in the thread it fell, so skipping it costs
		// nothing and leaves the budget where it was. Letting it end the
		// walk instead would throw away every older record behind it —
		// including the small one that named the pull request, and the
		// overview would come back empty with nothing to say why.
		if len(p) > maxScan {
			continue
		}
		if n += len(p); n > maxScan {
			break
		}
		keep = append(keep, recs[i])
	}
	slices.Reverse(keep) // back to oldest-first, which is what a scan reads
	return keep
}

// toolInput is how a logged event that carries a command spells it. Only
// tool calls have one; every other event marshals "ToolInput":null.
var toolInput = []byte(`"ToolInput":{`)

// mayMatter reports whether a record could hold anything a scan reads,
// judged on its bytes so that most of a thread is never decoded at all.
// A record matters when it could name a reference — every pattern needs a
// "#" or the hostname, both of which JSON leaves as they are — or when it
// is a tool call, whose command names the branch. What is left is the bulk
// of a coding thread: files read, diffs, test output, each carrying the
// whole vendor message in Raw, and each cheap to skip and dear to decode.
func mayMatter(p []byte) bool {
	return bytes.Contains(p, []byte("#")) ||
		bytes.Contains(p, []byte(githubHost)) ||
		bytes.Contains(p, toolInput)
}

// seenFor grades a command: what it creates is the thread's own, what it
// acts on is what the thread works on, anything else is talk.
func seenFor(cmd string) Seen {
	switch {
	case cmd == "":
		return SeenMentioned
	case createRe.MatchString(cmd):
		return SeenCreated
	case workRe.MatchString(cmd):
		return SeenWorked
	}
	return SeenMentioned
}

// branchOf is the branch a command creates, pushes or targets.
func branchOf(cmd string) string {
	for _, re := range []*regexp.Regexp{branchRe, pushToRe, headRe} {
		if m := re.FindStringSubmatch(cmd); m != nil && m[1] != "" && m[1] != "HEAD" {
			return m[1]
		}
	}
	return ""
}

func repoOf(owner, name string) string {
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + trimGit(name)
}

func path(k Kind) string {
	if k == KindIssue {
		return "issues"
	}
	return "pull"
}

func trimGit(s string) string { return strings.TrimSuffix(s, ".git") }

// heredocRe matches a here-document operator and captures its delimiter:
// `<<EOF`, `<<-EOF`, `<<'PY'`, `<< "SQL"`. Quoted, the delimiter is
// whatever it says; bare, it must be two or more capitals, which is how
// every here-document anybody writes spells one — and what tells the
// operator from a `<<` that is not one, a shell shift or a C++ stream
// gone past in a grep pattern, whose body would be the rest of the
// command and whose delimiter would never come.
var heredocRe = regexp.MustCompile(`<<-?\s*(?:'([^']+)'|"([^"]+)"|([A-Z_][A-Z0-9_]+))`)

// stripHeredocs removes the bodies of a command's here-documents, leaving
// what the command runs. An agent writes files through the shell — a test
// fixture, a PR body, a whole Python script — and everything between the
// operator and its delimiter is that file, not another command. The
// operator goes with the body, so each pass strictly shortens the string
// and the walk ends.
func stripHeredocs(cmd string) string {
	for {
		m := heredocRe.FindStringSubmatchIndex(cmd)
		if m == nil {
			return cmd
		}
		head, delim, rest := cmd[:m[0]], submatch(cmd, m), cmd[m[1]:]
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			// No line after the operator, so no body was written: drop
			// the operator alone and keep the rest of the command. A
			// `<<` that was never one — `echo "a << bb" && gh pr create`
			// — must not take the real command after it.
			cmd = head + rest
			continue
		}
		// Joined by a newline, not by nothing: what follows the body is
		// the next command, and it must still begin a line for cmdStart
		// to recognise it as one.
		cmd = head + "\n" + afterHeredoc(rest[nl+1:], delim)
	}
}

// submatch is the delimiter heredocRe captured, whichever of its three
// spellings — single-quoted, double-quoted, bare — matched.
func submatch(s string, m []int) string {
	for i := 2; i+1 < len(m); i += 2 {
		if m[i] >= 0 {
			return s[m[i]:m[i+1]]
		}
	}
	return ""
}

// afterHeredoc returns what follows the line that closes a here-document.
// A body that is never closed — a command cut short, a delimiter the clip
// took — ends the command, which is what an unterminated one does to a
// shell too.
func afterHeredoc(body, delim string) string {
	for i := 0; i < len(body); {
		line := body[i:]
		end := len(body)
		if j := strings.IndexByte(line, '\n'); j >= 0 {
			line, end = line[:j], i+j+1
		}
		if strings.TrimSpace(line) == delim {
			return body[end:]
		}
		if end == len(body) {
			break
		}
		i = end
	}
	return ""
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// clipEnds keeps both ends of a long string and drops the middle, for
// text whose reference could be at either — and which, when it is long,
// has a middle that is neither the summary nor the link. The two halves
// are joined by a newline so nothing can match across the seam.
func clipEnds(s string) string {
	if len(s) <= 2*maxText {
		return s
	}
	return s[:maxText] + "\n" + s[len(s)-maxText:]
}

func clip(s string) string {
	if len(s) > maxText {
		return s[:maxText]
	}
	return s
}
