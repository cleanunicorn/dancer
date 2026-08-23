package web

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cleanunicorn/dancer/internal/store"
)

// Users are accounts of the web UI. Every request to the API carries a
// session cookie that names one; that name is the Inbound.UserID (and
// UserName) of whatever the person sends, so the agent's closing line
// and prompts address them by it and a second person in the same thread
// is a second name. Accounts are made with `dancer user add` and kept
// in the store with a PBKDF2 hash of the password; sessions are kept
// there too, by the hash of the token the browser holds, so a restart
// logs nobody out and a copy of the database logs nobody in.

// Users is the part of the store the transport needs for accounts.
type Users interface {
	GetUser(ctx context.Context, name string) (store.User, error)
	PutUser(ctx context.Context, u store.User) error
	PutSession(ctx context.Context, s store.Session) error
	GetSession(ctx context.Context, token string) (store.Session, error)
	DeleteSession(ctx context.Context, token string) error
	DeleteUserSessions(ctx context.Context, user string) error
}

const (
	sessionCookie = "dancer_session"
	sessionTTL    = 30 * 24 * time.Hour
	hashLen       = 32
)

// hashIters is PBKDF2's work factor for new hashes (OWASP's figure for
// SHA-256); a stored hash carries its own. Tests lower it.
var hashIters = 600_000

// ValidName reports whether a user name is one dancer accepts: a short
// word, so it reads well in "✅ done" lines and cannot be confused with
// a Slack id.
var ValidName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// HashPassword encodes a password as "pbkdf2-sha256$<iter>$<salt>$<hash>".
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIters, hashLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", hashIters, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword says whether password matches an encoded hash.
func CheckPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil || iters <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iters, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// GeneratePassword makes a random password for `dancer user add` to
// print: 16 characters from an alphabet without look-alikes.
func GeneratePassword() (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// user resolves the request's session to a user name; "" when there is
// none or it expired.
func (t *Transport) user(r *http.Request) string {
	if t.Users == nil {
		return ""
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	s, err := t.Users.GetSession(r.Context(), tokenHash(c.Value))
	if err != nil {
		return ""
	}
	if t.now().After(s.ExpiresAt) {
		_ = t.Users.DeleteSession(r.Context(), s.Token)
		return ""
	}
	return s.User
}

// auth wraps an API handler with the session check and hands the
// handler the user.
func (t *Transport) auth(h func(w http.ResponseWriter, r *http.Request, user string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := t.user(r)
		if u == "" {
			jsonError(w, http.StatusUnauthorized, "login required")
			return
		}
		h(w, r, u)
	}
}

func (t *Transport) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if t.Users == nil {
		jsonError(w, http.StatusServiceUnavailable, "no user store")
		return
	}
	body.Name = strings.ToLower(strings.TrimSpace(body.Name))
	u, err := t.Users.GetUser(r.Context(), body.Name)
	// Check a hash either way so a missing name costs the same time.
	if err != nil {
		u.Password = missingHash
	}
	if !CheckPassword(u.Password, body.Password) || err != nil {
		t.log.Warn("web login refused", "user", body.Name, "from", r.RemoteAddr)
		jsonError(w, http.StatusUnauthorized, "wrong name or password")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := t.now()
	if err := t.Users.PutSession(r.Context(), store.Session{Token: tokenHash(token), User: u.Name, CreatedAt: now, ExpiresAt: now.Add(sessionTTL)}); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	t.log.Info("web login", "user", u.Name)
	writeJSON(w, map[string]any{"user": u.Name})
}

// missingHash is checked against when the name is unknown, so the
// answer takes as long as for a real user.
var missingHash = func() string {
	h, _ := HashPassword("no-such-user-password")
	return h
}()

func (t *Transport) logout(w http.ResponseWriter, r *http.Request, _ string) {
	if c, err := r.Cookie(sessionCookie); err == nil && t.Users != nil {
		_ = t.Users.DeleteSession(r.Context(), tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, map[string]any{"ok": true})
}

func (t *Transport) me(w http.ResponseWriter, _ *http.Request, user string) {
	writeJSON(w, map[string]any{"user": user})
}

// password changes the user's own password, given the current one, and
// ends their other sessions.
func (t *Transport) password(w http.ResponseWriter, r *http.Request, user string) {
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := t.Users.GetUser(r.Context(), user)
	if err != nil || !CheckPassword(u.Password, body.Current) {
		jsonError(w, http.StatusForbidden, "current password is wrong")
		return
	}
	h, err := HashPassword(body.New)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	u.Password = h
	if err := t.Users.PutUser(r.Context(), u); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Every other login of this user ends; this one gets a fresh session.
	_ = t.Users.DeleteUserSessions(r.Context(), user)
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := t.now()
	_ = t.Users.PutSession(r.Context(), store.Session{Token: tokenHash(token), User: user, CreatedAt: now, ExpiresAt: now.Add(sessionTTL)})
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	writeJSON(w, map[string]any{"ok": true})
}
