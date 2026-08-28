package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/cleanunicorn/dispatch/internal/transport"
)

// fakeSlack is a Web API that says ok to everything and keeps the form of
// every call, with the method under "method".
type fakeSlack struct {
	mu    sync.Mutex
	calls []url.Values
	// url is where the fake listens; files are served under url/files/<name>
	// to a request carrying the bot token, as Slack serves url_private.
	url   string
	files map[string][]byte
}

// newFakeSlack returns a fake Web API and a transport wired to it. Only the
// Web API side is real: handle Acks through the socketmode client only when
// evt.Request is set, so tests pass events without one. An empty allowed
// list lets every user through, like the real thing.
func newFakeSlack(t *testing.T, allowed ...string) (*fakeSlack, *Transport) {
	t.Helper()
	f := &fakeSlack{files: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := strings.CutPrefix(r.URL.Path, "/files/"); ok {
			if r.Header.Get("Authorization") != "Bearer xoxb-test" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.mu.Lock()
			data, have := f.files[name]
			f.mu.Unlock()
			if !have {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(data)
			return
		}
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
	f.url = srv.URL
	return f, newTransport(slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")), "UBOT", allowed, slog.Default())
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

// blocksOf decodes the blocks a call posted.
func blocksOf(t *testing.T, form url.Values) slack.Blocks {
	t.Helper()
	var bs slack.Blocks
	if err := json.Unmarshal([]byte(form.Get("blocks")), &bs); err != nil {
		t.Fatalf("blocks %q: %v", form.Get("blocks"), err)
	}
	return bs
}

// sectionText is what the transport itself reads back as a prompt's text.
func sectionText(t *testing.T, form url.Values) string {
	t.Helper()
	return firstText(slack.Message{Msg: slack.Msg{Blocks: blocksOf(t, form)}})
}

// actionBlock is the buttons (or select) a call posted under a prompt.
func actionBlock(t *testing.T, form url.Values) *slack.ActionBlock {
	t.Helper()
	for _, b := range blocksOf(t, form).BlockSet {
		if a, ok := b.(*slack.ActionBlock); ok {
			return a
		}
	}
	t.Fatalf("no actions block in %q", form.Get("blocks"))
	return nil
}

// click is the callback Slack sends when user presses the i-th button of
// a posted prompt, built from what was actually posted.
func click(t *testing.T, form url.Values, user string, i int) slack.InteractionCallback {
	t.Helper()
	ab := actionBlock(t, form)
	btn, ok := ab.Elements.ElementSet[i].(*slack.ButtonBlockElement)
	if !ok {
		t.Fatalf("element %d is %T, not a button", i, ab.Elements.ElementSet[i])
	}
	return callback(t, form, user, &slack.BlockAction{ActionID: btn.ActionID, BlockID: ab.BlockID, Value: btn.Value})
}

// callback is the block-actions callback Slack posts back for the prompt
// message 1.2 in thread C1/1.0, carrying the blocks that were posted.
func callback(t *testing.T, form url.Values, user string, a *slack.BlockAction) slack.InteractionCallback {
	t.Helper()
	return slack.InteractionCallback{
		Type:           slack.InteractionTypeBlockActions,
		User:           slack.User{ID: user},
		Channel:        slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
		Message:        slack.Message{Msg: slack.Msg{Timestamp: "1.2", ThreadTimestamp: "1.0", Blocks: blocksOf(t, form)}},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{a}},
	}
}

// interactive wraps a callback the way Socket Mode hands it to handle,
// without a Request to Ack.
func interactive(cb slack.InteractionCallback) socketmode.Event {
	return socketmode.Event{Type: socketmode.EventTypeInteractive, Data: cb}
}

// allowDeny is the permission prompt the chat surface posts.
var allowDeny = &transport.Prompt{ID: "chat:p1", Choices: []string{"allow", "deny"}}

// assertAllowDeny checks that a posted prompt carries its two buttons.
func assertAllowDeny(t *testing.T, form url.Values) {
	t.Helper()
	ab := actionBlock(t, form)
	if ab.BlockID != "chat:p1" || len(ab.Elements.ElementSet) != 2 {
		t.Fatalf("actions block = %+v", ab)
	}
	for i, want := range []struct {
		id, value string
		style     slack.Style
	}{{"decision:0", "allow", slack.StylePrimary}, {"decision:1", "deny", slack.StyleDanger}} {
		btn, ok := ab.Elements.ElementSet[i].(*slack.ButtonBlockElement)
		if !ok || btn.ActionID != want.id || btn.Value != want.value || btn.Style != want.style {
			t.Errorf("button %d = %+v, want %+v", i, ab.Elements.ElementSet[i], want)
		}
	}
}

func TestSendMention(t *testing.T) {
	f, c := newFakeSlack(t)
	ctx := context.Background()
	for _, m := range []transport.Outbound{
		{Thread: "C1/1.0", Text: "✅ done", Mention: "U42"},
		{Thread: "C1/1.0", Text: "plain"},
		{Thread: "C1/1.0", Text: "🔐 run?", Mention: "U42", Prompt: allowDeny},
		{Thread: "C1/1.0", Key: "status", Text: "⏳ thinking", Mention: "U42"},
		{Thread: "C1/1.0", Text: strings.Repeat("x", 4000), Mention: "U42", Prompt: allowDeny},
		{Thread: "C1/1.0", Text: "ab" + strings.Repeat("中", 2000), Mention: "U42", Prompt: allowDeny},
	} {
		if err := c.Send(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	posts := f.of("chat.postMessage")
	if len(posts) != 6 {
		t.Fatalf("posted %d messages, want 6: %+v", len(posts), posts)
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
	if got := sectionText(t, posts[2]); got != "<@U42> 🔐 run?" {
		t.Errorf("prompt section text = %q", got)
	}
	assertAllowDeny(t, posts[2])
	if got := posts[3].Get("text"); got != "⏳ thinking" {
		t.Errorf("keyed message addressed: %q", got)
	}
	// An over-long prompt is cut, and still ships its buttons.
	if got := sectionText(t, posts[4]); got != "<@U42> "+strings.Repeat("x", 2892)+"…" {
		t.Errorf("long section text = %d bytes, %q…", len(got), got[:20])
	}
	if got := posts[4].Get("text"); len(got) > 4000 {
		t.Errorf("long fallback text is %d bytes", len(got))
	}
	assertAllowDeny(t, posts[4])
	// ...on a rune boundary.
	if got := sectionText(t, posts[5]); !utf8.ValidString(got) || utf8.RuneCountInString(got) > 3000 {
		t.Errorf("multi-byte cut: valid=%v runes=%d", utf8.ValidString(got), utf8.RuneCountInString(got))
	}
	assertAllowDeny(t, posts[5])
}

func TestSettledPromptDropsLeadingMention(t *testing.T) {
	f, c := newFakeSlack(t)
	ctx := context.Background()
	inbox := make(chan transport.Inbound, 1)
	if err := c.Send(ctx, transport.Outbound{Thread: "C1/1.0", Text: "🔐 *Bash* wants to run <@U7>", Mention: "U42", Prompt: allowDeny}); err != nil {
		t.Fatal(err)
	}
	post := f.of("chat.postMessage")[0]
	c.handle(ctx, interactive(click(t, post, "U7", 0)), inbox)
	updates := f.of("chat.update")
	if len(updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(updates))
	}
	if got, want := updates[0].Get("text"), "🔐 *Bash* wants to run <@U7>\n→ *allow* by <@U7>"; got != want {
		t.Errorf("settled text = %q, want %q", got, want)
	}
	// The clicked message is the one edited, and its buttons are gone.
	if u := updates[0]; u.Get("channel") != "C1" || u.Get("ts") != "1.2" || u.Get("blocks") != "[]" {
		t.Errorf("update = channel %q ts %q blocks %q", u.Get("channel"), u.Get("ts"), u.Get("blocks"))
	}
	select {
	case in := <-inbox:
		if in.Transport != "slack" || in.Thread != "C1/1.0" || in.Decision == nil || in.Decision.PromptID != "chat:p1" || in.Decision.Choice != "allow" || in.UserID != "U7" {
			t.Errorf("decision = %+v", in)
		}
	default:
		t.Error("no decision delivered")
	}
}

func TestSelectAnswersWithTheOption(t *testing.T) {
	f, c := newFakeSlack(t)
	ctx := context.Background()
	inbox := make(chan transport.Inbound, 1)
	p := &transport.Prompt{ID: "chat:p2", Question: "Which agent?"}
	for i := 0; i < selectThreshold; i++ {
		p.Options = append(p.Options, transport.Option{Value: fmt.Sprintf("a%d", i), Label: fmt.Sprintf("agent %d", i)})
	}
	if err := c.Send(ctx, transport.Outbound{Thread: "C1/1.0", Text: "❓ Which agent?", Mention: "U42", Prompt: p}); err != nil {
		t.Fatal(err)
	}
	post := f.of("chat.postMessage")[0]
	ab := actionBlock(t, post)
	sel, ok := ab.Elements.ElementSet[0].(*slack.SelectBlockElement)
	if !ok || ab.BlockID != "chat:p2" {
		t.Fatalf("actions block = %+v", ab)
	}
	// A select carries its choice in the option, not in the action's value.
	c.handle(ctx, interactive(callback(t, post, "U7", &slack.BlockAction{ActionID: sel.ActionID, BlockID: ab.BlockID, SelectedOption: slack.OptionBlockObject{Value: "a3"}})), inbox)
	select {
	case in := <-inbox:
		if in.Decision == nil || in.Decision.PromptID != "chat:p2" || in.Decision.Choice != "a3" {
			t.Errorf("decision = %+v", in)
		}
	default:
		t.Error("no decision delivered")
	}
	if updates := f.of("chat.update"); len(updates) != 1 || updates[0].Get("blocks") != "[]" || !strings.HasSuffix(updates[0].Get("text"), "→ *a3* by <@U7>") || strings.HasPrefix(updates[0].Get("text"), "<@U42>") {
		t.Errorf("settled select = %+v", updates)
	}
}

func TestDecisionFromUnlistedUserIsIgnored(t *testing.T) {
	f, c := newFakeSlack(t, "U9")
	ctx := context.Background()
	inbox := make(chan transport.Inbound, 2)
	if err := c.Send(ctx, transport.Outbound{Thread: "C1/1.0", Text: "🔐 run?", Mention: "U9", Prompt: allowDeny}); err != nil {
		t.Fatal(err)
	}
	post := f.of("chat.postMessage")[0]
	c.handle(ctx, interactive(click(t, post, "U7", 0)), inbox)
	if n := len(f.of("chat.update")); n != 0 || len(inbox) != 0 {
		t.Fatalf("unlisted user settled the prompt: updates=%d inbox=%d", n, len(inbox))
	}
	c.handle(ctx, interactive(click(t, post, "U9", 1)), inbox)
	if n := len(f.of("chat.update")); n != 1 || len(inbox) != 1 {
		t.Fatalf("listed user could not answer: updates=%d inbox=%d", n, len(inbox))
	}
	if in := <-inbox; in.UserID != "U9" || in.Decision.Choice != "deny" {
		t.Errorf("decision = %+v", in)
	}
}
