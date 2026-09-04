package codex

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
)

var keyEnv = []string{"OPENAI_API_KEY", "CODEX_API_KEY"}

const noLoginHint = " — the container has no Codex login and the host has none to lend: run `codex login` on the host that runs dispatch, or set CODEX_API_KEY or OPENAI_API_KEY in the definition's environment env"

const lendScript = `set -e
d="${CODEX_HOME:-$HOME/.codex}"
f="$d/auth.json"
ref="$d/.dispatch-lend-ref.$$"
tmp="$f.tmp.$$"
umask 077
mkdir -p "$d"
TZ=UTC touch -t "$1" "$ref"
if [ -s "$f" ] && [ "$f" -nt "$ref" ]; then rm -f "$ref"; echo kept; exit 0; fi
cat > "$tmp"
mv -f "$tmp" "$f"
TZ=UTC touch -t "$1" "$f"
rm -f "$ref"
echo copied
`

func hostCredentials() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}
func hasKey(def agent.Definition) bool {
	for _, k := range keyEnv {
		if def.Environment.Env[k] != "" {
			return true
		}
	}
	return false
}
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
		slog.Debug("codex: no host login to lend", "path", path, "err", err)
		return noLoginHint
	}
	fi, err := os.Stat(path)
	if err != nil {
		return noLoginHint
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stamp := fi.ModTime().UTC().Format("200601021504.05")
	out, err := environment.Run(cctx, env, data, "sh", "-c", lendScript, "sh", stamp)
	if err != nil {
		slog.Warn("codex: could not lend host login", "err", err)
		return ""
	}
	slog.Debug("codex: lent host login", "result", out)
	return ""
}
