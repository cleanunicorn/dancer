package gh

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cleanunicorn/dancer/internal/environment"
)

// Lending the host's git identity.
//
// The login is only half of committing as yourself: a fresh container has
// no user.name or user.email either, so `git commit` in it stops with
// "Please tell me who you are" before the token is ever used. The identity
// that goes with a lent login is the host's own, so dancer lends that too —
// whatever `git config user.name` / `user.email` says on the machine
// running dancer, or the GIT_AUTHOR_*/GIT_COMMITTER_* variables in its
// environment.
//
// It is written once, as the container's global git config, and never
// overwritten: an identity already set in there was chosen by the operator
// (a definition's setup command) or by the agent, and dancer's guess does
// not get to win. That also makes it stable across a reused container,
// whose $HOME is a volume.
//
// Nothing is lent when the definition's env sets an identity of its own
// (GIT_AUTHOR_EMAIL / GIT_COMMITTER_EMAIL), the same way a definition's own
// GH_TOKEN stops the login being lent.

// identityEnv are the variables that give a container its own committer.
var identityEnv = []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"}

// identityScript sets the environment's global git identity to $1 (name)
// and $2 (email) unless it already has one. It prints "set", "kept" or
// "none". POSIX sh only.
const identityScript = `set -e
command -v git >/dev/null 2>&1 || { echo no-git; exit 0; }
if [ -n "$(git config --global --get user.email 2>/dev/null || true)" ]; then
	echo kept
	exit 0
fi
if [ -z "$2" ]; then
	echo none
	exit 0
fi
git config --global user.email "$2"
if [ -n "$1" ]; then
	git config --global user.name "$1"
fi
echo set
`

// Identity is who commits: the name and email git signs with, and where
// dancer read them.
type Identity struct {
	Name   string
	Email  string
	Source string
}

// String is the identity as git itself would write it in a commit.
func (i Identity) String() string {
	switch {
	case i.Name != "" && i.Email != "":
		return fmt.Sprintf("%s <%s>", i.Name, i.Email)
	case i.Email != "":
		return i.Email
	default:
		return i.Name
	}
}

// HostIdentity is the identity of the human running dancer: git's own
// answer first (the global config, then whatever config applies here), and
// the GIT_AUTHOR_*/GIT_COMMITTER_* variables when git has no answer or is
// not installed. An identity without an email is not one git can commit
// with, so that is an error.
func HostIdentity(ctx context.Context) (Identity, error) {
	name, email, source := gitConfigIdentity(ctx)
	if email == "" {
		name, email, source = envIdentity()
	}
	if !usableField(email) || email == "" {
		return Identity{}, fmt.Errorf("gh: no git identity on this host to lend")
	}
	if !usableField(name) {
		name = ""
	}
	return Identity{Name: name, Email: email, Source: source}, nil
}

// gitConfigIdentity asks the host's git. --global first: dancer's working
// directory may be inside a repository whose local config answers for that
// repository only, which is not the host's identity.
func gitConfigIdentity(ctx context.Context) (name, email, source string) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return "", "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	get := func(scope, key string) string {
		args := []string{"config"}
		if scope != "" {
			args = append(args, scope)
		}
		out, err := exec.CommandContext(ctx, bin, append(args, "--get", key)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	for _, scope := range []string{"--global", ""} {
		if e := get(scope, "user.email"); e != "" {
			where := "git config " + strings.TrimPrefix(scope+" ", " ")
			return get(scope, "user.name"), e, strings.TrimSpace(where)
		}
	}
	return "", "", ""
}

// envIdentity reads the identity git would take from the environment when
// there is no config to read it from.
func envIdentity() (name, email, source string) {
	for _, pair := range [][2]string{
		{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL"},
		{"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"},
	} {
		if e := strings.TrimSpace(os.Getenv(pair[1])); e != "" {
			return strings.TrimSpace(os.Getenv(pair[0])), e, pair[1]
		}
	}
	return "", "", ""
}

// usableField rejects anything that would not survive being written into a
// config file as one value.
func usableField(s string) bool {
	return !strings.ContainsAny(s, "\r\n")
}

// lendIdentity gives the environment the host's committer, unless it has
// one already or the definition chose one.
func lendIdentity(ctx context.Context, env environment.Environment, envVars map[string]string) {
	for _, k := range identityEnv {
		if v, ok := envVars[k]; ok && v != "" {
			return
		}
	}
	id, err := HostIdentity(ctx)
	if err != nil {
		slog.Debug("gh: no host git identity to lend", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := environment.Run(ctx, env, nil, "sh", "-c", identityScript, "sh", id.Name, id.Email)
	if err != nil {
		slog.Warn("gh: could not lend git identity", "err", err)
		return
	}
	slog.Debug("gh: lent git identity", "identity", id.String(), "from", id.Source, "result", out)
}
