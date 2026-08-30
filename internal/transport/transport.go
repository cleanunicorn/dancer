// Package transport defines the communication channels humans use to reach
// dispatch: Slack, a terminal, the web UI, later Telegram.
//
// A Transport moves messages and knows how to address a conversation
// (ThreadID). It has no idea what the messages mean — that is the job of a
// surface (package surface), and several surfaces can share one transport.
//
// A conversation belongs to dispatch, not to the transport it started on.
// Its ThreadID is minted by the transport that hosts it (Slack: a channel
// and a message ts) and that transport renders it natively; but any
// transport may post into it, and a transport that can list conversations
// (Observer — the web UI) is shown every one of them, with what humans
// said on the other transports relayed as Outbound.From. History is not
// the transport's to keep: it is the coordinator's log, read back through
// History.
package transport

import (
	"context"
	"time"
)

// dispatch's own lines are transport markup, not Markdown, and one piece
// of it is shared: Link writes "<url|label>" — Slack's own form, which the
// web UI's mrkdwn renderer reads and the terminal turns into an OSC 8
// hyperlink (markup.go). Agent text (Outbound.Markdown) is never touched.
//
// ThreadID identifies a conversation on a transport. For Slack this is
// "<channel>/<thread_ts>" ("<channel>/" posts at top level); for the
// terminal it is a single constant thread.
type ThreadID string

// Inbound is a message from a human.
//
// Thread may name a channel alone ("<channel>/"): the human wants a new
// conversation there. The coordinator asks the transport that owns the
// channel (ChannelLister) to open one (ThreadOpener) and carries on with
// the id it returns.
type Inbound struct {
	Transport string    // transport name, e.g. "slack", "terminal"
	Thread    ThreadID  // conversation the message belongs to
	UserID    string    // transport-specific user identifier
	UserName  string    // display name when the transport knows it; shown where UserID means nothing
	Text      string    // raw text as typed
	Decision  *Decision // set when the message answers a Prompt
	// Files are the attachments the human sent with the message — an
	// image pasted into Slack, a log, a PDF — already downloaded by the
	// transport. A transport that cannot carry uploads (the terminal)
	// never sets it. The surface hands them to the task and the executor
	// copies them into the agent's environment.
	Files []File
}

// Decision is a human's answer to a Prompt.
type Decision struct {
	PromptID string
	Choice   string // one of Prompt.Choices
}

// Outbound is a message to a human.
type Outbound struct {
	Thread ThreadID
	Text   string
	Prompt *Prompt // non-nil: render as a question with buttons/choices
	Files  []File  // attachments uploaded after the text
	// From, when set, says a human wrote Text (or made Decision) on
	// another transport: the coordinator relays what people say so every
	// transport showing the thread has the whole conversation. A
	// transport renders it as that person's message, not dispatch's
	// (Slack: "💬 *name* via web: …"; the web UI: a message bubble with
	// their name). Transports never write it themselves.
	From *Author
	// Decision, with From, says that person answered the prompt it names
	// (Decision.PromptID) with Decision.Choice. A transport that posted
	// that prompt settles it — Slack swaps the buttons for the outcome —
	// and otherwise shows who answered what. Text is empty.
	Decision *Decision
	// Markdown says Text is Markdown as an agent writes it (CommonMark:
	// **bold**, # headings, [links](url), fenced code), not the
	// transport's own markup. A transport with a different dialect
	// converts or hands it to a renderer that understands it.
	Markdown bool
	// Mention is the transport user id (Inbound.UserID) of the human this
	// message addresses: the transport renders it the way that notifies
	// them (Slack: "<@U…> " in front of Text), so someone who muted the
	// thread still hears that the agent finished or needs an answer. It
	// is honoured on ordinary and prompt text: a keyed message is edited in place
	// and must not re-notify, so transports ignore Mention there; and a
	// transport that hands Markdown text to a separate renderer may not
	// render the mention at all, so surfaces set it on their own lines,
	// never on the agent's. A transport without mentions (the terminal)
	// ignores it.
	Mention string
	// Key names a message the surface will post again. A transport that
	// can edit what it posted (Slack, a terminal) replaces the earlier
	// message with this Key on the same Thread instead of adding a new
	// one; an empty Text removes it. Surfaces use this for the live
	// status line of a running task. Transports that cannot edit post
	// every update as a new message and ignore removals.
	Key string
}

// Author is the human behind a relayed message (Outbound.From).
type Author struct {
	ID   string // Inbound.UserID on their transport
	Name string // Inbound.UserName, or ID when the transport had none
	Via  string // the transport they wrote on
}

// Display is the name to show for the author.
func (a Author) Display() string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

// File is an attachment, either way: one the human sent (Inbound.Files)
// or one the agent produced (Outbound.Files). Data stays out of the event
// log, which records the message around it; only Name is kept there.
type File struct {
	Name string
	Data []byte `json:"-"`
}

// Prompt asks the human for a decision.
//
// Permission prompts set Choices ("allow"/"deny"); questions set Options
// and usually FreeText, meaning a typed reply in the thread also answers.
// Decision.Choice carries the chosen value (or the typed text).
type Prompt struct {
	ID       string
	Choices  []string // simple prompts: the accepted values
	Question string   // questions: the text shown above the options
	Options  []Option // questions: selectable answers
	FreeText bool     // questions: accept a typed answer too
}

// Option is one selectable answer of a Prompt.
type Option struct {
	Value       string // what comes back in Decision.Choice
	Label       string // button text
	Description string
}

// Values returns the accepted decision values of a prompt.
func (p *Prompt) Values() []string {
	if len(p.Options) == 0 {
		return p.Choices
	}
	out := make([]string, 0, len(p.Options))
	for _, o := range p.Options {
		out = append(out, o.Value)
	}
	return out
}

// ThreadTracker is implemented by transports that only forward replies
// from threads they know about (Slack). The coordinator re-seeds them with
// every stored task thread at startup so conversations survive restarts.
type ThreadTracker interface {
	Remember(thread ThreadID)
}

// ThreadCloser is implemented by transports that can stop following a
// thread again (Slack). Forget is the inverse of ThreadTracker.Remember:
// plain replies in the thread are ignored afterwards, until a human
// addresses the bot there directly — or Follow lifts it, when the human
// reopened the thread from another transport.
type ThreadCloser interface {
	Forget(thread ThreadID)
	Follow(thread ThreadID)
}

// Reactor is implemented by transports that can mark a conversation with
// an emoji (Slack). It is best-effort: a transport lacking the permission
// returns an error the caller only logs. Unreact takes a mark back; the
// coordinator uses the pair to show where a thread stands on its root
// message (working, waiting for a decision, answered and waiting for the
// next message, failed, closed) — one mark at a time.
type Reactor interface {
	React(ctx context.Context, thread ThreadID, emoji string) error
	Unreact(ctx context.Context, thread ThreadID, emoji string) error
}

// Channel is a place a transport can open conversations in: a Slack
// channel, a web channel. ID is the first part of the ThreadIDs under it.
type Channel struct {
	ID   string
	Name string // human name; ID when the transport has none
}

// ChannelLister is implemented by transports that know their channels.
// The coordinator uses it to tell whose channel an Inbound addressed, and
// to list every channel to an Observer.
type ChannelLister interface {
	Channels() []Channel
}

// ThreadOpener is implemented by transports that can start a conversation
// in a channel of theirs on request: the coordinator calls it when a
// human on another transport (or on the web UI, for its own channels)
// writes to "<channel>/". The transport posts msg as the root of the new
// thread — msg.From names who asked — and returns its id.
type ThreadOpener interface {
	OpenThread(ctx context.Context, channel string, msg Outbound) (ThreadID, error)
}

// Observer is implemented by transports that show every conversation,
// whichever transport hosts it (the web UI). The coordinator sends them
// each Outbound of every thread, relays what humans say on the other
// transports (Outbound.From) — and what their own humans say, so they
// need no memory of their own — and routes what they send on any thread
// to that thread's task.
type Observer interface {
	ObservesAllThreads()
}

// ThreadInfo is one conversation as History lists it.
type ThreadInfo struct {
	ID        ThreadID
	Transport string // the transport hosting it
	Channel   string
	Title     string    // the first thing the human asked
	Status    string    // the task's status (store.Status*), "" without a task
	Closed    bool      // the conversation was closed
	Requester string    // who started it
	Updated   time.Time // the task's last change
	// What is working on it: the agent definition's name, the model the
	// session resolved to (what was asked for until the agent reports),
	// the environment kind (local, docker, ssh) and the agent's session
	// id. Empty without a task.
	Agent       string
	Model       string
	Environment string
	Session     string
}

// AgentInfo is one agent definition as History lists it: enough to
// offer it as a choice, not its configuration.
type AgentInfo struct {
	Name        string
	Model       string // as configured; "" is the agent CLI's default
	Environment string // environment kind
}

// History is the read side an Observer needs: the channels of every
// transport, the agents that can be run, the conversations dispatch knows
// and what was said in one, rebuilt from the coordinator's log. Messages
// come back as the Outbounds the thread saw, with what humans wrote as
// Outbound.From entries, oldest first, and the tools the agent ran
// between them as Entry.Tool; live status lines (keyed messages) are
// not part of it.
type History interface {
	// Channels lists every transport's channels, keyed by transport name.
	Channels() map[string][]Channel
	// Agents lists the definitions a human can name in `run <agent>`.
	Agents(ctx context.Context) ([]AgentInfo, error)
	Threads(ctx context.Context) ([]ThreadInfo, error)
	Messages(ctx context.Context, thread ThreadID, limit int) ([]Entry, error)
}

// Entry is one message of a thread's history, with when it was logged.
// Exactly one of Message and Tool is set.
type Entry struct {
	At      time.Time
	Message Outbound
	Tool    *ToolCall
}

// ToolCall is one tool the agent ran, as the log remembers it: the call
// and, once it came back, its result. An observer can show a turn's
// tool calls after the fact; while the turn runs, the status line is
// the live view.
type ToolCall struct {
	ID       string
	Name     string
	Input    string        // the gist of the input: a command, a path, a pattern, else its JSON
	Sub      bool          // run by a sub-agent
	Done     bool          // the result arrived
	Error    bool          // the tool failed, or the call was refused
	Denied   bool          // refused by policy (the agent's own, or a human) rather than failed
	Duration time.Duration // from the call to its result; 0 until Done
}

// Transport is the interface every communication channel implements.
type Transport interface {
	// Name returns the transport name reported in Inbound.Transport.
	Name() string
	// Run blocks, delivering human input to inbox until ctx is cancelled.
	Run(ctx context.Context, inbox chan<- Inbound) error
	// Send renders one message into the given thread.
	Send(ctx context.Context, msg Outbound) error
}
