package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/transport"
)

// fakeHistory is the coordinator's read side, canned.
type fakeHistory struct {
	channels map[string][]transport.Channel
	threads  []transport.ThreadInfo
	messages map[transport.ThreadID][]transport.Outbound
}

func (f fakeHistory) Messages(_ context.Context, th transport.ThreadID, _ int) ([]transport.Entry, error) {
	var out []transport.Entry
	for _, m := range f.messages[th] {
		out = append(out, transport.Entry{At: time.Unix(1700000000, 0), Message: m})
	}
	return out, nil
}

func (f fakeHistory) Channels() map[string][]transport.Channel { return f.channels }
func (f fakeHistory) Threads(context.Context) ([]transport.ThreadInfo, error) {
	return f.threads, nil
}

// newTest runs a transport on a loopback port and returns it with a
// server handle for the URL and the inbox Run delivers to.
func newTest(t *testing.T, token string) (*Transport, *httptest.Server, chan transport.Inbound) {
	t.Helper()
	tr := New("", token, []string{"general"}, nil)
	tr.History = fakeHistory{
		channels: map[string][]transport.Channel{"web": tr.Channels(), "slack": {{ID: "C1", Name: "dev"}}},
		threads:  []transport.ThreadInfo{{ID: "C1/1.1", Transport: "slack", Channel: "C1", Title: "fix the build", Status: "running"}},
		messages: map[transport.ThreadID][]transport.Outbound{"C1/1.1": {
			{Thread: "C1/1.1", Text: "fix the build", From: &transport.Author{ID: "U1", Name: "ana", Via: "slack"}},
			{Thread: "C1/1.1", Text: "▶️ task started"},
			{Thread: "C1/1.1", Text: "🔐 wants to run", Prompt: &transport.Prompt{ID: "chat-slack:p1", Choices: []string{"allow", "deny"}}},
			{Thread: "C1/1.1", Decision: &transport.Decision{PromptID: "chat-slack:p1", Choice: "allow"}, From: &transport.Author{ID: "U1", Name: "ana", Via: "slack"}},
		}},
	}
	inbox := make(chan transport.Inbound, 8)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tr.Listener = ln
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = tr.Run(ctx, inbox) }()
	t.Cleanup(func() { cancel(); <-done })
	<-tr.ready
	return tr, &httptest.Server{URL: "http://" + ln.Addr().String()}, inbox
}

func post(t *testing.T, srv *httptest.Server, path string, body any, token string) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	return res, out
}

func get(t *testing.T, srv *httptest.Server, path, token string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	return res, out
}

// events opens the stream and returns a channel of decoded events.
func events(t *testing.T, srv *httptest.Server, token string) <-chan event {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	out := make(chan event, 32)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev event
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil {
				out <- ev
			}
		}
		close(out)
	}()
	if ev := <-out; ev.Type != "hello" {
		t.Fatalf("first event %q, want hello", ev.Type)
	}
	return out
}

func next(t *testing.T, ch <-chan event, typ string) event {
	t.Helper()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed waiting for %s", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %s event", typ)
		}
	}
}

// TestStateAndHistory: the sidebar and a thread's past come from History,
// decorated with what only the transport knows (a live line, an open
// prompt), and the transport's own channels are listed with the rest.
func TestStateAndHistory(t *testing.T) {
	tr, srv, _ := newTest(t, "")
	if err := tr.Send(context.Background(), transport.Outbound{Thread: "C1/1.1", Key: "status", Text: "⏳ thinking · 4s"}); err != nil {
		t.Fatal(err)
	}
	_, st := get(t, srv, "/api/state", "")
	chans := st["channels"].([]any)
	if len(chans) != 2 {
		t.Fatalf("channels: %v", chans)
	}
	threads := st["threads"].([]any)
	th := threads[0].(map[string]any)
	if th["id"] != "C1/1.1" || th["transport"] != "slack" || th["title"] != "fix the build" || th["live"] != "⏳ thinking · 4s" {
		t.Errorf("thread: %v", th)
	}
	_, msgs := get(t, srv, "/api/threads/C1/1.1", "")
	list := msgs["messages"].([]any)
	if len(list) != 5 { // 4 from history + the live line
		t.Fatalf("messages: %d", len(list))
	}
	first := list[0].(map[string]any)
	if from := first["from"].(map[string]any); from["name"] != "ana" || from["via"] != "slack" {
		t.Errorf("first message from: %v", from)
	}
	if p := list[2].(map[string]any)["prompt"].(map[string]any); p["id"] != "chat-slack:p1" {
		t.Errorf("prompt: %v", p)
	}
	if d := list[3].(map[string]any)["decision"].(map[string]any); d["promptId"] != "chat-slack:p1" || d["choice"] != "allow" {
		t.Errorf("decision: %v", d)
	}
	if last := list[4].(map[string]any); last["key"] != "status" {
		t.Errorf("live line: %v", last)
	}
}

// TestPostAndDecide: what the browser sends reaches the inbox as this
// transport's inbound — a message in a thread, a message to a channel
// (the coordinator opens the thread), a decision.
func TestPostAndDecide(t *testing.T) {
	_, srv, inbox := newTest(t, "")
	res, _ := post(t, srv, "/api/messages", map[string]any{"thread": "C1/1.1", "text": "go on", "user": "dan"}, "")
	if res.StatusCode != 200 {
		t.Fatalf("post: %d", res.StatusCode)
	}
	in := <-inbox
	if in.Transport != "web" || in.Thread != "C1/1.1" || in.UserID != "dan" || in.UserName != "dan" || in.Text != "go on" {
		t.Errorf("inbound: %+v", in)
	}
	post(t, srv, "/api/messages", map[string]any{"channel": "general", "text": "new work", "user": "dan"}, "")
	if in = <-inbox; in.Thread != "general/" || in.Text != "new work" {
		t.Errorf("channel inbound: %+v", in)
	}
	res, _ = post(t, srv, "/api/decide", map[string]any{"thread": "C1/1.1", "promptId": "chat-slack:p1", "choice": "allow", "user": "dan"}, "")
	if res.StatusCode != 200 {
		t.Fatalf("decide: %d", res.StatusCode)
	}
	if in = <-inbox; in.Decision == nil || in.Decision.PromptID != "chat-slack:p1" || in.Decision.Choice != "allow" || in.Text != "" {
		t.Errorf("decision inbound: %+v", in)
	}
	if res, _ := post(t, srv, "/api/messages", map[string]any{"thread": "C1/1.1", "text": "  ", "user": "dan"}, ""); res.StatusCode != 400 {
		t.Errorf("empty message: %d", res.StatusCode)
	}
}

// TestStream: Send pushes messages to open pages; a keyed message is an
// edit of one line until removed; OpenThread mints a thread in an own
// channel and announces it with its root.
func TestStream(t *testing.T) {
	tr, srv, _ := newTest(t, "")
	ch := events(t, srv, "")
	ctx := context.Background()
	tr.Send(ctx, transport.Outbound{Thread: "C1/1.1", Text: "hello", Markdown: true})
	if ev := next(t, ch, "message"); ev.Message.Text != "hello" || !ev.Message.Markdown || ev.Message.ID < liveBase {
		t.Errorf("message: %+v", ev.Message)
	}
	tr.Send(ctx, transport.Outbound{Thread: "C1/1.1", Key: "status", Text: "⏳ 1s"})
	first := next(t, ch, "edit")
	tr.Send(ctx, transport.Outbound{Thread: "C1/1.1", Key: "status", Text: "⏳ 2s"})
	if ev := next(t, ch, "edit"); ev.Message.ID != first.Message.ID || ev.Message.Text != "⏳ 2s" {
		t.Errorf("edit: %+v", ev.Message)
	}
	tr.Send(ctx, transport.Outbound{Thread: "C1/1.1", Key: "status"})
	if ev := next(t, ch, "remove"); ev.ID != first.Message.ID || ev.ThreadID != "C1/1.1" {
		t.Errorf("remove: %+v", ev)
	}
	// a prompt opens the thread's waiting flag; a relayed decision closes it
	tr.Send(ctx, transport.Outbound{Thread: "C1/1.1", Text: "🔐", Prompt: &transport.Prompt{ID: "chat-slack:p2", Choices: []string{"allow", "deny"}}})
	next(t, ch, "message")
	if _, st := get(t, srv, "/api/state", ""); !st["threads"].([]any)[0].(map[string]any)["waiting"].(bool) {
		t.Error("thread not waiting after a prompt")
	}
	tr.Send(ctx, transport.Outbound{Thread: "C1/1.1", Decision: &transport.Decision{PromptID: "chat-slack:p2", Choice: "deny"}, From: &transport.Author{ID: "dan", Name: "dan", Via: "web"}})
	if ev := next(t, ch, "message"); ev.Message.Decision == nil || ev.Message.Decision.Choice != "deny" || ev.Message.From.Via != "web" {
		t.Errorf("decision: %+v", ev.Message)
	}
	if _, st := get(t, srv, "/api/state", ""); st["threads"].([]any)[0].(map[string]any)["waiting"] != nil {
		t.Error("thread still waiting after the decision")
	}

	id, err := tr.OpenThread(ctx, "general", transport.Outbound{Text: "new work\nmore", From: &transport.Author{ID: "dan", Name: "dan", Via: "web"}})
	if err != nil || !strings.HasPrefix(string(id), "general/") {
		t.Fatalf("open: %q %v", id, err)
	}
	if ev := next(t, ch, "thread"); ev.Thread.ID != id || ev.Thread.Title != "new work" || ev.Thread.Transport != "web" {
		t.Errorf("thread event: %+v", ev.Thread)
	}
	if ev := next(t, ch, "message"); ev.Message.Thread != id || ev.Message.From.Name != "dan" {
		t.Errorf("root: %+v", ev.Message)
	}
	if _, err := tr.OpenThread(ctx, "C1", transport.Outbound{Text: "x"}); err == nil {
		t.Error("opened a thread in a channel that is not ours")
	}
	// the opened thread is listed even though History never heard of it
	_, st := get(t, srv, "/api/state", "")
	found := false
	for _, th := range st["threads"].([]any) {
		if th.(map[string]any)["id"] == string(id) {
			found = true
		}
	}
	if !found {
		t.Error("opened thread not listed")
	}
}

// TestToken: with a token set the API needs it, the static page does
// not, and login sets the cookie the page then sends.
func TestToken(t *testing.T) {
	_, srv, _ := newTest(t, "secret")
	if res, _ := get(t, srv, "/api/state", ""); res.StatusCode != 401 {
		t.Errorf("no token: %d", res.StatusCode)
	}
	if res, _ := get(t, srv, "/api/state", "secret"); res.StatusCode != 200 {
		t.Errorf("bearer: %d", res.StatusCode)
	}
	if res, err := http.Get(srv.URL + "/"); err != nil || res.StatusCode != 200 {
		t.Errorf("page: %v %v", res, err)
	}
	if res, _ := post(t, srv, "/api/login", map[string]any{"token": "nope"}, ""); res.StatusCode != 401 {
		t.Errorf("bad login: %d", res.StatusCode)
	}
	res, _ := post(t, srv, "/api/login", map[string]any{"token": "secret"}, "")
	if res.StatusCode != 200 {
		t.Fatalf("login: %d", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatalf("cookie: %+v", cookie)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
	req.AddCookie(cookie)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Errorf("cookie auth: %v %v", res, err)
	}
}
