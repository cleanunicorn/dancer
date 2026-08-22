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
}

// Prompt asks the human to pick one choice. Used for permission requests.
type Prompt struct {
	ID      string
	Choices []string // e.g. ["allow", "deny"]
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
