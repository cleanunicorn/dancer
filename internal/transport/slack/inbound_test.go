package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/cleanunicorn/dispatch/internal/transport"
)

// callback wraps an Events API inner event the way Socket Mode hands it
// to handle, without a Request to Ack.
func eventsAPI(inner any) socketmode.Event {
	return socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: inner},
	}}
}

// hosted is a Slack file record pointing at the fake's download URL.
func hosted(f *fakeSlack, name string, data []byte) slack.File {
	f.mu.Lock()
	f.files[name] = data
	f.mu.Unlock()
	return slack.File{ID: "F" + name, Name: name, Size: len(data), URLPrivateDownload: f.url + "/files/" + name}
}

// A mention with files delivers them downloaded; so does a reply with a
// file in a thread dispatch follows (a file_share message).
func TestInboundFiles(t *testing.T) {
	f, c := newFakeSlack(t)
	ctx := context.Background()
	inbox := make(chan transport.Inbound, 4)

	shot := hosted(f, "shot.png", []byte("png-bytes"))
	c.handle(ctx, eventsAPI(&slackevents.AppMentionEvent{Channel: "C1", TimeStamp: "1.0", User: "U7", Text: "<@UBOT> what is this?", Files: []slack.File{shot}}), inbox)
	in := <-inbox
	if in.Text != "what is this?" || in.Thread != "C1/1.0" || len(in.Files) != 1 {
		t.Fatalf("inbound = %+v", in)
	}
	if in.Files[0].Name != "shot.png" || string(in.Files[0].Data) != "png-bytes" {
		t.Errorf("file = %+v", in.Files[0])
	}

	// The thread is known now: a file_share reply there comes through too,
	// files and all, though slackevents keeps them under Message.
	log := hosted(f, "app.log", []byte("boom"))
	c.handle(ctx, eventsAPI(&slackevents.MessageEvent{Channel: "C1", TimeStamp: "1.5", ThreadTimeStamp: "1.0", User: "U7", Text: "and the log", SubType: "file_share", Message: &slack.Msg{Files: []slack.File{log}}}), inbox)
	in = <-inbox
	if in.Text != "and the log" || len(in.Files) != 1 || string(in.Files[0].Data) != "boom" {
		t.Fatalf("reply = %+v", in)
	}

	// Other subtypes are still not a human talking.
	c.handle(ctx, eventsAPI(&slackevents.MessageEvent{Channel: "C1", TimeStamp: "1.6", ThreadTimeStamp: "1.0", User: "U7", Text: "edited", SubType: "message_changed"}), inbox)
	select {
	case in := <-inbox:
		t.Fatalf("message_changed delivered: %+v", in)
	default:
	}
	if len(f.of("chat.postMessage")) != 0 {
		t.Errorf("nothing to warn about, yet posted: %v", f.of("chat.postMessage"))
	}
}

// A file that cannot be fetched is reported in the thread and skipped;
// the message still arrives with the files that could.
func TestInboundFileSkipped(t *testing.T) {
	f, c := newFakeSlack(t)
	ctx := context.Background()
	inbox := make(chan transport.Inbound, 4)

	ok := hosted(f, "ok.txt", []byte("fine"))
	big := slack.File{ID: "Fbig", Name: "big.zip", Size: maxFileBytes + 1, URLPrivateDownload: f.url + "/files/big.zip"}
	gone := slack.File{ID: "Fgone", Name: "gone.png", Size: 3, URLPrivateDownload: f.url + "/files/gone.png"}
	hidden := slack.File{ID: "Fhidden", Filetype: "png", Size: 3} // no URL: hidden by a plan limit
	c.handle(ctx, eventsAPI(&slackevents.AppMentionEvent{Channel: "C1", TimeStamp: "2.0", User: "U7", Text: "<@UBOT> look", Files: []slack.File{big, ok, gone, hidden}}), inbox)
	in := <-inbox
	if in.Text != "look" || len(in.Files) != 1 || in.Files[0].Name != "ok.txt" {
		t.Fatalf("inbound = %+v", in)
	}
	posts := f.of("chat.postMessage")
	if len(posts) != 3 {
		t.Fatalf("%d warnings, want 3: %v", len(posts), posts)
	}
	for i, want := range []string{"`big.zip` skipped: 20 MiB is over the 20 MiB limit", "`gone.png` skipped:", "`Fhidden.png` skipped: no download link"} {
		if got := posts[i].Get("text"); !strings.Contains(got, want) || posts[i].Get("thread_ts") != "2.0" {
			t.Errorf("warning %d = %q (thread %q), want %q", i, got, posts[i].Get("thread_ts"), want)
		}
	}
}

// A download past the cap is cut off even when Slack said it was smaller.
func TestCapWriter(t *testing.T) {
	w := &capWriter{max: 5}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("def")); err != errTooBig {
		t.Fatalf("past the cap: %v", err)
	}
	if w.buf.String() != "abc" {
		t.Errorf("kept %q", w.buf.String())
	}
}

func TestFileName(t *testing.T) {
	cases := []struct {
		f    slack.File
		want string
	}{
		{slack.File{ID: "F1", Name: "a.png", Title: "A"}, "a.png"},
		{slack.File{ID: "F1", Title: "A"}, "A"},
		{slack.File{ID: "F1", Filetype: "pdf"}, "F1.pdf"},
		{slack.File{ID: "F1"}, "F1"},
	}
	for _, c := range cases {
		if got := fileName(c.f); got != c.want {
			t.Errorf("fileName(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}
