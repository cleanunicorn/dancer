package decider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chatServer is an OpenAI-compatible stand-in that records the request it
// got and answers with the given body and status.
func chatServer(t *testing.T, status int, reply string) (*httptest.Server, *map[string]json.RawMessage, *http.Header) {
	t.Helper()
	var got map[string]json.RawMessage
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		hdr = r.Header.Clone()
		// Raw keys, not chatRequest: the test must see which fields were
		// sent, and a typed decode cannot tell "absent" from "zero".
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &hdr
}

func question() Question {
	return Question{Kind: "resume", Task: "t-1", Thread: "C1/1.0",
		Options: []string{"continue", "wait"},
		Facts:   map[string]any{"last_prompt": "add retries"},
		Static:  Verdict{Action: "continue", Prompt: "carry on"}}
}

func TestOpenAIAsksTheQuestionAndReadsTheVerdict(t *testing.T) {
	srv, got, hdr := chatServer(t, http.StatusOK,
		`{"choices":[{"message":{"role":"assistant","content":"{\"action\":\"wait\",\"reason\":\"stale\"}"},"finish_reason":"stop"}]}`)
	d := OpenAI{BaseURL: srv.URL + "/v1/", APIKey: "sk-test", Model: "gpt-4o-mini"}
	v, err := d.Decide(context.Background(), question())
	if err != nil {
		t.Fatal(err)
	}
	if v.Action != "wait" || v.Reason != "stale" || v.By != "openai" {
		t.Fatalf("verdict = %+v", v)
	}
	var model string
	var messages []chatMessage
	if err := json.Unmarshal((*got)["model"], &model); err != nil || model != "gpt-4o-mini" {
		t.Fatalf("model = %s (%v)", (*got)["model"], err)
	}
	if err := json.Unmarshal((*got)["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "system" || messages[0].Content != policy || messages[1].Role != "user" {
		t.Fatalf("messages = %+v", messages)
	}
	if !strings.Contains(messages[1].Content, `"last_prompt":"add retries"`) || !strings.Contains(messages[1].Content, `"options":["continue","wait"]`) {
		t.Fatalf("the question was not handed over whole: %s", messages[1].Content)
	}
	// Reasoning models reject any temperature but their default, and not
	// every endpoint implements response_format: neither may be sent.
	for _, k := range []string{"temperature", "response_format", "tools"} {
		if _, sent := (*got)[k]; sent {
			t.Fatalf("%q was sent: %s", k, (*got)[k])
		}
	}
	if hdr.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", hdr.Get("Authorization"))
	}
}

func TestOpenAIWithoutAKeySendsNoAuthorization(t *testing.T) {
	srv, _, hdr := chatServer(t, http.StatusOK,
		`{"choices":[{"message":{"content":"Sure! {\"action\":\"continue\",\"prompt\":\"go on\"} Hope that helps."}}]}`)
	d := OpenAI{BaseURL: srv.URL + "/v1", Model: "llama3.2"}
	v, err := d.Decide(context.Background(), question())
	if err != nil {
		t.Fatal(err)
	}
	if v.Action != "continue" || v.Prompt != "go on" {
		t.Fatalf("verdict = %+v (prose around the JSON should be tolerated)", v)
	}
	if _, set := (*hdr)["Authorization"]; set {
		t.Fatalf("Authorization header sent without a key: %q", hdr.Get("Authorization"))
	}
}

func TestOpenAIFailuresAreErrorsNotVerdicts(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		reply  string
		want   string
	}{
		"api error":       {http.StatusUnauthorized, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`, "Incorrect API key"},
		"http error":      {http.StatusBadGateway, `<html>bad gateway</html>`, "HTTP 502"},
		"no choices":      {http.StatusOK, `{"choices":[]}`, "no choices"},
		"not json":        {http.StatusOK, `{"choices":[{"message":{"content":"I would continue."}}]}`, "no JSON object"},
		"outside options": {http.StatusOK, `{"choices":[{"message":{"content":"{\"action\":\"abandon\"}"}}]}`, "not one of"},
		"truncated":       {http.StatusOK, `{"choices":[{"message":{"content":"{\"action\":\"continue\",\"prompt\":\"Finish the"},"finish_reason":"length"}]}`, "truncated"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _, _ := chatServer(t, tc.status, tc.reply)
			d := OpenAI{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m"}
			_, err := d.Decide(context.Background(), question())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	if _, err := (OpenAI{BaseURL: "http://127.0.0.1:9"}).Decide(context.Background(), question()); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("a missing model should be an error before any request: %v", err)
	}
	if _, err := (OpenAI{Model: "m"}).Decide(context.Background(), question()); err == nil || !strings.Contains(err.Error(), "no base_url") {
		t.Fatalf("a missing base_url should be an error before any request: %v", err)
	}
}

// TestOpenAIPing: doctor's endpoint check — reachable and keyed is fine,
// a rejected key or a dead server is an error that quotes the reason.
func TestOpenAIPing(t *testing.T) {
	var hdr http.Header
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		hdr = r.Header.Clone()
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`))
		}
	}))
	t.Cleanup(srv.Close)

	d := OpenAI{BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "m"}
	if err := d.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hdr.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", hdr.Get("Authorization"))
	}
	status = http.StatusUnauthorized
	if err := d.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "Incorrect API key") {
		t.Fatalf("a rejected key should be reported: %v", err)
	}
	if err := (OpenAI{BaseURL: "http://127.0.0.1:9/v1"}).Ping(context.Background()); err == nil {
		t.Fatal("a dead server should be an error")
	}
}
