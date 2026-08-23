package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// A conversation belongs to dancer, not to a transport. The transport
// that minted its ThreadID hosts it — its task records that name
// (store.TaskState.Transport) — and renders it natively; every other
// transport that follows it (transport.Observer) is shown the same
// messages and may write into it. What a human writes is relayed to the
// host and the observers as transport.Outbound.From, so every transport
// showing the thread has the whole exchange, and the log keeps one
// record of it (the inbound), never the relays. History for an observer
// is read back from that log (transport.History, implemented below).

// hostOf names the transport hosting th: the transport of its task, else
// the owner of its channel, else "" (a thread nobody hosts, such as the
// terminal's).
func (c *Coordinator) hostOf(ctx context.Context, th transport.ThreadID) string {
	c.mu.Lock()
	host, ok := c.hosts[th]
	c.mu.Unlock()
	if ok {
		return host
	}
	if st, err := c.Store.LatestTaskForThread(ctx, th); err == nil && st.Transport != "" {
		host = st.Transport
	} else {
		host = c.channelOwner(channelOf(th))
	}
	if host != "" {
		c.setHost(th, host)
	}
	return host
}

func (c *Coordinator) setHost(th transport.ThreadID, host string) {
	c.mu.Lock()
	c.hosts[th] = host
	c.mu.Unlock()
}

// channelOwner names the transport whose channel ch is, "" when no
// transport claims it.
func (c *Coordinator) channelOwner(ch string) string {
	if ch == "" {
		return ""
	}
	for name, t := range c.transports {
		cl, ok := t.(transport.ChannelLister)
		if !ok {
			continue
		}
		for _, known := range cl.Channels() {
			if known.ID == ch {
				return name
			}
		}
	}
	return ""
}

// place resolves where an inbound message goes. A message to "<channel>/"
// asks for a new conversation in that channel: the transport owning the
// channel opens one with the message as its root, and the inbound carries
// on under the new id. It returns the transport that opened the thread
// (it has the message already, so the relay skips it), "" otherwise.
func (c *Coordinator) place(ctx context.Context, in *transport.Inbound) (openedOn string) {
	ch, rest, ok := strings.Cut(string(in.Thread), "/")
	if !ok || rest != "" || ch == "" || in.Text == "" {
		return ""
	}
	owner := c.channelOwner(ch)
	if owner == "" {
		return ""
	}
	opener, ok := c.transports[owner].(transport.ThreadOpener)
	if !ok {
		return ""
	}
	th, err := opener.OpenThread(ctx, ch, transport.Outbound{Text: in.Text, From: authorOf(*in)})
	if err != nil {
		c.Log.Error("open thread", "transport", owner, "channel", ch, "err", err)
		if t := c.transports[in.Transport]; t != nil {
			_ = t.Send(ctx, transport.Outbound{Thread: in.Thread, Text: fmt.Sprintf("❌ could not start a thread in %s channel %s: %v", owner, ch, err)})
		}
		return ""
	}
	c.Log.Info("opened thread", "transport", owner, "channel", ch, "thread", th, "for", in.Transport)
	in.Thread = th
	c.setHost(th, owner)
	if tt, ok := c.transports[owner].(transport.ThreadTracker); ok {
		tt.Remember(th)
	}
	return owner
}

// relay shows what a human wrote to the transports following the thread
// that did not see it typed: the host when the human wrote elsewhere,
// and every observer — including the one the human wrote on, which
// keeps no record of its own. openedOn is skipped: it posted the text
// as the thread's root.
func (c *Coordinator) relay(ctx context.Context, in transport.Inbound, openedOn string) {
	if in.Text == "" && in.Decision == nil {
		return
	}
	host := c.hostOf(ctx, in.Thread)
	msg := transport.Outbound{Thread: in.Thread, Text: in.Text, Decision: in.Decision, From: authorOf(in)}
	if in.Decision != nil {
		msg.Text = ""
	}
	mu := c.threadLock(in.Thread)
	mu.Lock()
	defer mu.Unlock()
	for name, t := range c.transports {
		if name == openedOn {
			continue
		}
		_, observer := t.(transport.Observer)
		if !observer && (name == in.Transport || name != host) {
			continue
		}
		if err := t.Send(ctx, msg); err != nil {
			c.Log.Warn("relay failed", "transport", name, "thread", in.Thread, "err", err)
		}
	}
}

func authorOf(in transport.Inbound) *transport.Author {
	return &transport.Author{ID: in.UserID, Name: in.UserName, Via: in.Transport}
}

// observersBesides lists the transports that follow every thread, except
// name (which just got the message as the thread's own transport).
func (c *Coordinator) observersBesides(name string) []transport.Transport {
	var out []transport.Transport
	for n, t := range c.transports {
		if n == name {
			continue
		}
		if _, ok := t.(transport.Observer); ok {
			out = append(out, t)
		}
	}
	return out
}

// Threads implements transport.History: one entry per conversation that
// ever had a task, newest first.
func (c *Coordinator) Threads(ctx context.Context) ([]transport.ThreadInfo, error) {
	tasks, err := c.Store.ListTasks(ctx, "")
	if err != nil {
		return nil, err
	}
	latest := map[transport.ThreadID]store.TaskState{}
	for _, t := range tasks {
		if cur, ok := latest[t.Thread]; !ok || t.UpdatedAt.After(cur.UpdatedAt) {
			latest[t.Thread] = t
		}
	}
	out := make([]transport.ThreadInfo, 0, len(latest))
	for th, t := range latest {
		out = append(out, transport.ThreadInfo{
			ID: th, Transport: t.Transport, Channel: channelOf(th),
			Title: c.title(ctx, th), Status: t.Status, Closed: c.threadClosed(th),
			Requester: t.Requester, Updated: t.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// title is the first thing a human wrote on th, remembered once found:
// the first record never changes.
func (c *Coordinator) title(ctx context.Context, th transport.ThreadID) string {
	c.mu.Lock()
	t, ok := c.titles[th]
	c.mu.Unlock()
	if ok {
		return t
	}
	recs, err := c.Store.ThreadHeadOfKind(ctx, th, "inbound", 1)
	if err != nil || len(recs) == 0 {
		return ""
	}
	var in transport.Inbound
	if json.Unmarshal(recs[0].Payload, &in) != nil || in.Text == "" {
		return ""
	}
	t = firstLine(in.Text)
	c.mu.Lock()
	c.titles[th] = t
	c.mu.Unlock()
	return t
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:119]) + "…"
	}
	return s
}

// Messages implements transport.History: the last limit records of th
// as the Outbounds a transport following it saw, oldest first. Inbound
// records come back as From entries (the relay form); status lines
// (keyed outbounds) are left out, they described a moment.
func (c *Coordinator) Messages(ctx context.Context, th transport.ThreadID, limit int) ([]transport.Entry, error) {
	recs, err := c.Store.ThreadRecords(ctx, th, limit)
	if err != nil {
		return nil, err
	}
	out := make([]transport.Entry, 0, len(recs))
	for _, r := range recs {
		switch r.Kind {
		case "inbound":
			var in transport.Inbound
			if json.Unmarshal(r.Payload, &in) != nil || (in.Text == "" && in.Decision == nil) {
				continue
			}
			msg := transport.Outbound{Thread: th, Text: in.Text, Decision: in.Decision, From: authorOf(in)}
			if in.Decision != nil {
				msg.Text = ""
			}
			out = append(out, transport.Entry{At: r.At, Message: msg})
		case "outbound":
			var msg transport.Outbound
			if json.Unmarshal(r.Payload, &msg) != nil || msg.Key != "" {
				continue
			}
			msg.Thread = th
			out = append(out, transport.Entry{At: r.At, Message: msg})
		}
	}
	return out, nil
}

// Channels implements the channel list an observer shows: every channel
// of every transport that has any, with the transport it belongs to.
func (c *Coordinator) Channels() map[string][]transport.Channel {
	out := map[string][]transport.Channel{}
	for name, t := range c.transports {
		if cl, ok := t.(transport.ChannelLister); ok {
			out[name] = cl.Channels()
		}
	}
	return out
}
