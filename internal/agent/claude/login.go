package claude

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
)

// Lending the host's login.
//
// A container has no ~/.claude of its own, so `claude` inside it starts out
// logged out and the first turn ends with "Not logged in · Please run
// /login". Rather than make every docker definition carry a token, the
// driver lends the host's login: before each turn it copies the host's
// ~/.claude/.credentials.json into the environment's $HOME/.claude. The CLI
// in the container then refreshes the token itself like any other login.
//
// The copy keeps the host file's mtime, and a copy in the environment that
// is newer than that is left alone: the CLI there refreshed the token
// after the host did, and its credentials are the current ones.
//
// The driver does not lend when
//   - the definition's env already carries a key (ANTHROPIC_API_KEY,
//     CLAUDE_CODE_OAUTH_TOKEN, a Bedrock/Vertex/Foundry switch): that is the
//     operator choosing how the container authenticates;
//   - the environment is local: it is the same home already;
//   - the environment is ssh: that is someone's machine with its own
//     login, and the host's credentials are not dancer's to put there.
//
// When the host has no login to lend the turn still runs, and the CLI's
// "Not logged in" result is annotated with what to do about it.

// keyEnv are the variables that make the CLI authenticate without a login.
var keyEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
}

// noLoginHint is appended to the CLI's "Not logged in" result when the
// driver had nothing to lend.
const noLoginHint = " — the container has no login and the host has none to lend: " +
	"log in with `claude` on the host that runs dancer, or set CLAUDE_CODE_OAUTH_TOKEN " +
	"(from `claude setup-token`) or ANTHROPIC_API_KEY in the definition's environment env"

// lendScript installs the credentials read from stdin as
// $CLAUDE_CONFIG_DIR/.credentials.json (default ~/.claude), unless the file
// there is newer than $1, the host file's mtime as a `touch -t` stamp in
// UTC. It prints "kept" or "copied". POSIX sh only: it runs in whatever
// /bin/sh the image has.
//
// The scratch files carry $$: a reused container is shared by every task on
// its thread or definition, and two turns lending at once must not be two
// writers on one ".credentials.json.tmp".
const lendScript = `set -e
d="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
f="$d/.credentials.json"
ref="$d/.dancer-lend-ref.$$"
tmp="$f.tmp.$$"
umask 077
mkdir -p "$d"
TZ=UTC touch -t "$1" "$ref"
if [ -s "$f" ] && [ "$f" -nt "$ref" ]; then
	rm -f "$ref"
	echo kept
	exit 0
fi
cat > "$tmp"
mv -f "$tmp" "$f"
TZ=UTC touch -t "$1" "$f"
rm -f "$ref"
echo copied
`

// hostCredentials is the login file on the host: the CLI's own lookup,
// $CLAUDE_CONFIG_DIR/.credentials.json, else ~/.claude/.credentials.json.
func hostCredentials() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".credentials.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// hasKey reports whether the definition's environment authenticates the
// CLI on its own.
func hasKey(def agent.Definition) bool {
	for _, k := range keyEnv {
		if v, ok := def.Environment.Env[k]; ok && v != "" {
			return true
		}
	}
	return false
}

// lendLogin copies the host's login into env when the turn would otherwise
// run logged out. It returns the hint to attach to a "Not logged in"
// result: empty when there is nothing to add, because lending was not
// needed or it worked. A failed copy is logged and the turn goes on; the
// CLI's own error is clearer than anything the driver could fail with.
func (a *Agent) lendLogin(ctx context.Context, env environment.Environment, def agent.Definition) string {
	if env.Kind() != environment.KindDocker || hasKey(def) {
		return ""
	}
	path := a.Credentials
	if path == "" {
		path = hostCredentials()
	}
	data, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		slog.Debug("claude: no host login to lend", "path", path, "err", err)
		return noLoginHint
	}
	fi, err := os.Stat(path)
	if err != nil {
		return noLoginHint
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stamp := fi.ModTime().UTC().Format("200601021504.05")
	out, err := environment.Run(ctx, env, data, "sh", "-c", lendScript, "sh", stamp)
	if err != nil {
		slog.Warn("claude: could not lend host login", "err", err)
		return ""
	}
	slog.Debug("claude: lent host login", "result", out)
	return ""
}
