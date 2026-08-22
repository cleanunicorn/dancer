package slack

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/cleanunicorn/dancer/internal/transport"
)

// fakeSlack is a Web API that says ok to everything and keeps the form of
// every call, keyed by method ("chat.postMessage").
type fakeSlack struct {
	mu    sync.Mutex
	calls []url.Values
}

func newFakeSlack(t *testing.T) (*fakeSlack, *Transport) {
	t.Helper()
	f := &fakeSlack{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		v := r.PostForm
		v.Set("method", strings.TrimPrefix(r.URL.Path, "/"))
		f.mu.Lock()
		f.calls = append(f.calls, v)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.1"}`))
	}))
	t.Cleanup(srv.Close)
	c := &Transport{
		api:          slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")),
		log:          slog.Default(),
		botUserID:    "UBOT",
		allowedUsers: map[string]bool{},
		threads:      map[transport.ThreadID]bool{},
		keyed:        map[string]string{},
	}
	return f, c
}

// of returns the calls to one method.
func (f *fakeSlack) of(method string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []url.Values
	for _, v := range f.calls {
		if v.Get("method") == method {
			out = append(out, v)
		}
	}
	return out
}

// sectionText is the text of the first section block in a blocks form value.
func sectionText(t *testing.T, blocks string) string {
	t.Helper()
	var bs []struct {
		Type string `json:"type"`
		Text *struct {
			Text string `json:"text"`
		} `json:"text"`
	}
	if err := json.Unmarshal([]byte(blocks), &bs); err != nil {
		t.Fatalf("blocks %q: %v", blocks, err)
	}
	for _, b := range bs {
		if b.Type == "section" && b.Text != nil {
			return b.Text.Text
		}
	}
	return ""
}

func TestSendMention(t *testing.T) {
	f, c := newFakeSlack(t)
	ctx := context.Background()
	for _, m := range []transport.Outbound{
		{Thread: "C1/1.0", Text: "✅ done", Mention: "U42"},
		{Thread: "C1/1.0", Text: "plain"},
		{Thread: "C1/1.0", Text: "🔐 run?", Mention: "U42", Prompt: &transport.Prompt{ID: "chat:p1", Choices: []string{"allow", "deny"}}},
		{Thread: "C1/1.0", Key: "status", Text: "⏳ thinking", Mention: "U42"},
	} {
		if err := c.Send(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	posts := f.of("chat.postMessage")
	if len(posts) != 4 {
		t.Fatalf("posted %d messages, want 4: %+v", len(posts), posts)
	}
	if got := posts[0].Get("text"); got != "<@U42> ✅ done" {
		t.Errorf("addressed text = %q", got)
	}
	if got := posts[1].Get("text"); got != "plain" {
		t.Errorf("unaddressed text = %q", got)
	}
	if got := posts[2].Get("text"); got != "<@U42> 🔐 run?" {
		t.Errorf("prompt fallback text = %q", got)
	}
	if got := sectionText(t, posts[2].Get("blocks")); got != "<@U42> 🔐 run?" {
		t.Errorf("prompt section text = %q", got)
	}
	if got := posts[3].Get("text"); got != "⏳ thinking" {
		t.Errorf("keyed message addressed: %q", got)
	}
}

func TestSettledPromptDropsLeadingMention(t *testing.T) {
	f, c := newFakeSlack(t)
	inbox := make(chan transport.Inbound, 1)
	text := address("🔐 *Bash* wants to run <@U7>", "U42")
	cb := slack.InteractionCallback{
		Type:    slack.InteractionTypeBlockActions,
		User:    slack.User{ID: "U7"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
		Message: slack.Message{Msg: slack.Msg{Timestamp: "1.2", ThreadTimestamp: "1.0", Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
		}}}},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{ActionID: "decision:allow", BlockID: "chat:p1", Value: "allow"}}},
	}
	c.handle(context.Background(), socketmode.Event{Type: socketmode.EventTypeInteractive, Data: cb}, inbox)
	updates := f.of("chat.update")
	if len(updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(updates))
	}
	if got, want := updates[0].Get("text"), "🔐 *Bash* wants to run <@U7>\n→ *allow* by <@U7>"; got != want {
		t.Errorf("settled text = %q, want %q", got, want)
	}
	select {
	case in := <-inbox:
		if in.Decision == nil || in.Decision.PromptID != "chat:p1" || in.Decision.Choice != "allow" || in.UserID != "U7" {
			t.Errorf("decision = %+v", in)
		}
	default:
		t.Error("no decision delivered")
	}
}
