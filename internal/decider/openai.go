package decider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI answers with one chat completion against an OpenAI-compatible
// endpoint: api.openai.com itself, or anything that speaks the same
// /chat/completions shape — DeepSeek, Groq, Mistral, OpenRouter, a local
// Ollama or vLLM. The policy is the system message and the question is the
// user message. No tools are offered, so there is nothing for the model to
// do but answer.
//
// The request carries only model and messages. No temperature (reasoning
// models reject anything but their default) and no response_format (not
// every endpoint implements it): the policy already asks for one JSON
// object and parseVerdict tolerates prose around it, exactly as it does
// for the Claude backend.
//
// The key comes from config and is never written to the log. An empty key
// sends no Authorization header, which is what a local server wants.
type OpenAI struct {
	BaseURL string        // e.g. "https://api.openai.com/v1"; the path is appended
	APIKey  string        // bearer token, "" for servers that need none
	Model   string        // e.g. "gpt-4o-mini", "deepseek-chat", "llama3.2"
	Timeout time.Duration // 0: bounded by the caller's context only
	Client  *http.Client  // nil: http.DefaultClient
}

func (o OpenAI) Name() string { return "openai" }

// Wire shapes: only the fields this package reads or writes.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func (o OpenAI) Decide(ctx context.Context, q Question) (Verdict, error) {
	if o.Model == "" {
		return Verdict{}, fmt.Errorf("decider: openai: no model configured")
	}
	question, err := json.Marshal(q)
	if err != nil {
		return Verdict{}, fmt.Errorf("decider: marshal question: %w", err)
	}
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	body, err := json.Marshal(chatRequest{
		Model: o.Model,
		Messages: []chatMessage{
			{Role: "system", Content: policy},
			{Role: "user", Content: string(question)},
		},
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("decider: marshal request: %w", err)
	}
	raw, status, err := o.call(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return Verdict{}, err
	}
	var res chatResponse
	if jerr := json.Unmarshal(raw, &res); jerr != nil && status == http.StatusOK {
		return Verdict{}, fmt.Errorf("decider: openai: parse response: %w: %s", jerr, truncate(string(raw), 200))
	}
	if err := apiStatus(status, raw, res.Error); err != nil {
		return Verdict{}, err
	}
	if len(res.Choices) == 0 {
		return Verdict{}, fmt.Errorf("decider: openai: no choices in reply")
	}
	choice := res.Choices[0]
	if choice.FinishReason == "length" {
		return Verdict{}, fmt.Errorf("decider: openai: reply truncated by the server (finish_reason=length): %s", truncate(choice.Message.Content, 200))
	}
	v, err := parseVerdict(choice.Message.Content)
	if err != nil {
		return Verdict{}, err
	}
	v.By = o.Name()
	return Validate(q, v)
}

// Ping proves the endpoint is reachable and accepts the key: one GET
// /models, which every OpenAI-compatible server implements. It does not
// check that Model is served — model lists are not comparable across
// providers (Ollama appends ":latest", OpenRouter lists thousands).
func (o OpenAI) Ping(ctx context.Context) error {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	raw, status, err := o.call(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return err
	}
	var res struct {
		Error *apiError `json:"error"`
	}
	_ = json.Unmarshal(raw, &res) // best effort: an error body may not be JSON
	return apiStatus(status, raw, res.Error)
}

// call performs one request against BaseURL+path and returns the body and
// status. Transport failures are errors; HTTP failures are the caller's to
// read, since the body usually says why.
func (o OpenAI) call(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	if o.BaseURL == "" {
		return nil, 0, fmt.Errorf("decider: openai: no base_url configured")
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(o.BaseURL, "/")+path, rd)
	if err != nil {
		return nil, 0, fmt.Errorf("decider: openai: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("decider: openai: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("decider: openai: read response: %w", err)
	}
	return raw, resp.StatusCode, nil
}

// apiStatus turns a non-2xx reply, or a 2xx carrying an error object, into
// an error that quotes what the server said.
func apiStatus(status int, raw []byte, apiErr *apiError) error {
	if apiErr != nil {
		return fmt.Errorf("decider: openai: %s (%s, HTTP %d)", apiErr.Message, apiErr.Type, status)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("decider: openai: HTTP %d: %s", status, truncate(strings.TrimSpace(string(raw)), 200))
	}
	return nil
}
