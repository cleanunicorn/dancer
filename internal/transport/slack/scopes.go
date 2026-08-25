package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The bot scopes dancer uses, in one place so deploy/slack-manifest.yaml,
// the table in docs/slack.md and `dancer doctor` cannot drift
// (TestScopesMatchManifest pins all three together). Required scopes are
// the ones a feature is lost without; optional ones only make something
// nicer (channel names in the web UI, the composer status line) and doctor
// reports them as ℹ rather than ✘.
var (
	RequiredScopes = []string{
		"app_mentions:read",
		"channels:history",
		"groups:history",
		"im:history",
		"chat:write",
		"reactions:write",
		"files:read",
		"files:write",
		"users:read",
	}
	OptionalScopes = []string{
		"assistant:write",
		"channels:read",
		"groups:read",
		"im:read",
		"im:write",
	}
)

// AppScope is the one scope the app-level token (xapp-…) needs, for
// Socket Mode. It is granted when the token is generated, not by the
// manifest.
const AppScope = "connections:write"

// APIURL is the Slack Web API base; tests point it at a fake.
var APIURL = "https://slack.com/api/"

// Auth is what auth.test says about a token: who it is and, from the
// X-OAuth-Scopes response header Slack sets on every Web API call, which
// scopes it was granted. Scopes is nil when the header was absent.
type Auth struct {
	User   string
	UserID string
	Team   string
	Scopes []string
}

// AuthScopes calls auth.test with token and reads the granted scopes out of
// the response header. slack-go's AuthTest hides the headers, and auth.test
// is a one-line POST, so this is a plain net/http call rather than a
// wrapped client. Works for the bot token and the app-level token alike.
func AuthScopes(ctx context.Context, token string) (Auth, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, APIURL+"auth.test", strings.NewReader(url.Values{}.Encode()))
	if err != nil {
		return Auth{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Auth{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Auth{}, err
	}
	var r struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		User   string `json:"user"`
		UserID string `json:"user_id"`
		Team   string `json:"team"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Auth{}, fmt.Errorf("auth.test: HTTP %d: %w", resp.StatusCode, err)
	}
	if !r.OK {
		if r.Error == "" {
			r.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return Auth{}, fmt.Errorf("auth.test: %s", r.Error)
	}
	a := Auth{User: r.User, UserID: r.UserID, Team: r.Team}
	if h, ok := resp.Header["X-Oauth-Scopes"]; ok {
		a.Scopes = []string{}
		for _, part := range strings.Split(strings.Join(h, ","), ",") {
			if s := strings.TrimSpace(part); s != "" {
				a.Scopes = append(a.Scopes, s)
			}
		}
	}
	return a, nil
}

// MissingScopes returns the scopes in want that have does not carry, in
// want's order.
func MissingScopes(have, want []string) []string {
	got := map[string]bool{}
	for _, s := range have {
		got[s] = true
	}
	var missing []string
	for _, s := range want {
		if !got[s] {
			missing = append(missing, s)
		}
	}
	return missing
}
