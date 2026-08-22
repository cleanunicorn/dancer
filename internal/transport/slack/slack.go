// Package slack is a Slack transport over Socket Mode (no public URL needed).
//
// Thread mapping: ThreadID is "<channel>/<thread_ts>". A mention or DM at top level
// starts a thread under that message; replies in a thread the bot is
// active in are forwarded as follow-ups. Permission prompts are rendered
// as Block Kit buttons.
//
// Keyed messages (Outbound.Key) are edited in place with chat.update and
// removed with chat.delete, so a surface can keep one live status line per
// task instead of posting a new message for every change. The same text is
// mirrored into the thread's assistant status ("dancer ⏳ thinking · 4s"
// above the composer) with assistant.threads.setStatus, which works once
// the app has the Agents & AI Apps feature and the assistant:write scope;
// without them the first failure turns the mirror off for the process.
// Agent text (Outbound.Markdown) goes out as a Block Kit "markdown" block,
// which renders standard Markdown; Slack's own mrkdwn would show **bold**
// and # headings literally.
package slack

import (
	"bytes"
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
	keyed   map[string]string           // "<thread>\x00<key>" -> ts of the message posted under that key
	// noAssistant is set after assistant.threads.setStatus failed: the
	// app lacks the feature or the scope, and every keyed update would
	// fail the same way.
	noAssistant bool
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
		keyed:        map[string]string{},
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
			if choice == "" {
				choice = a.SelectedOption.Value // static_select
			}
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
		// Unrelated chatter in a channel we are in. Logged because it is
		// also what a second dancer instance sees for every thread the
		// other instance owns — a silent drop that looks like the bot
		// going deaf mid-conversation.
		c.log.Debug("slack message in an unknown thread ignored", "thread", th)
		return
	}
	if direct {
		c.follow(th) // a direct message reopens a thread that was closed
	} else {
		c.remember(th)
	}
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
	if msg.Key != "" {
		return c.sendKeyed(ctx, chID, opts, msg)
	}
	text := address(msg.Text, msg.Mention)
	if msg.Prompt != nil && len(promptOptions(msg.Prompt)) > 0 {
		opts = append(opts,
			slack.MsgOptionText(text, false),
			slack.MsgOptionBlocks(
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
				slack.NewActionBlock(msg.Prompt.ID, promptElements(msg.Prompt)...),
			))
		_, _, err := c.api.PostMessageContext(ctx, chID, opts...)
		return err
	}
	for _, chunk := range chunks(text, 3900) {
		if chunk == "" {
			continue
		}
		if _, _, err := c.api.PostMessageContext(ctx, chID, append(opts, textOptions(chunk, msg.Markdown)...)...); err != nil {
			if !msg.Markdown {
				return err
			}
			// A workspace or client that rejects the markdown block still
			// gets the text, just with Markdown syntax showing through.
			c.log.Warn("slack markdown block rejected, posting plain text", "err", err)
			if _, _, err := c.api.PostMessageContext(ctx, chID, append(opts, slack.MsgOptionText(chunk, false))...); err != nil {
				return err
			}
		}
	}
	for _, f := range msg.Files {
		_, err := c.api.UploadFileContext(ctx, slack.UploadFileParameters{
			Reader: bytes.NewReader(f.Data), FileSize: len(f.Data), Filename: f.Name, Title: f.Name,
			Channel: chID, ThreadTimestamp: ts,
		})
		if err != nil {
			c.log.Warn("slack file upload failed (needs files:write scope?)", "file", f.Name, "err", err)
			if _, _, perr := c.api.PostMessageContext(ctx, chID, append(opts, slack.MsgOptionText(fmt.Sprintf("⚠️ could not upload `%s`: %v", f.Name, err), false))...); perr != nil {
				return perr
			}
		}
	}
	return nil
}

// sendKeyed posts, edits or removes the message filed under msg.Key on
// the thread. The status line it carries is short by construction, so it
// is never chunked; an over-long text is cut to Slack's limit instead.
func (c *Transport) sendKeyed(ctx context.Context, chID string, opts []slack.MsgOption, msg transport.Outbound) error {
	k := string(msg.Thread) + "\x00" + msg.Key
	c.mu.Lock()
	ts, have := c.keyed[k]
	c.mu.Unlock()
	text := truncate(msg.Text, 3900)
	switch {
	case text == "" && !have:
		return nil
	case text == "":
		c.mu.Lock()
		delete(c.keyed, k)
		c.mu.Unlock()
		c.setStatus(ctx, chID, threadTS(msg.Thread), "")
		if _, _, err := c.api.DeleteMessageContext(ctx, chID, ts); err != nil {
			// Already gone (deleted by hand, or a restart in between) is
			// the outcome we wanted anyway.
			if !strings.Contains(err.Error(), "message_not_found") {
				return err
			}
		}
		return nil
	case have:
		_, _, _, err := c.api.UpdateMessageContext(ctx, chID, ts, textOptions(text, msg.Markdown)...)
		if err == nil {
			c.setStatus(ctx, chID, threadTS(msg.Thread), text)
			return nil
		}
		if !strings.Contains(err.Error(), "message_not_found") {
			return err
		}
		// Someone deleted it: post a fresh one below.
	}
	_, newTS, err := c.api.PostMessageContext(ctx, chID, append(opts, textOptions(text, msg.Markdown)...)...)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.keyed[k] = newTS
	c.mu.Unlock()
	// After the post: Slack clears the status whenever the app posts in
	// the thread, so it has to be set again each time.
	c.setStatus(ctx, chID, threadTS(msg.Thread), text)
	return nil
}

// setStatus mirrors the live line into the thread's assistant status, or
// clears it with an empty text. Best effort: the first failure (no Agents
// & AI Apps feature, no assistant:write scope, a thread Slack will not
// show a status in) switches the mirror off instead of failing every
// update the same way.
func (c *Transport) setStatus(ctx context.Context, chID, ts, text string) {
	c.mu.Lock()
	off := c.noAssistant
	c.mu.Unlock()
	if off || ts == "" {
		return
	}
	err := c.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{ChannelID: chID, ThreadTS: ts, Status: text})
	if err == nil {
		return
	}
	c.mu.Lock()
	c.noAssistant = true
	c.mu.Unlock()
	c.log.Info("slack assistant status unavailable; the status line in the thread still works (enable Agents & AI Apps and the assistant:write scope to get it)", "err", err)
}

// address puts a mention of user in front of text, so Slack notifies
// them even with the thread muted. The mention is ordinary mrkdwn, which
// the markdown block for agent text does not render, so surfaces only set
// it on dancer's own lines.
func address(text, user string) string {
	if user == "" {
		return text
	}
	return "<@" + user + "> " + text
}

// threadTS is the root message ts of a thread id, "" at top level.
func threadTS(th transport.ThreadID) string {
	_, ts, _ := strings.Cut(string(th), "/")
	return ts
}

// textOptions renders text either as Slack mrkdwn (our own lines) or as a
// markdown block (agent text), with the raw text as notification fallback.
func textOptions(text string, markdown bool) []slack.MsgOption {
	if !markdown {
		return []slack.MsgOption{slack.MsgOptionText(text, false)}
	}
	return []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(slack.NewMarkdownBlock("", text)),
	}
}

// selectThreshold is the option count from which a prompt is rendered as a
// searchable select menu instead of a row of buttons.
const selectThreshold = 5

// promptElements renders a prompt's options as buttons, or as a single
// static select (with Slack's type-to-filter) when there are many.
func promptElements(p *transport.Prompt) []slack.BlockElement {
	options := promptOptions(p)
	if len(options) >= selectThreshold {
		var items []*slack.OptionBlockObject
		for _, o := range options {
			var desc *slack.TextBlockObject
			if o.Description != "" {
				desc = slack.NewTextBlockObject(slack.PlainTextType, truncate(o.Description, 75), false, false)
			}
			items = append(items, slack.NewOptionBlockObject(o.Value, slack.NewTextBlockObject(slack.PlainTextType, truncate(o.Label, 75), false, false), desc))
		}
		placeholder := slack.NewTextBlockObject(slack.PlainTextType, "Choose…", false, false)
		return []slack.BlockElement{slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, placeholder, "decision:select", items...)}
	}
	var buttons []slack.BlockElement
	for i, o := range options {
		btn := slack.NewButtonBlockElement(fmt.Sprintf("decision:%d", i), o.Value, slack.NewTextBlockObject(slack.PlainTextType, truncate(o.Label, 75), false, false))
		switch o.Value {
		case "allow":
			btn.Style = slack.StylePrimary
		case "deny":
			btn.Style = slack.StyleDanger
		}
		buttons = append(buttons, btn)
	}
	return buttons
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

// Forget stops following a thread. Implements transport.ThreadCloser.
//
// It leaves a tombstone rather than deleting the entry, because posting
// into a thread remembers it again and a closed task still emits its last
// events ("cancelled", a final result) after the close notice. Only
// follow, i.e. a human addressing the bot in the thread, lifts it.
func (c *Transport) Forget(th transport.ThreadID) {
	c.mu.Lock()
	c.threads[th] = false
	c.mu.Unlock()
}

// React adds an emoji reaction to the thread's root message.
// Implements transport.Reactor.
func (c *Transport) React(ctx context.Context, th transport.ThreadID, emoji string) error {
	ch, ts, ok := strings.Cut(string(th), "/")
	if !ok || ts == "" {
		return fmt.Errorf("slack: cannot react on thread %q", th)
	}
	err := c.api.AddReactionContext(ctx, emoji, slack.ItemRef{Channel: ch, Timestamp: ts})
	if err != nil && strings.Contains(err.Error(), "already_reacted") {
		return nil
	}
	return err
}

// Unreact removes one of the bot's reactions from the thread's root
// message. Implements transport.Reactor. A reaction that is not there
// (removed by hand, or added before a restart by a previous process) is
// not an error.
func (c *Transport) Unreact(ctx context.Context, th transport.ThreadID, emoji string) error {
	ch, ts, ok := strings.Cut(string(th), "/")
	if !ok || ts == "" {
		return fmt.Errorf("slack: cannot unreact on thread %q", th)
	}
	err := c.api.RemoveReactionContext(ctx, emoji, slack.ItemRef{Channel: ch, Timestamp: ts})
	if err != nil && strings.Contains(err.Error(), "no_reaction") {
		return nil
	}
	return err
}

func (c *Transport) known(th transport.ThreadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threads[th]
}

// remember starts following a thread, unless Forget tombstoned it.
func (c *Transport) remember(th transport.ThreadID) {
	c.mu.Lock()
	if _, seen := c.threads[th]; !seen {
		c.threads[th] = true
	}
	c.mu.Unlock()
}

// follow starts following a thread even after Forget: a human talking to
// the bot in a closed thread reopens it.
func (c *Transport) follow(th transport.ThreadID) {
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
