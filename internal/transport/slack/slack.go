// Package slack is a Slack transport over Socket Mode (no public URL needed).
//
// Thread mapping: ThreadID is "<channel>/<thread_ts>". A mention or DM at top level
// starts a thread under that message; replies in a thread the bot is
// active in are forwarded as follow-ups. Permission prompts are rendered
// as Block Kit buttons.
package slack

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/cleanunicorn/dancer/internal/transport"
)

// Transport implements transport.Transport for Slack.
type Transport struct {
	api *slack.Client
	sm  *socketmode.Client
	log *slog.Logger

	botUserID    string
	allowedUsers map[string]bool

	mu      sync.Mutex
	threads map[transport.ThreadID]bool // threads the bot has posted in
}

// New builds a Socket Mode Slack transport. allowedUsers may be empty.
func New(appToken, botToken string, allowedUsers []string, log *slog.Logger) (*Transport, error) {
	if log == nil {
		log = slog.Default()
	}
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	auth, err := api.AuthTest()
	if err != nil {
		return nil, fmt.Errorf("slack: auth test: %w", err)
	}
	c := &Transport{
		api:          api,
		sm:           socketmode.New(api),
		log:          log,
		botUserID:    auth.UserID,
		allowedUsers: map[string]bool{},
		threads:      map[transport.ThreadID]bool{},
	}
	for _, u := range allowedUsers {
		c.allowedUsers[u] = true
	}
	return c, nil
}

func (c *Transport) Name() string { return "slack" }

// BotUserID returns the bot's Slack user id (for doctor output).
func (c *Transport) BotUserID() string { return c.botUserID }

func (c *Transport) Run(ctx context.Context, inbox chan<- transport.Inbound) error {
	go func() {
		if err := c.sm.RunContext(ctx); err != nil && ctx.Err() == nil {
			c.log.Error("slack socket mode stopped", "err", err)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt := <-c.sm.Events:
			c.handle(ctx, evt, inbox)
		}
	}
}

func (c *Transport) handle(ctx context.Context, evt socketmode.Event, inbox chan<- transport.Inbound) {
	switch evt.Type {
	case socketmode.EventTypeConnected:
		c.log.Info("slack connected", "bot_user", c.botUserID)
	case socketmode.EventTypeEventsAPI:
		ev, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		if evt.Request != nil {
			c.sm.Ack(*evt.Request)
		}
		if ev.Type != slackevents.CallbackEvent {
			return
		}
		switch inner := ev.InnerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			c.deliver(ctx, inbox, inner.Channel, inner.TimeStamp, inner.ThreadTimeStamp, inner.User, inner.Text, true)
		case *slackevents.MessageEvent:
			if inner.BotID != "" || inner.User == c.botUserID || inner.SubType != "" {
				return
			}
			mentioned := strings.Contains(inner.Text, "<@"+c.botUserID+">")
			if mentioned {
				return // delivered via AppMentionEvent
			}
			c.deliver(ctx, inbox, inner.Channel, inner.TimeStamp, inner.ThreadTimeStamp, inner.User, inner.Text, inner.ChannelType == "im")
		}
	case socketmode.EventTypeInteractive:
		cb, ok := evt.Data.(slack.InteractionCallback)
		if !ok {
			return
		}
		if evt.Request != nil {
			c.sm.Ack(*evt.Request)
		}
		if cb.Type != slack.InteractionTypeBlockActions {
			return
		}
		for _, a := range cb.ActionCallback.BlockActions {
			if !strings.HasPrefix(a.ActionID, "decision:") {
				continue
			}
			if !c.allowed(cb.User.ID) {
				continue
			}
			promptID := a.BlockID
			choice := a.Value
			thread := threadID(cb.Channel.ID, cb.Message.ThreadTimestamp, cb.Message.Timestamp)
			// Replace the buttons with the outcome so they cannot be clicked twice.
			_, _, _, err := c.api.UpdateMessageContext(ctx, cb.Channel.ID, cb.Message.Timestamp,
				slack.MsgOptionText(fmt.Sprintf("%s\n→ *%s* by <@%s>", firstText(cb.Message), choice, cb.User.ID), false),
				slack.MsgOptionBlocks())
			if err != nil {
				c.log.Warn("slack update message", "err", err)
			}
			in := transport.Inbound{Transport: "slack", Thread: thread, UserID: cb.User.ID, Decision: &transport.Decision{PromptID: promptID, Choice: choice}}
			select {
			case inbox <- in:
			case <-ctx.Done():
			}
		}
	}
}

func (c *Transport) deliver(ctx context.Context, inbox chan<- transport.Inbound, ch, ts, threadTS, user, text string, direct bool) {
	if !c.allowed(user) {
		c.log.Warn("slack message from unauthorized user ignored", "user", user)
		return
	}
	th := threadID(ch, threadTS, ts)
	if !direct && !c.known(th) {
		return // unrelated chatter in a channel we are in
	}
	c.remember(th)
	in := transport.Inbound{Transport: "slack", Thread: th, UserID: user, Text: stripMention(text, c.botUserID)}
	select {
	case inbox <- in:
	case <-ctx.Done():
	}
}

func (c *Transport) Send(ctx context.Context, msg transport.Outbound) error {
	chID, ts, ok := strings.Cut(string(msg.Thread), "/")
	if !ok {
		return fmt.Errorf("slack: bad thread id %q", msg.Thread)
	}
	c.remember(msg.Thread)
	opts := []slack.MsgOption{slack.MsgOptionTS(ts)}
	if msg.Prompt != nil {
		var buttons []slack.BlockElement
		for i, o := range promptOptions(msg.Prompt) {
			btn := slack.NewButtonBlockElement(fmt.Sprintf("decision:%d", i), o.Value, slack.NewTextBlockObject(slack.PlainTextType, truncate(o.Label, 75), false, false))
			switch o.Value {
			case "allow":
				btn.Style = slack.StylePrimary
			case "deny":
				btn.Style = slack.StyleDanger
			}
			buttons = append(buttons, btn)
		}
		opts = append(opts,
			slack.MsgOptionText(msg.Text, false),
			slack.MsgOptionBlocks(
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, msg.Text, false, false), nil, nil),
				slack.NewActionBlock(msg.Prompt.ID, buttons...),
			))
		_, _, err := c.api.PostMessageContext(ctx, chID, opts...)
		return err
	}
	for _, chunk := range chunks(msg.Text, 3900) {
		if _, _, err := c.api.PostMessageContext(ctx, chID, append(opts, slack.MsgOptionText(chunk, false))...); err != nil {
			return err
		}
	}
	return nil
}

// promptOptions normalizes a prompt into labelled options.
func promptOptions(p *transport.Prompt) []transport.Option {
	if len(p.Options) > 0 {
		return p.Options
	}
	out := make([]transport.Option, 0, len(p.Choices))
	for _, ch := range p.Choices {
		out = append(out, transport.Option{Value: ch, Label: strings.ToUpper(ch[:1]) + ch[1:]})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func (c *Transport) allowed(user string) bool {
	return len(c.allowedUsers) == 0 || c.allowedUsers[user]
}

// Remember marks a thread as one the bot takes part in, so plain replies
// there are forwarded. Implements transport.ThreadTracker.
func (c *Transport) Remember(th transport.ThreadID) { c.remember(th) }

func (c *Transport) known(th transport.ThreadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threads[th]
}

func (c *Transport) remember(th transport.ThreadID) {
	c.mu.Lock()
	c.threads[th] = true
	c.mu.Unlock()
}

func threadID(ch, threadTS, ts string) transport.ThreadID {
	if threadTS == "" {
		threadTS = ts
	}
	return transport.ThreadID(ch + "/" + threadTS)
}

var mentionRE = regexp.MustCompile(`<@[A-Z0-9]+>`)

func stripMention(text, botID string) string {
	text = strings.ReplaceAll(text, "<@"+botID+">", "")
	return strings.TrimSpace(text)
}

func firstText(m slack.Message) string {
	for _, b := range m.Blocks.BlockSet {
		if s, ok := b.(*slack.SectionBlock); ok && s.Text != nil {
			return s.Text.Text
		}
	}
	return m.Text
}

func chunks(s string, n int) []string {
	if len(s) <= n {
		return []string{s}
	}
	var out []string
	for len(s) > n {
		cut := strings.LastIndex(s[:n], "\n")
		if cut < n/2 {
			cut = n
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

var _ = mentionRE
