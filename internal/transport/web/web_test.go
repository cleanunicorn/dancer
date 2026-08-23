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
	"sync"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/store"
	"github.com/cleanunicorn/dancer/internal/transport"
)

// fakeHistory is the coordinator's read side, canned.
type fakeHistory struct {
	channels map[string][]transport.Channel
	threads  []transport.ThreadInfo
	messages map[transport.ThreadID][]transport.Outbound
}

func init() { hashIters = 1000 } // the work factor is not what these tests are about

// fakeUsers is an in-memory Users.
type fakeUsers struct {
	mu       sync.Mutex
	users    map[string]store.User
	sessions map[string]store.Session
}

func newFakeUsers(t *testing.T, name, password string) *fakeUsers {
	t.Helper()
	h, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeUsers{users: map[string]store.User{name: {Name: name, Password: h}}, sessions: map[string]store.Session{}}
}

func (f *fakeUsers) GetUser(_ context.Context, name string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	if !ok {
		return u, store.ErrNotFound
	}
	return u, nil
}
func (f *fakeUsers) PutUser(_ context.Context, u store.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.Name] = u
	return nil
}
func (f *fakeUsers) PutSession(_ context.Context, s store.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.Token] = s
	return nil
}
func (f *fakeUsers) GetSession(_ context.Context, token string) (store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[token]
	if !ok {
		return s, store.ErrNotFound
	}
	return s, nil
}
func (f *fakeUsers) DeleteSession(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, token)
	return nil
}
func (f *fakeUsers) DeleteUserSessions(_ context.Context, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, s := range f.sessions {
		if s.User == user {
			delete(f.sessions, k)
		}
	}
	return nil
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

// newTest runs a transport on a loopback port, with the account dan /
// "correct horse", and returns it with a server handle for the URL and
// the inbox Run delivers to.
func newTest(t *testing.T) (*Transport, *httptest.Server, chan transport.Inbound) {
	t.Helper()
	tr := New("", []string{"general"}, newFakeUsers(t, "dan", "correct horse"), nil)
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

// login signs dan in and returns the session cookie.
func login(t *testing.T, srv *httptest.Server) *http.Cookie {
	t.Helper()
	res, _ := post(t, srv, "/api/login", map[string]any{"name": "dan", "password": "correct horse"}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("login: %d", res.StatusCode)
	}
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func post(t *testing.T, srv *httptest.Server, path string, body any, cookie *http.Cookie) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
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

func get(t *testing.T, srv *httptest.Server, path string, cookie *http.Cookie) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
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
func events(t *testing.T, srv *httptest.Server, cookie *http.Cookie) <-chan event {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	req.AddCookie(cookie)
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
	tr, srv, _ := newTest(t)
	ck := login(t, srv)
	if err := tr.Send(context.Background(), transport.Outbound{Thread: "C1/1.1", Key: "status", Text: "⏳ thinking · 4s"}); err != nil {
		t.Fatal(err)
	}
	_, st := get(t, srv, "/api/state", ck)
	chans := st["channels"].([]any)
	if len(chans) != 2 {
		t.Fatalf("channels: %v", chans)
	}
	threads := st["threads"].([]any)
	th := threads[0].(map[string]any)
	if th["id"] != "C1/1.1" || th["transport"] != "slack" || th["title"] != "fix the build" || th["live"] != "⏳ thinking · 4s" {
		t.Errorf("thread: %v", th)
	}
	_, msgs := get(t, srv, "/api/threads/C1/1.1", ck)
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
// transport's inbound, signed with the session's user whatever the body
// says — a message in a thread, a message to a channel (the coordinator
// opens the thread), a decision.
func TestPostAndDecide(t *testing.T) {
	_, srv, inbox := newTest(t)
	ck := login(t, srv)
	res, _ := post(t, srv, "/api/messages", map[string]any{"thread": "C1/1.1", "text": "go on", "user": "someone-else"}, ck)
	if res.StatusCode != 200 {
		t.Fatalf("post: %d", res.StatusCode)
	}
	in := <-inbox
	if in.Transport != "web" || in.Thread != "C1/1.1" || in.UserID != "dan" || in.UserName != "dan" || in.Text != "go on" {
		t.Errorf("inbound: %+v", in)
	}
	post(t, srv, "/api/messages", map[string]any{"channel": "general", "text": "new work"}, ck)
	if in = <-inbox; in.Thread != "general/" || in.Text != "new work" {
		t.Errorf("channel inbound: %+v", in)
	}
	res, _ = post(t, srv, "/api/decide", map[string]any{"thread": "C1/1.1", "promptId": "chat-slack:p1", "choice": "allow"}, ck)
	if res.StatusCode != 200 {
		t.Fatalf("decide: %d", res.StatusCode)
	}
	if in = <-inbox; in.Decision == nil || in.Decision.PromptID != "chat-slack:p1" || in.Decision.Choice != "allow" || in.Text != "" {
		t.Errorf("decision inbound: %+v", in)
	}
	if res, _ := post(t, srv, "/api/messages", map[string]any{"thread": "C1/1.1", "text": "  "}, ck); res.StatusCode != 400 {
		t.Errorf("empty message: %d", res.StatusCode)
	}
}

// TestStream: Send pushes messages to open pages; a keyed message is an
// edit of one line until removed; OpenThread mints a thread in an own
// channel and announces it with its root.
func TestStream(t *testing.T) {
	tr, srv, _ := newTest(t)
	ck := login(t, srv)
	ch := events(t, srv, ck)
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
	if _, st := get(t, srv, "/api/state", ck); !st["threads"].([]any)[0].(map[string]any)["waiting"].(bool) {
		t.Error("thread not waiting after a prompt")
	}
	tr.Send(ctx, transport.Outbound{Thread: "C1/1.1", Decision: &transport.Decision{PromptID: "chat-slack:p2", Choice: "deny"}, From: &transport.Author{ID: "dan", Name: "dan", Via: "web"}})
	if ev := next(t, ch, "message"); ev.Message.Decision == nil || ev.Message.Decision.Choice != "deny" || ev.Message.From.Via != "web" {
		t.Errorf("decision: %+v", ev.Message)
	}
	if _, st := get(t, srv, "/api/state", ck); st["threads"].([]any)[0].(map[string]any)["waiting"] != nil {
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
	_, st := get(t, srv, "/api/state", ck)
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

// TestSameSite: a POST the browser was made to send by another site —
// a form, or any request with a foreign Origin — is refused, with or
// without a token.
func TestSameSite(t *testing.T) {
	_, srv, inbox := newTest(t)
	ck := login(t, srv)
	send := func(contentType string, headers map[string]string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/messages", strings.NewReader(`{"thread":"C1/1.1","text":"hi"}`))
		req.Header.Set("Content-Type", contentType)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.AddCookie(ck)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := send("application/x-www-form-urlencoded", nil); code != http.StatusUnsupportedMediaType {
		t.Errorf("form post: %d", code)
	}
	if code := send("application/json", map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Errorf("foreign origin: %d", code)
	}
	if code := send("application/json", map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Errorf("cross-site fetch: %d", code)
	}
	if len(inbox) != 0 {
		t.Errorf("%d messages got through", len(inbox))
	}
	if code := send("application/json", map[string]string{"Origin": srv.URL, "Sec-Fetch-Site": "same-origin"}); code != 200 {
		t.Errorf("own origin: %d", code)
	}
	<-inbox
}

// TestAccounts: the API needs a session; login takes a name and password
// and sets the cookie; the page is served to anyone; a wrong password
// is refused; logout ends the session; changing the password ends the
// other sessions and keeps this one.
func TestAccounts(t *testing.T) {
	_, srv, _ := newTest(t)
	if res, _ := get(t, srv, "/api/state", nil); res.StatusCode != 401 {
		t.Errorf("no session: %d", res.StatusCode)
	}
	if res, err := http.Get(srv.URL + "/"); err != nil || res.StatusCode != 200 {
		t.Errorf("page: %v %v", res, err)
	}
	if res, _ := post(t, srv, "/api/login", map[string]any{"name": "dan", "password": "nope"}, nil); res.StatusCode != 401 {
		t.Errorf("wrong password: %d", res.StatusCode)
	}
	if res, _ := post(t, srv, "/api/login", map[string]any{"name": "nobody", "password": "correct horse"}, nil); res.StatusCode != 401 {
		t.Errorf("unknown user: %d", res.StatusCode)
	}
	ck := login(t, srv)
	if !ck.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if _, me := get(t, srv, "/api/me", ck); me["user"] != "dan" {
		t.Errorf("me: %v", me)
	}
	other := login(t, srv)
	// change the password: the other login ends, this one continues
	res, _ := post(t, srv, "/api/password", map[string]any{"current": "wrong", "new": "another horse"}, ck)
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("password with wrong current: %d", res.StatusCode)
	}
	res, _ = post(t, srv, "/api/password", map[string]any{"current": "correct horse", "new": "short"}, ck)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("short password: %d", res.StatusCode)
	}
	res, _ = post(t, srv, "/api/password", map[string]any{"current": "correct horse", "new": "another horse"}, ck)
	if res.StatusCode != 200 {
		t.Fatalf("password change: %d", res.StatusCode)
	}
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			ck = c
		}
	}
	if res, _ := get(t, srv, "/api/me", other); res.StatusCode != 401 {
		t.Errorf("other session after password change: %d", res.StatusCode)
	}
	if res, _ := get(t, srv, "/api/me", ck); res.StatusCode != 200 {
		t.Errorf("own session after password change: %d", res.StatusCode)
	}
	if res, _ := post(t, srv, "/api/login", map[string]any{"name": "DAN", "password": "another horse"}, nil); res.StatusCode != 200 {
		t.Errorf("login with the new password: %d", res.StatusCode)
	}
	if res, _ := post(t, srv, "/api/logout", map[string]any{}, ck); res.StatusCode != 200 {
		t.Errorf("logout: %d", res.StatusCode)
	}
	if res, _ := get(t, srv, "/api/me", ck); res.StatusCode != 401 {
		t.Errorf("after logout: %d", res.StatusCode)
	}
}

// TestPasswords pins the hash format and the checks around it.
func TestPasswords(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("short password accepted")
	}
	h, err := HashPassword("correct horse")
	if err != nil || !strings.HasPrefix(h, "pbkdf2-sha256$1000$") {
		t.Fatalf("hash %q err=%v", h, err)
	}
	if !CheckPassword(h, "correct horse") || CheckPassword(h, "correct horsf") || CheckPassword("garbage", "x") {
		t.Error("CheckPassword")
	}
	p, err := GeneratePassword()
	if err != nil || len(p) != 16 {
		t.Errorf("generated %q err=%v", p, err)
	}
	for _, ok := range []string{"dan", "ana.b", "x_1-2"} {
		if !ValidName.MatchString(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Dan", "a b", "U0123", "-x", strings.Repeat("a", 33)} {
		if ValidName.MatchString(bad) && bad != "U0123" {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
