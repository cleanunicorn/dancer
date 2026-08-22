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
// Ollama or vLLM. The policy is the system message, the question is the
// user message, and the model is asked for a JSON object. No tools are
// offered, so there is nothing for the model to do but answer.
//
// The API key is read once by whoever builds this value (from the
// environment, see config); it is never written to the log. An empty key
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
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Temperature    float64       `json:"temperature"`
	ResponseFormat *chatFormat   `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
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
		Temperature:    0,
		ResponseFormat: &chatFormat{Type: "json_object"},
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("decider: marshal request: %w", err)
	}
	url := strings.TrimRight(o.BaseURL, "/")
	if url == "" {
		url = "https://api.openai.com/v1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Verdict{}, fmt.Errorf("decider: openai: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Verdict{}, fmt.Errorf("decider: openai: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Verdict{}, fmt.Errorf("decider: openai: read response: %w", err)
	}
	var res chatResponse
	if jerr := json.Unmarshal(raw, &res); jerr != nil && resp.StatusCode == http.StatusOK {
		return Verdict{}, fmt.Errorf("decider: openai: parse response: %w: %s", jerr, truncate(string(raw), 200))
	}
	if res.Error != nil {
		return Verdict{}, fmt.Errorf("decider: openai: %s (%s, HTTP %d)", res.Error.Message, res.Error.Type, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, fmt.Errorf("decider: openai: HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))
	}
	if len(res.Choices) == 0 {
		return Verdict{}, fmt.Errorf("decider: openai: no choices in reply")
	}
	v, err := parseVerdict(res.Choices[0].Message.Content)
	if err != nil {
		return Verdict{}, err
	}
	v.By = o.Name()
	return Validate(q, v)
}
