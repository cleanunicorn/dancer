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
// Scan is a projection over records the caller already has, not new state:
// it stores nothing and appends nothing.
package work

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/transport"
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
// handed a megabyte.
const maxText = 16 << 10

// maxAlso is how many extra references the overview carries. Past a
// handful it stops being an overview.
const maxAlso = 4

// Scan projects the state of the work out of a thread's records, oldest
// first — what Store.ThreadRecords returns. Records it cannot parse are
// skipped; outbound records are ignored on purpose, because dancer's own
// overview lines carry the very references they were mined from and would
// otherwise keep re-confirming themselves.
func Scan(recs []store.Record) State {
	sc := scanner{refs: map[string]*Ref{}, tools: map[string]string{}, repos: map[string]int{}}
	for _, r := range recs {
		switch r.Kind {
		case "inbound":
			var in transport.Inbound
			if json.Unmarshal(r.Payload, &in) != nil {
				continue
			}
			sc.text(in.Text, r.At, SeenMentioned)
		case "agent":
			var ev agent.Event
			if json.Unmarshal(r.Payload, &ev) != nil {
				continue
			}
			sc.event(ev, r.At)
		}
	}
	return sc.state()
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
func (sc *scanner) event(ev agent.Event, at time.Time) {
	switch ev.Type {
	case agent.EventToolUse, agent.EventNeedsPermission:
		cmd, _ := ev.ToolInput["command"].(string)
		if cmd == "" {
			return
		}
		if ev.ToolID != "" {
			sc.tools[ev.ToolID] = cmd
		}
		sc.command(cmd, at)
	case agent.EventToolResult:
		sc.text(ev.Text, at, seenFor(sc.tools[ev.ToolID]))
		sc.remotes(ev.Text)
		sc.pushHint(ev.Text)
	case agent.EventText, agent.EventResult:
		sc.text(ev.Text, at, SeenMentioned)
	}
}

// command reads a shell command for what it says about the work: the
// branch it creates or pushes, and the pull request or issue it acts on.
func (sc *scanner) command(cmd string, at time.Time) {
	cmd = clip(cmd)
	if b := branchOf(cmd); b != "" {
		sc.branch = b
	}
	sc.remotes(cmd)
	sc.text(cmd, at, seenFor(cmd))
	// `gh pr view 51`, `gh issue develop 47`: the bare number after the
	// subcommand is a reference even without a `#`.
	if m := ghTargetRe.FindStringSubmatch(cmd); m != nil {
		k := KindPR
		if m[1] == "issue" {
			k = KindIssue
		}
		sc.add(Ref{Kind: k, Number: atoi(m[3]), Seen: SeenWorked, At: at})
	}
}

// text mines free text — a human's message, the agent's prose, a tool's
// output — for references at no more than strength max.
func (sc *scanner) text(s string, at time.Time, max Seen) {
	if s == "" {
		return
	}
	s = clip(s)
	for _, m := range urlRe.FindAllStringSubmatch(s, -1) {
		k := KindPR
		if m[3] == "issues" {
			k = KindIssue
		}
		repo := m[1] + "/" + trimGit(m[2])
		sc.repos[repo]++
		sc.add(Ref{Repo: repo, Kind: k, Number: atoi(m[4]), URL: m[0], Seen: max, At: at})
	}
	// "Closes #47" links an issue to the work however casually it was
	// written, so it counts as worked on even in prose.
	for _, m := range closesRe.FindAllStringSubmatch(s, -1) {
		sc.add(Ref{Repo: repoOf(m[1], m[2]), Kind: KindIssue, Number: atoi(m[3]), Seen: SeenWorked, At: at})
	}
	// A bare "#12" says a number but not what it is; GitHub numbers pull
	// requests and issues from one series, and the log rarely says which.
	// It is recorded as an issue, which is what a human writing "#12" in
	// the message that starts a task nearly always means. It is always
	// the weakest sighting there is, whatever said it: a number in prose
	// is not work, and max cannot lower it further.
	for _, m := range bareRe.FindAllStringSubmatch(s, -1) {
		sc.add(Ref{Repo: repoOf(m[2], m[3]), Kind: KindIssue, Number: atoi(m[4]), Seen: SeenMentioned, At: at})
	}
}

// remotes notes every "owner/repo" a git remote or clone URL names. The
// last one wins: it is the repository the work most recently touched.
func (sc *scanner) remotes(s string) {
	for _, m := range remoteRe.FindAllStringSubmatch(clip(s), -1) {
		repo := m[1] + "/" + trimGit(m[2])
		sc.repo = repo
		sc.repos[repo] += 2 // a remote names the repo more surely than prose
	}
}

// pushHint reads git's own "create a pull request" advice, which names the
// branch that was just pushed.
func (sc *scanner) pushHint(s string) {
	if m := pushRe.FindStringSubmatch(clip(s)); m != nil {
		sc.branch = m[1]
	}
}

// add records a sighting, keeping the strongest one and the latest time.
func (sc *scanner) add(r Ref) {
	if r.Number <= 0 {
		return
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
		return all[i].Number > all[j].Number
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

var (
	urlRe    = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/(pull|issues)/(\d+)`)
	bareRe   = regexp.MustCompile(`(^|[^\w/#-])(?:([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+))?#(\d{1,5})\b`)
	closesRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+(?:([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+))?#(\d{1,5})\b`)
	remoteRe = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)`)
	pushRe   = regexp.MustCompile(`github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/new/(\S+)`)
	// branchRe: the commands that name a branch dancer should believe.
	branchRe = regexp.MustCompile(`\bgit\s+(?:checkout\s+-b|switch\s+-c|branch)\s+(?:-\S+\s+)*([A-Za-z0-9._/-]+)`)
	pushToRe = regexp.MustCompile(`\bgit\s+push\s+(?:-\S+\s+|--\S+\s+)*origin\s+(?:HEAD:)?([A-Za-z0-9._/-]+)`)
	headRe   = regexp.MustCompile(`--head[=\s]+([A-Za-z0-9._/-]+)`)
	createRe = regexp.MustCompile(`\bgh\s+(?:pr|issue)\s+create\b`)
	workRe   = regexp.MustCompile(`\bgh\s+(?:pr|issue)\s+(?:view|checkout|edit|diff|merge|ready|comment|close|reopen|develop|status|review)\b`)
	// ghTargetRe: the number a `gh pr`/`gh issue` subcommand acts on.
	ghTargetRe = regexp.MustCompile(`\bgh\s+(pr|issue)\s+(view|checkout|edit|diff|merge|ready|comment|close|reopen|develop|review)\s+#?(\d{1,5})\b`)
)

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

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func clip(s string) string {
	if len(s) > maxText {
		return s[:maxText]
	}
	return s
}
