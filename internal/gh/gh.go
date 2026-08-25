// Package gh lends the host's GitHub CLI login, and the git identity that
// goes with it, to a container.
//
// An agent that opens pull requests needs a logged-in `gh`, and a fresh
// container has none: `gh pr create` there ends with "To get started with
// GitHub CLI, please run: gh auth login". Rather than make every docker
// definition carry a token, dancer lends the host's login the way it lends
// the claude one: before a task starts it writes the host's hosts.yml into
// the environment's gh config dir and runs `gh auth setup-git`, so both
// `gh` and `git push` speak for the operator's account.
//
// It lends two things: the login `gh` authenticates with, and the name and
// email git commits with, which a fresh container has no more of than it
// has a token (identity.go).
//
// The login comes from the first source that has a token:
//
//   - the host's hosts.yml ($GH_CONFIG_DIR, else $XDG_CONFIG_HOME/gh, else
//     ~/.config/gh) — copied verbatim, so every host it knows about
//     (github.com, an enterprise server) is lent with it;
//   - `gh auth token` on the host, for a login kept in the system keyring,
//     where hosts.yml holds the account but no token;
//   - GH_TOKEN / GITHUB_TOKEN in dancer's own environment.
//
// The copy keeps the source's mtime, and a hosts.yml in the environment
// that is newer than that is left alone: something in there logged in
// after dancer last lent, and that login is the current one.
//
// Nothing is lent when
//   - the environment is not docker: local is the same home already, and
//     ssh is someone else's machine with its own login, not dancer's to
//     overwrite;
//   - the definition's env carries GH_TOKEN or GITHUB_TOKEN: that is the
//     operator choosing how the container authenticates.
//
// When the host has nothing to lend the task still runs; only GitHub is
// out of reach, and `gh` says so itself.
package gh

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cleanunicorn/dancer/internal/environment"
)

// keyEnv are the variables that authenticate gh without a login. A
// definition carrying one of them is left alone.
var keyEnv = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// DefaultHost is the host a synthesized hosts.yml is written for.
const DefaultHost = "github.com"

// lendScript installs the hosts.yml read from stdin under the gh config dir
// ($GH_CONFIG_DIR, else $XDG_CONFIG_HOME/gh, else ~/.config/gh), unless the
// file there is newer than $1 — the source's mtime as a `touch -t` stamp in
// UTC. Either way it points git at gh as a credential helper, so a push
// from the agent uses the same login. It prints "kept" or "copied". POSIX
// sh only: it runs in whatever /bin/sh the image has.
const lendScript = `set -e
d="${GH_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/gh}"
f="$d/hosts.yml"
ref="$d/.dancer-lend-ref"
result=copied
umask 077
mkdir -p "$d"
TZ=UTC touch -t "$1" "$ref"
if [ -s "$f" ] && [ "$f" -nt "$ref" ]; then
	cat > /dev/null
	result=kept
else
	cat > "$f.tmp"
	mv -f "$f.tmp" "$f"
	TZ=UTC touch -t "$1" "$f"
fi
rm -f "$ref"
if command -v gh >/dev/null 2>&1; then
	gh auth setup-git >/dev/null 2>&1 || result="$result no-setup-git"
else
	result="$result no-gh"
fi
echo "$result"
`

// oauthToken matches a hosts.yml entry that carries a token of its own.
var oauthToken = regexp.MustCompile(`(?m)^[ \t]+oauth_token:[ \t]*[^ \t\r\n#]`)

// Login is the host's GitHub login as something that can be written into a
// container: the bytes of a hosts.yml, and the mtime to stamp it with.
type Login struct {
	Hosts []byte
	// ModTime is when the login last changed on the host, so a fresher
	// login inside the environment survives. Synthesized logins carry the
	// current time: nothing on disk vouches for them.
	ModTime time.Time
	// Source names where it came from, for the logs and for doctor.
	Source string
}

// ConfigDir is gh's config directory on this host, by gh's own lookup.
func ConfigDir() string {
	if d := os.Getenv("GH_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "gh")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gh")
}

// HostsPath is the host's hosts.yml, gh's record of who is logged in where.
func HostsPath() string {
	d := ConfigDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "hosts.yml")
}

// HostLogin finds a login on this host to lend, trying hosts.yml, then the
// `gh auth token` the CLI can produce from a keyring, then dancer's own
// environment.
func HostLogin(ctx context.Context) (Login, error) {
	if path := HostsPath(); path != "" {
		if data, err := os.ReadFile(path); err == nil && oauthToken.Match(data) {
			mtime := time.Now()
			if fi, err := os.Stat(path); err == nil {
				mtime = fi.ModTime()
			}
			return Login{Hosts: data, ModTime: mtime, Source: path}, nil
		}
	}
	if tok, err := cliToken(ctx); err == nil {
		return synthesize(tok, "gh auth token"), nil
	}
	for _, k := range keyEnv {
		if tok := strings.TrimSpace(os.Getenv(k)); usableToken(tok) {
			return synthesize(tok, k), nil
		}
	}
	return Login{}, fmt.Errorf("gh: no GitHub login on this host to lend")
}

// cliToken asks the host's gh for the token it would use, which is the only
// way to reach a login the CLI keeps in the system keyring.
func cliToken(ctx context.Context) (string, error) {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := []string{"auth", "token"}
	if h := strings.TrimSpace(os.Getenv("GH_HOST")); h != "" {
		args = append(args, "--hostname", h)
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(out))
	if !usableToken(tok) {
		return "", fmt.Errorf("gh: `gh auth token` printed no token")
	}
	return tok, nil
}

// usableToken rejects anything that is not a bare token, so nothing that
// would need quoting or span lines reaches the file dancer writes.
func usableToken(tok string) bool {
	return tok != "" && !strings.ContainsAny(tok, " \t\r\n\"'#")
}

// synthesize writes the minimal hosts.yml gh needs for one host: a token,
// and https so the credential helper is the one that answers for it.
func synthesize(token, source string) Login {
	host := strings.TrimSpace(os.Getenv("GH_HOST"))
	if host == "" {
		host = DefaultHost
	}
	body := fmt.Sprintf("%s:\n    oauth_token: %s\n    git_protocol: https\n", host, token)
	return Login{Hosts: []byte(body), ModTime: time.Now(), Source: source}
}

// Lend gives env what it needs to work on GitHub as the host does: the
// host's login, unless the definition authenticates on its own, and the
// host's git identity, unless the container or the definition already has
// one (identity.go). envVars is the definition's environment env.
//
// It never fails a task: a login that cannot be lent is logged, and the
// agent meets gh's own "please run gh auth login", which says more about
// what to do than anything dancer could report from here.
func Lend(ctx context.Context, env environment.Environment, envVars map[string]string) {
	if env.Kind() != environment.KindDocker {
		return
	}
	// The identity is lent whatever the login turns out to be: a container
	// that cannot say who committed is stuck before it ever pushes.
	lendIdentity(ctx, env, envVars)
	if hasKey(envVars) {
		return
	}
	login, err := HostLogin(ctx)
	if err != nil {
		slog.Debug("gh: no host login to lend", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stamp := login.ModTime.UTC().Format("200601021504.05")
	out, err := environment.Run(ctx, env, login.Hosts, "sh", "-c", lendScript, "sh", stamp)
	if err != nil {
		slog.Warn("gh: could not lend host login", "err", err)
		return
	}
	slog.Debug("gh: lent host login", "from", login.Source, "result", out)
}

// hasKey reports whether the definition's environment authenticates gh on
// its own.
func hasKey(envVars map[string]string) bool {
	for _, k := range keyEnv {
		if v, ok := envVars[k]; ok && v != "" {
			return true
		}
	}
	return false
}
