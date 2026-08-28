// Package web is a browser transport: an HTTP server that serves a
// single-page chat UI and streams it every message over server-sent
// events. The UI is a React app on HeroUI (ui/: Vite, TypeScript,
// Tailwind; react-markdown renders the agent's Markdown) — the one place
// in dispatch with a JavaScript toolchain. It is drawn as a flight-strip
// board: every thread a paper strip in a rack, flagged and lit by state,
// the open one pulled across the head of the desk (ui/src/styles.css
// carries the materials, DESIGN.md the system). Its build is committed under
// static/ and embedded, so `go build` needs no Node; `make ui` rebuilds
// it after a change in ui/. It is a transport.Observer — it shows every conversation dispatch
// has, whichever transport hosts it — and runs next to Slack on the same
// coordinator, so one dispatch serves both.
//
// It keeps nothing a reload could lose: the channel, agent and thread
// lists and each thread's messages — the agent's tool calls among them —
// are read from the coordinator's log (transport.History) whenever a page
// asks, and what arrives live is pushed to open pages as it comes. The
// only memory is of the moment — the status line of a running turn (a
// keyed message), the prompt that is open on a thread (so the sidebar
// can offer its choices without opening it), and threads opened here
// that have no task yet. The page re-reads the thread list after every
// line from dispatch or the agent, and a thread's history when its status
// line comes down, so the facts it shows (status, closed, model, session,
// tool calls) follow the task.
//
// Channels of its own: Channels (config: web.channels) are places to
// start a thread when there is no Slack, or for work nobody needs in
// Slack. A thread is "<channel>/<id>" with an id minted in OpenThread; a
// thread in a Slack channel is opened by the Slack transport on the
// coordinator's request and shows up here like any other.
//
// Identity is an account (auth.go): the UI is a login page until a
// session cookie names a user, and that name is the Inbound.UserID and
// UserName of everything they send — so a mention (Outbound.Mention) of
// it lights up for them and nobody else. Accounts come from
// `dispatch user add`; there is no anonymous mode. The server speaks plain
// HTTP: listen on the loopback interface (the default) or put TLS in
// front of it.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dispatch/internal/transport"
)

//go:embed static
var static embed.FS

// Name is the transport name reported in Inbound.Transport.
const Name = "web"

// Transport implements transport.Transport over HTTP.
type Transport struct {
	// Listen is the address the HTTP server binds ("127.0.0.1:8788").
	Listen string
	// Users is where accounts and sessions live (the store).
	Users Users
	// OwnChannels are this transport's own channels, by id.
	OwnChannels []string
	// History is where the lists and the past come from (the
	// coordinator). Nil shows only what arrives live.
	History transport.History
	// Listener, when set, is used instead of Listen (tests).
	Listener net.Listener

	log   *slog.Logger
	hub   *hub
	inbox chan<- transport.Inbound
	ready chan struct{} // closed once Run has the inbox
	now   func() time.Time

	mu      sync.Mutex
	seq     int64                                      // ids of live messages; history ids count from 1 per fetch
	live    map[transport.ThreadID]map[string]*Message // keyed messages up right now, per thread and key
	open    map[transport.ThreadID]*Message            // the prompt waiting on the thread, if one is
	opened  map[transport.ThreadID]*Thread             // threads opened here that History may not list yet
	titles  map[transport.ThreadID]string              // first human line of threads opened here
	counter int
}

// New returns a web transport.
func New(listen string, channels []string, users Users, log *slog.Logger) *Transport {
	if log == nil {
		log = slog.Default()
	}
	return &Transport{
		Listen: listen, OwnChannels: channels, Users: users, log: log,
		hub: newHub(), ready: make(chan struct{}), now: time.Now,
		live:   map[transport.ThreadID]map[string]*Message{},
		open:   map[transport.ThreadID]*Message{},
		opened: map[transport.ThreadID]*Thread{},
		titles: map[transport.ThreadID]string{},
	}
}

func (t *Transport) Name() string { return Name }

// ObservesAllThreads marks the transport as one that shows every
// conversation. Implements transport.Observer.
func (t *Transport) ObservesAllThreads() {}

// Channels implements transport.ChannelLister: the transport's own
// channels.
func (t *Transport) Channels() []transport.Channel {
	out := make([]transport.Channel, 0, len(t.OwnChannels))
	for _, id := range t.OwnChannels {
		out = append(out, transport.Channel{ID: id, Name: id})
	}
	return out
}

// OpenThread starts a thread in one of the transport's own channels,
// with msg as its root. Implements transport.ThreadOpener.
func (t *Transport) OpenThread(ctx context.Context, channel string, msg transport.Outbound) (transport.ThreadID, error) {
	if !t.owns(channel) {
		return "", fmt.Errorf("web: no channel %q", channel)
	}
	t.mu.Lock()
	now := t.now()
	t.counter++
	id := transport.ThreadID(channel + "/" + strconv.FormatInt(now.UnixMilli(), 36) + "-" + strconv.Itoa(t.counter))
	th := &Thread{ID: id, Transport: Name, Channel: channel, Title: firstLine(msg.Text), Updated: now}
	if msg.From != nil {
		th.Requester = msg.From.ID
	}
	t.opened[id] = th
	t.titles[id] = th.Title
	msg.Thread = id
	m := t.message(msg, now)
	t.mu.Unlock()
	t.hub.publish(event{Type: "thread", Thread: th})
	t.hub.publish(event{Type: "message", Message: m})
	return id, nil
}

func (t *Transport) owns(channel string) bool {
	for _, id := range t.OwnChannels {
		if id == channel {
			return true
		}
	}
	return false
}

// Run serves until ctx is cancelled.
func (t *Transport) Run(ctx context.Context, inbox chan<- transport.Inbound) error {
	t.inbox = inbox
	close(t.ready)
	ln := t.Listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", t.Listen)
		if err != nil {
			return fmt.Errorf("web: listen: %w", err)
		}
	}
	srv := &http.Server{Handler: t.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	t.log.Info("web ui listening", "url", "http://"+ln.Addr().String())
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		t.hub.closeAll()
		_ = srv.Shutdown(sctx)
		return ctx.Err()
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return ctx.Err()
		}
		return fmt.Errorf("web: serve: %w", err)
	}
}

// Send pushes a message to every open page. Keyed messages are kept as
// the thread's live line until removed; prompts and decisions move the
// thread's waiting flag.
func (t *Transport) Send(ctx context.Context, msg transport.Outbound) error {
	if msg.Thread == "" {
		return errors.New("web: message without a thread")
	}
	t.mu.Lock()
	now := t.now()
	if msg.Key != "" {
		byKey := t.live[msg.Thread]
		cur := byKey[msg.Key]
		switch {
		case msg.Text == "" && cur == nil:
			t.mu.Unlock()
			return nil
		case msg.Text == "":
			delete(byKey, msg.Key)
			t.mu.Unlock()
			t.hub.publish(event{Type: "remove", ThreadID: msg.Thread, ID: cur.ID})
			return nil
		case cur != nil:
			cur.Text, cur.At = msg.Text, now
			cp := *cur // published outside the lock; the next edit must not race it
			t.mu.Unlock()
			t.hub.publish(event{Type: "edit", Message: &cp})
			return nil
		}
		if byKey == nil {
			byKey = map[string]*Message{}
			t.live[msg.Thread] = byKey
		}
		cur = t.message(msg, now)
		byKey[msg.Key] = cur
		cp := *cur
		t.mu.Unlock()
		t.hub.publish(event{Type: "edit", Message: &cp})
		return nil
	}
	if msg.Text == "" && msg.Prompt == nil && len(msg.Files) == 0 && msg.Decision == nil {
		t.mu.Unlock()
		return nil
	}
	m := t.message(msg, now)
	switch {
	case msg.Prompt != nil:
		t.open[msg.Thread] = m
	case msg.Decision != nil, msg.From == nil:
		delete(t.open, msg.Thread)
	}
	if th := t.opened[msg.Thread]; th != nil {
		th.Updated = now
	}
	t.mu.Unlock()
	t.hub.publish(event{Type: "message", Message: m})
	return nil
}

// message converts an Outbound for the browser. Called with mu held.
func (t *Transport) message(msg transport.Outbound, at time.Time) *Message {
	t.seq++
	return convert(msg, liveBase+t.seq, at)
}

// liveBase keeps live ids clear of history ids, which count from 1 per
// fetch; a page dedupes by id within a thread.
const liveBase = int64(1) << 40

// convertTool is a tool call from the log for the browser.
func convertTool(th transport.ThreadID, tc *transport.ToolCall, id int64, at time.Time) *Message {
	return &Message{ID: id, Thread: th, At: at, Tool: &ToolCall{
		ID: tc.ID, Name: tc.Name, Input: tc.Input, Sub: tc.Sub, Done: tc.Done, Error: tc.Error, Denied: tc.Denied,
		Millis: tc.Duration.Milliseconds(),
	}}
}

func convert(msg transport.Outbound, id int64, at time.Time) *Message {
	m := &Message{ID: id, Thread: msg.Thread, At: at, Text: msg.Text, Markdown: msg.Markdown, Mention: msg.Mention, Key: msg.Key}
	if p := msg.Prompt; p != nil {
		m.Prompt = &Prompt{ID: p.ID, Choices: p.Choices, Question: p.Question, FreeText: p.FreeText}
		for _, o := range p.Options {
			m.Prompt.Options = append(m.Prompt.Options, Option{Value: o.Value, Label: o.Label, Description: o.Description})
		}
	}
	if d := msg.Decision; d != nil {
		m.Decision = &Decision{PromptID: d.PromptID, Choice: d.Choice}
	}
	if msg.From != nil {
		m.From = &Author{ID: msg.From.ID, Name: msg.From.Display(), Via: msg.From.Via}
	}
	for _, f := range msg.Files {
		// From the log a file is a name only (transport.File keeps Data
		// out of it); live, the bytes come along when they fit.
		ref := File{Name: f.Name, Size: len(f.Data)}
		if len(f.Data) > 0 && len(f.Data) <= maxInlineFile {
			ref.Data = f.Data
		}
		m.Files = append(m.Files, ref)
	}
	return m
}

// maxInlineFile bounds the attachments sent to the browser inline.
const maxInlineFile = 8 << 20

// Message is one line of a thread as the browser sees it.
type Message struct {
	ID       int64              `json:"id"`
	Thread   transport.ThreadID `json:"thread"`
	At       time.Time          `json:"at"`
	Text     string             `json:"text"`
	Markdown bool               `json:"markdown,omitempty"`
	Mention  string             `json:"mention,omitempty"`
	Key      string             `json:"key,omitempty"`
	Prompt   *Prompt            `json:"prompt,omitempty"`
	From     *Author            `json:"from,omitempty"`
	Decision *Decision          `json:"decision,omitempty"`
	Files    []File             `json:"files,omitempty"`
	Tool     *ToolCall          `json:"tool,omitempty"` // a tool the agent ran; nothing else is set
}

// ToolCall is transport.ToolCall for the browser.
type ToolCall struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Input  string `json:"input,omitempty"`
	Sub    bool   `json:"sub,omitempty"`
	Done   bool   `json:"done,omitempty"`
	Error  bool   `json:"error,omitempty"`
	Denied bool   `json:"denied,omitempty"`
	Millis int64  `json:"millis,omitempty"` // how long it ran
}

// Prompt is transport.Prompt for the browser.
type Prompt struct {
	ID       string   `json:"id"`
	Choices  []string `json:"choices,omitempty"`
	Question string   `json:"question,omitempty"`
	Options  []Option `json:"options,omitempty"`
	FreeText bool     `json:"freeText,omitempty"`
}

// Option is transport.Option for the browser.
type Option struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Decision is transport.Decision for the browser.
type Decision struct {
	PromptID string `json:"promptId"`
	Choice   string `json:"choice"`
}

// Author is who wrote a relayed message.
type Author struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Via  string `json:"via"`
}

// File is an attachment, inline when small enough.
type File struct {
	Name string `json:"name"`
	Size int    `json:"size"`
	Data []byte `json:"data,omitempty"` // base64 in JSON
}

// Thread is one conversation in the sidebar.
type Thread struct {
	ID        transport.ThreadID `json:"id"`
	Transport string             `json:"transport"`
	Channel   string             `json:"channel"`
	Title     string             `json:"title"`
	Status    string             `json:"status,omitempty"`
	Closed    bool               `json:"closed,omitempty"`
	Requester string             `json:"requester,omitempty"`
	Updated   time.Time          `json:"updated"`
	Agent     string             `json:"agent,omitempty"`
	Model     string             `json:"model,omitempty"`
	Env       string             `json:"env,omitempty"`
	Session   string             `json:"session,omitempty"`
	Live      string             `json:"live,omitempty"`    // the status line up right now
	Waiting   bool               `json:"waiting,omitempty"` // a prompt is open
	Ask       string             `json:"ask,omitempty"`     // what the open prompt says, when this transport saw it
	Prompt    *Prompt            `json:"prompt,omitempty"`  // the open prompt, when this transport saw it
	Mention   string             `json:"mention,omitempty"` // who the open prompt is for
}

// Agent is one definition a human can run.
type Agent struct {
	Name  string `json:"name"`
	Model string `json:"model,omitempty"`
	Env   string `json:"env,omitempty"`
}

// Channel is one place to start threads, with the transport it belongs to.
type Channel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
}

// Handler is the HTTP API and the static UI.
func (t *Transport) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("POST /api/login", sameSite(t.login))
	mux.HandleFunc("POST /api/logout", t.auth(sameSiteUser(t.logout)))
	mux.HandleFunc("GET /api/me", t.auth(t.me))
	mux.HandleFunc("POST /api/password", t.auth(sameSiteUser(t.password)))
	mux.HandleFunc("GET /api/state", t.auth(t.state))
	mux.HandleFunc("GET /api/events", t.auth(t.events))
	mux.HandleFunc("GET /api/threads/{channel}/{thread}", t.auth(t.thread))
	mux.HandleFunc("POST /api/messages", t.auth(sameSiteUser(t.post)))
	mux.HandleFunc("POST /api/decide", t.auth(sameSiteUser(t.decide)))
	return mux
}

// userHandler is an API handler that knows who is asking.
type userHandler func(w http.ResponseWriter, r *http.Request, user string)

func sameSiteUser(h userHandler) userHandler {
	return func(w http.ResponseWriter, r *http.Request, user string) {
		sameSite(func(w http.ResponseWriter, r *http.Request) { h(w, r, user) })(w, r)
	}
}

// sameSite refuses a POST another site made the browser send: the
// browser attaches the session cookie to any request, so a page the
// operator has open elsewhere could otherwise start a task here. The
// page's own requests are JSON and same-origin; a cross-site form can be
// neither.
func sameSite(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			jsonError(w, http.StatusUnsupportedMediaType, "JSON only")
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			jsonError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || !strings.EqualFold(u.Host, r.Host) {
				jsonError(w, http.StatusForbidden, "cross-site request refused")
				return
			}
		}
		h(w, r)
	}
}

// state is the sidebar: every channel and thread dispatch knows.
func (t *Transport) state(w http.ResponseWriter, r *http.Request, _ string) {
	var channels []Channel
	var threads []Thread
	agents := []Agent{}
	if t.History != nil {
		defs, err := t.History.Agents(r.Context())
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, d := range defs {
			agents = append(agents, Agent{Name: d.Name, Model: d.Model, Env: d.Environment})
		}
		for name, chs := range t.History.Channels() {
			for _, c := range chs {
				channels = append(channels, Channel{ID: c.ID, Name: c.Name, Transport: name})
			}
		}
		infos, err := t.History.Threads(r.Context())
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, in := range infos {
			threads = append(threads, Thread{ID: in.ID, Transport: in.Transport, Channel: in.Channel, Title: in.Title,
				Status: in.Status, Closed: in.Closed, Requester: in.Requester, Updated: in.Updated,
				Agent: in.Agent, Model: in.Model, Env: in.Environment, Session: in.Session})
		}
	} else {
		for _, c := range t.Channels() {
			channels = append(channels, Channel{ID: c.ID, Name: c.Name, Transport: Name})
		}
	}
	t.mu.Lock()
	listed := map[transport.ThreadID]bool{}
	for i := range threads {
		listed[threads[i].ID] = true
		t.decorate(&threads[i])
	}
	for id, th := range t.opened {
		if !listed[id] {
			cp := *th
			t.decorate(&cp)
			threads = append(threads, cp)
		}
	}
	t.mu.Unlock()
	sort.SliceStable(channels, func(i, j int) bool {
		if channels[i].Transport != channels[j].Transport {
			return channels[i].Transport < channels[j].Transport
		}
		return channels[i].Name < channels[j].Name
	})
	sort.SliceStable(threads, func(i, j int) bool { return threads[i].Updated.After(threads[j].Updated) })
	if channels == nil {
		channels = []Channel{}
	}
	if threads == nil {
		threads = []Thread{}
	}
	writeJSON(w, map[string]any{"transport": Name, "channels": channels, "threads": threads, "agents": agents})
}

// decorate adds what only the live transport knows. Called with mu held.
func (t *Transport) decorate(th *Thread) {
	if title, ok := t.titles[th.ID]; ok && th.Title == "" {
		th.Title = title
	}
	for _, m := range t.live[th.ID] {
		th.Live = m.Text
	}
	if p := t.open[th.ID]; p != nil {
		th.Ask, th.Prompt, th.Mention = gist(p.Text), p.Prompt, p.Mention
	}
	th.Waiting = t.open[th.ID] != nil || th.Status == "waiting_permission"
}

// thread is one thread's messages: the log, then the live line.
func (t *Transport) thread(w http.ResponseWriter, r *http.Request, _ string) {
	th := transport.ThreadID(r.PathValue("channel") + "/" + r.PathValue("thread"))
	msgs := []*Message{}
	if t.History != nil {
		hist, err := t.History.Messages(r.Context(), th, historyLimit)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for i, e := range hist {
			if e.Tool != nil {
				msgs = append(msgs, convertTool(th, e.Tool, int64(i+1), e.At))
				continue
			}
			msgs = append(msgs, convert(e.Message, int64(i+1), e.At))
		}
	}
	t.mu.Lock()
	for _, m := range t.live[th] {
		cp := *m
		msgs = append(msgs, &cp)
	}
	t.mu.Unlock()
	writeJSON(w, map[string]any{"thread": th, "messages": msgs})
}

// historyLimit is how many log records a page loads for a thread.
const historyLimit = 2000

// post is what the human typed: in a thread, or in a channel to start
// one (the coordinator opens it and the page hears about it live).
func (t *Transport) post(w http.ResponseWriter, r *http.Request, user string) {
	var body struct {
		Thread  string `json:"thread"`
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" {
		jsonError(w, http.StatusBadRequest, "empty message")
		return
	}
	th := transport.ThreadID(body.Thread)
	if th == "" {
		if body.Channel == "" || strings.Contains(body.Channel, "/") {
			jsonError(w, http.StatusBadRequest, "a thread or a channel is needed")
			return
		}
		th = transport.ThreadID(body.Channel + "/")
	}
	in := transport.Inbound{Transport: Name, Thread: th, UserID: user, UserName: user, Text: body.Text}
	if err := t.deliver(r.Context(), in); err != nil {
		jsonError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// decide answers a prompt.
func (t *Transport) decide(w http.ResponseWriter, r *http.Request, user string) {
	var body struct {
		Thread   string `json:"thread"`
		PromptID string `json:"promptId"`
		Choice   string `json:"choice"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Choice = strings.TrimSpace(body.Choice)
	if body.Thread == "" || body.PromptID == "" || body.Choice == "" {
		jsonError(w, http.StatusBadRequest, "thread, promptId and choice are needed")
		return
	}
	in := transport.Inbound{Transport: Name, Thread: transport.ThreadID(body.Thread), UserID: user, UserName: user,
		Decision: &transport.Decision{PromptID: body.PromptID, Choice: body.Choice}}
	if err := t.deliver(r.Context(), in); err != nil {
		jsonError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// deliver hands an inbound message to the coordinator.
func (t *Transport) deliver(ctx context.Context, in transport.Inbound) error {
	select {
	case <-t.ready:
	case <-ctx.Done():
		return errors.New("dispatch is not accepting messages")
	case <-time.After(5 * time.Second):
		return errors.New("dispatch is not accepting messages")
	}
	select {
	case t.inbox <- in:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("dispatch is busy, try again")
	}
}

// events streams every change as server-sent events.
func (t *Transport) events(w http.ResponseWriter, r *http.Request, _ string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := t.hub.subscribe()
	defer t.hub.unsubscribe(ch)
	hello, _ := json.Marshal(event{Type: "hello"})
	fmt.Fprintf(w, "data: %s\n\n", hello)
	fl.Flush()
	keep := time.NewTicker(20 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keep.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case b, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// event is one server-sent event.
type event struct {
	Type     string             `json:"type"` // hello, message, edit, remove, thread
	Message  *Message           `json:"message,omitempty"`
	ThreadID transport.ThreadID `json:"thread,omitempty"`
	ID       int64              `json:"id,omitempty"`
	Thread   *Thread            `json:"threadInfo,omitempty"`
}

// hub fans events out to every open event stream. A page that does not
// keep up is dropped and reconnects on its own.
type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newHub() *hub { return &hub{subs: map[chan []byte]struct{}{}} }

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *hub) publish(ev event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

// gist is a prompt's text as one short plain line: the markup a chat
// renders is dropped and the lines joined, so "Bash wants to run:
// `make test`" reads in a list.
func gist(s string) string {
	s = strings.NewReplacer("```", " ", "`", "", "*", "", "_", "").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:159]) + "…"
	}
	return s
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

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("bad request: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
