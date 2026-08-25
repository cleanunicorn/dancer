package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// manifestBotScopes reads oauth_config.scopes.bot out of the app manifest
// with a line scanner: the manifest is our own, and a YAML dependency for
// one list is not worth it.
func manifestBotScopes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "slack-manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var scopes []string
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case trim == "bot:":
			in = true
		case in && strings.HasPrefix(trim, "- "):
			scopes = append(scopes, strings.TrimSpace(strings.TrimPrefix(trim, "- ")))
		case in && trim != "" && !strings.HasPrefix(trim, "#"):
			in = false
		}
	}
	if len(scopes) == 0 {
		t.Fatal("no bot scopes found in deploy/slack-manifest.yaml")
	}
	return scopes
}

// TestScopesMatchManifest pins RequiredScopes+OptionalScopes to the
// manifest and to the table in docs/slack.md, so adding a scope in one
// place fails until the other two follow.
func TestScopesMatchManifest(t *testing.T) {
	ours := append(slices.Clone(RequiredScopes), OptionalScopes...)
	manifest := manifestBotScopes(t)
	if m := MissingScopes(manifest, ours); len(m) > 0 {
		t.Errorf("in slack.RequiredScopes/OptionalScopes but not in deploy/slack-manifest.yaml: %v", m)
	}
	if m := MissingScopes(ours, manifest); len(m) > 0 {
		t.Errorf("in deploy/slack-manifest.yaml but not in slack.RequiredScopes/OptionalScopes: %v", m)
	}
	for _, s := range RequiredScopes {
		if slices.Contains(OptionalScopes, s) {
			t.Errorf("%s is both required and optional", s)
		}
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "slack.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(doc), "| scope | used for | without it |")
	if !ok {
		t.Fatal("docs/slack.md: scopes table not found")
	}
	table, _, _ = strings.Cut(table, "\n\n")
	documented := regexp.MustCompile("`([a-z_]+:[a-z]+)`").FindAllStringSubmatch(table, -1)
	var inDocs []string
	for _, m := range documented {
		inDocs = append(inDocs, m[1])
	}
	if m := MissingScopes(inDocs, ours); len(m) > 0 {
		t.Errorf("not in the docs/slack.md scopes table: %v", m)
	}
	if m := MissingScopes(ours, inDocs); len(m) > 0 {
		t.Errorf("in the docs/slack.md scopes table but not in slack.RequiredScopes/OptionalScopes: %v", m)
	}
}

func TestAuthScopes(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/auth.test" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		switch gotAuth {
		case "Bearer xoxb-good":
			w.Header().Set("X-OAuth-Scopes", "chat:write, files:read,im:read")
			w.Write([]byte(`{"ok":true,"user":"dancer","user_id":"U1","team":"acme"}`))
		case "Bearer xoxb-noheader":
			w.Write([]byte(`{"ok":true,"user":"dancer","user_id":"U1","team":"acme"}`))
		default:
			w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		}
	}))
	defer srv.Close()
	old := APIURL
	APIURL = srv.URL + "/"
	defer func() { APIURL = old }()

	a, err := AuthScopes(context.Background(), "xoxb-good")
	if err != nil {
		t.Fatal(err)
	}
	if a.User != "dancer" || a.Team != "acme" || a.UserID != "U1" {
		t.Errorf("identity: %+v", a)
	}
	if want := []string{"chat:write", "files:read", "im:read"}; !slices.Equal(a.Scopes, want) {
		t.Errorf("scopes = %v, want %v", a.Scopes, want)
	}
	if m := MissingScopes(a.Scopes, RequiredScopes); len(m) != len(RequiredScopes)-2 {
		t.Errorf("missing = %v", m)
	}

	a, err = AuthScopes(context.Background(), "xoxb-noheader")
	if err != nil {
		t.Fatal(err)
	}
	if a.Scopes != nil {
		t.Errorf("no header should give nil scopes, got %v", a.Scopes)
	}

	if _, err := AuthScopes(context.Background(), "xoxb-bad"); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("bad token: err = %v", err)
	}
}
