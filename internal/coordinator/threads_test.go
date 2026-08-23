package coordinator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
	envlocal "github.com/cleanunicorn/dancer/internal/environment/local"
	execlocal "github.com/cleanunicorn/dancer/internal/executor/local"
	"github.com/cleanunicorn/dancer/internal/store/sqlite"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/surface/chat"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// observerTransport is a fakeTransport that follows every thread (the web
// UI) and owns channels it can open threads in.
type observerTransport struct {
	fakeTransport
	channels []string
	opened   []transport.ThreadID
}

func (o *observerTransport) ObservesAllThreads() {}
func (o *observerTransport) Channels() []transport.Channel {
	var out []transport.Channel
	for _, c := range o.channels {
		out = append(out, transport.Channel{ID: c, Name: c})
	}
	return out
}
func (o *observerTransport) OpenThread(ctx context.Context, channel string, msg transport.Outbound) (transport.ThreadID, error) {
	th := transport.ThreadID(channel + "/t" + string(rune('1'+len(o.opened))))
	o.mu.Lock()
	o.opened = append(o.opened, th)
	msg.Thread = th
	o.out = append(o.out, msg)
	o.mu.Unlock()
	return th, nil
}

// hostTransport is a fakeTransport that owns channels and opens threads
// in them (Slack).
type hostTransport struct {
	fakeTransport
	channels []string
}

func (h *hostTransport) Channels() []transport.Channel {
	var out []transport.Channel
	for _, c := range h.channels {
		out = append(out, transport.Channel{ID: c, Name: c})
	}
	return out
}
func (h *hostTransport) OpenThread(ctx context.Context, channel string, msg transport.Outbound) (transport.ThreadID, error) {
	th := transport.ThreadID(channel + "/9.9")
	msg.Thread = th
	h.mu.Lock()
	h.out = append(h.out, msg)
	h.mu.Unlock()
	return th, nil
}

func (f *fakeTransport) count(th transport.ThreadID, sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.out {
		if o.Thread == th && strings.Contains(o.Text, sub) {
			n++
		}
	}
	return n
}

// waitFrom returns the relayed message (Outbound.From) on th written
// via a transport that says what (its text, or a decision's choice).
func (f *fakeTransport) waitFrom(t *testing.T, th transport.ThreadID, via, what string) transport.Outbound {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, o := range f.out {
			if o.Thread == th && o.From != nil && o.From.Via == via && (o.Text == what || (o.Decision != nil && o.Decision.Choice == what)) {
				f.mu.Unlock()
				return o
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no relayed message %q via %s on %s", what, via, th)
	return transport.Outbound{}
}

// TestSharedThreads: a task started in Slack is shown to the web observer
// as it happens, the web user answers its prompt and follows up, and what
// they wrote is relayed to Slack; a web user can also open a thread in a
// Slack channel, which then lives in Slack. Slack never sees web-only
// threads, and the log holds one copy of everything.
func TestSharedThreads(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	slack := &hostTransport{fakeTransport: fakeTransport{name: "slack", ready: make(chan struct{})}, channels: []string{"C-dev"}}
	web := &observerTransport{fakeTransport: fakeTransport{name: "web", ready: make(chan struct{})}, channels: []string{"general"}}
	c := New(st, ex, []transport.Transport{slack, web}, []surface.Surface{chat.New("chat-slack", "slack", false), chat.New("chat-web", "web", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	go c.Run(ctx)
	<-slack.ready
	<-web.ready

	// A Slack user starts a task. The web sees the start line and the
	// prompt (rendered once, by the Slack chat surface) and the human's
	// own words relayed; Slack does not get its own words back.
	th := transport.ThreadID("C-dev/1.0")
	slack.inbox <- transport.Inbound{Transport: "slack", Thread: th, UserID: "U1", UserName: "ana", Text: "run coder do the thing"}
	slack.waitFor(t, th, "started with agent *coder*")
	web.waitFor(t, th, "started with agent *coder*")
	if o := web.waitFrom(t, th, "slack", "run coder do the thing"); o.From.Name != "ana" {
		t.Errorf("relayed to web: %+v", o)
	}
	if n := slack.count(th, "run coder do the thing"); n != 0 {
		t.Errorf("slack got its own words back %d times", n)
	}
	sp := slack.waitFor(t, th, "wants to run")
	wp := web.waitFor(t, th, "wants to run")
	if sp.Prompt == nil || wp.Prompt == nil || sp.Prompt.ID != wp.Prompt.ID || !strings.HasPrefix(wp.Prompt.ID, "chat-slack:") {
		t.Fatalf("prompts differ: slack %+v web %+v", sp.Prompt, wp.Prompt)
	}
	if slack.count(th, "wants to run") != 1 || web.count(th, "wants to run") != 1 {
		t.Errorf("prompt rendered more than once: slack %d web %d", slack.count(th, "wants to run"), web.count(th, "wants to run"))
	}

	// The web user answers the Slack surface's prompt: the decision
	// reaches the agent, and both transports see who decided.
	web.inbox <- transport.Inbound{Transport: "web", Thread: th, UserID: "dan", UserName: "dan", Decision: &transport.Decision{PromptID: wp.Prompt.ID, Choice: "allow"}}
	slack.waitFor(t, th, "✅ done")
	web.waitFor(t, th, "✅ done")
	if o := slack.waitFrom(t, th, "web", "allow"); o.From.Name != "dan" || o.Text != "" {
		t.Errorf("decision relayed to slack: %+v", o)
	}
	web.waitFrom(t, th, "web", "allow")

	// A follow-up from the web is relayed to Slack and drives the task.
	web.inbox <- transport.Inbound{Transport: "web", Thread: th, UserID: "dan", UserName: "dan", Text: "again"}
	slack.waitFrom(t, th, "web", "again")
	slack.waitFor(t, th, "echo:again")
	web.waitFor(t, th, "echo:again")

	// The log has one inbound and one outbound per message: the relays
	// are derived, and History hands them back as From entries.
	msgs, err := c.Messages(ctx, th, 100)
	if err != nil {
		t.Fatal(err)
	}
	var froms, prompts, decisions int
	for _, e := range msgs {
		m := e.Message
		if m.From != nil {
			froms++
		}
		if m.Prompt != nil {
			prompts++
		}
		if m.Decision != nil {
			decisions++
		}
	}
	if froms != 3 || prompts != 1 || decisions != 1 {
		t.Errorf("history: %d from, %d prompts, %d decisions; %+v", froms, prompts, decisions, msgs)
	}
	infos, err := c.Threads(ctx)
	if err != nil || len(infos) != 1 || infos[0].ID != th || infos[0].Transport != "slack" || infos[0].Title != "run coder do the thing" || infos[0].Requester != "U1" {
		t.Errorf("threads: %+v err=%v", infos, err)
	}
	if chans := c.Channels(); len(chans["slack"]) != 1 || len(chans["web"]) != 1 {
		t.Errorf("channels: %+v", chans)
	}

	// A web user starts a thread in a Slack channel: Slack opens it with
	// their words as the root, the task is Slack's, the web follows.
	web.inbox <- transport.Inbound{Transport: "web", Thread: "C-dev/", UserID: "dan", UserName: "dan", Text: "run coder more"}
	opened := transport.ThreadID("C-dev/9.9")
	slack.waitFrom(t, opened, "web", "run coder more")
	slack.waitFor(t, opened, "started with agent *coder*")
	web.waitFor(t, opened, "started with agent *coder*")
	web.waitFrom(t, opened, "web", "run coder more")
	if slack.count(opened, "run coder more") != 1 {
		t.Errorf("slack got the root %d times", slack.count(opened, "run coder more"))
	}
	ts, err := st.LatestTaskForThread(ctx, opened)
	if err != nil || ts.Transport != "slack" || ts.Requester != "dan" {
		t.Errorf("task on the opened thread: %+v err=%v", ts, err)
	}
	// its prompt goes out with a plain name, not a Slack mention; the web
	// user answers it from the web
	p := web.waitFor(t, opened, "wants to run")
	if p.Mention != "dan" {
		t.Errorf("mention %q, want dan", p.Mention)
	}
	web.inbox <- transport.Inbound{Transport: "web", Thread: opened, UserID: "dan", UserName: "dan", Decision: &transport.Decision{PromptID: p.Prompt.ID, Choice: "allow"}}
	slack.waitFor(t, opened, "✅ done")

	// A thread in the web's own channel: opened by the web, never shown
	// to Slack.
	web.inbox <- transport.Inbound{Transport: "web", Thread: "general/", UserID: "dan", UserName: "dan", Text: "run coder private"}
	own := transport.ThreadID("general/t1")
	web.waitFor(t, own, "started with agent *coder*")
	wp = web.waitFor(t, own, "wants to run")
	if !strings.HasPrefix(wp.Prompt.ID, "chat-web:") {
		t.Errorf("web thread prompt rendered by %q", wp.Prompt.ID)
	}
	web.inbox <- transport.Inbound{Transport: "web", Thread: own, UserID: "dan", UserName: "dan", Decision: &transport.Decision{PromptID: wp.Prompt.ID, Choice: "allow"}}
	web.waitFor(t, own, "✅ done")
	slack.mu.Lock()
	for _, o := range slack.out {
		if o.Thread == own {
			t.Errorf("slack was sent a web-only thread message: %+v", o)
		}
	}
	slack.mu.Unlock()
	if web.count(own, "run coder private") != 1 {
		t.Errorf("web saw its own root %d times", web.count(own, "run coder private"))
	}
	if ts, err := st.LatestTaskForThread(ctx, own); err != nil || ts.Transport != "web" {
		t.Errorf("web task: %+v err=%v", ts, err)
	}
}
