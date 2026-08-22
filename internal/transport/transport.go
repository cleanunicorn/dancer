// Package transport defines the communication channels humans use to reach
// dancer: Slack, a terminal, later Telegram.
//
// A Transport moves messages and knows how to address a conversation
// (ThreadID). It has no idea what the messages mean — that is the job of a
// surface (package surface), and several surfaces can share one transport.
package transport

import "context"

// ThreadID identifies a conversation on a transport. For Slack this is
// "<channel>/<thread_ts>" ("<channel>/" posts at top level); for the
// terminal it is a single constant thread.
type ThreadID string

// Inbound is a message from a human.
type Inbound struct {
	Transport string    // transport name, e.g. "slack", "terminal"
	Thread    ThreadID  // conversation the message belongs to
	UserID    string    // transport-specific user identifier
	Text      string    // raw text as typed
	Decision  *Decision // set when the message answers a Prompt
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
}

// File is an attachment.
type File struct {
	Name string
	Data []byte
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

// Transport is the interface every communication channel implements.
type Transport interface {
	// Name returns the transport name reported in Inbound.Transport.
	Name() string
	// Run blocks, delivering human input to inbox until ctx is cancelled.
	Run(ctx context.Context, inbox chan<- Inbound) error
	// Send renders one message into the given thread.
	Send(ctx context.Context, msg Outbound) error
}
