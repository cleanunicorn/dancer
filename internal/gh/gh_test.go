package gh

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/environment"
	"github.com/cleanunicorn/dancer/internal/environment/local"
)

const hostsWithToken = "github.com:\n    oauth_token: gho_hosttoken\n    user: octocat\n"

// shEnv runs commands on the host with its own HOME, standing in for a
// container: the lend script only cares about $HOME and a POSIX sh.
type shEnv struct {
	environment.Environment
	kind environment.Kind
}

func (e shEnv) Kind() environment.Kind { return e.kind }

// newShEnv returns an environment whose gh config lands under home, with
// the host's own GH_CONFIG_DIR/XDG_CONFIG_HOME masked off so a developer's
// real gh config can never be what the script writes to, and a `gh` shim
// first on PATH so a real one cannot rewrite the file the test just wrote.
// It returns the home and the file the shim logs its arguments to.
func newShEnv(t *testing.T, kind environment.Kind, env map[string]string) (shEnv, string, string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	home := t.TempDir()
	shim := t.TempDir()
	log := filepath.Join(shim, "calls")
	if err := os.WriteFile(filepath.Join(shim, "gh"), []byte("#!/bin/sh\necho \"$@\" >> \"$GH_SHIM_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"HOME":              home,
		"GH_CONFIG_DIR":     "",
		"XDG_CONFIG_HOME":   "",
		"GH_SHIM_LOG":       log,
		"PATH":              shim + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL": filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_SYSTEM": filepath.Join(home, ".gitconfig-system"),
	}
	for k, v := range env {
		vars[k] = v
	}
	e, err := local.Factory{}.New(environment.Spec{Workdir: t.TempDir(), Env: vars})
	if err != nil {
		t.Fatal(err)
	}
	return shEnv{Environment: e, kind: kind}, home, log
}

// hostConfig points this process's gh lookup at a fresh config dir and
// clears every other source of a token, so a test only sees what it wrote.
func hostConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", dir)
	t.Setenv("GH_HOST", "")
	for _, k := range keyEnv {
		t.Setenv(k, "")
	}
	// The same for the identity: a config of its own, so the developer's
	// name is neither what a test reads nor what one could overwrite.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(dir, "gitconfig-system"))
	for _, k := range append(identityEnv, "GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME") {
		t.Setenv(k, "")
	}
	return dir
}

// hostIdentity gives this host the identity a test expects to see lent.
func hostIdentity(t *testing.T, name, email string) {
	t.Helper()
	body := "[user]\n"
	if name != "" {
		body += "\tname = " + name + "\n"
	}
	if email != "" {
		body += "\temail = " + email + "\n"
	}
	if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// containerIdentity is what the environment would commit as.
func containerIdentity(t *testing.T, home string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if err != nil {
		return ""
	}
	return string(body)
}

func writeHosts(t *testing.T, dir, body string, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hosts.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLendCopiesHostHostsFile(t *testing.T) {
	dir := hostConfig(t)
	hostTime := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	writeHosts(t, dir, hostsWithToken, hostTime)
	env, home, log := newShEnv(t, environment.KindDocker, nil)

	Lend(context.Background(), env, nil)

	dst := filepath.Join(home, ".config", "gh", "hosts.yml")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("hosts.yml not lent: %v", err)
	}
	if string(got) != hostsWithToken {
		t.Fatalf("lent %q", got)
	}
	fi, _ := os.Stat(dst)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if !fi.ModTime().Equal(hostTime) {
		t.Errorf("mtime = %v, want the host's %v", fi.ModTime(), hostTime)
	}
	if left, _ := filepath.Glob(filepath.Join(home, ".config", "gh", ".dancer-lend-ref*")); len(left) > 0 {
		t.Errorf("reference file left behind: %v", left)
	}
	// The scratch file is per-process, so two lends into one reused
	// container are never two writers on the same path.
	if left, _ := filepath.Glob(filepath.Join(home, ".config", "gh", "hosts.yml.tmp*")); len(left) > 0 {
		t.Errorf("temporary file left behind: %v", left)
	}
	// git has to speak for the same account, or `git push` in the
	// container asks for a password nobody can type.
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "auth setup-git") {
		t.Errorf("gh calls = %q, want auth setup-git", calls)
	}
}

func TestLendKeepsNewerCopy(t *testing.T) {
	dir := hostConfig(t)
	hostTime := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	writeHosts(t, dir, hostsWithToken, hostTime)
	env, home, _ := newShEnv(t, environment.KindDocker, nil)
	// Someone logged in inside the container after the host last did.
	inside := writeHosts(t, filepath.Join(home, ".config", "gh"),
		"github.com:\n    oauth_token: gho_inside\n", hostTime.Add(time.Minute))

	Lend(context.Background(), env, nil)
	if got, _ := os.ReadFile(inside); !strings.Contains(string(got), "gho_inside") {
		t.Fatalf("newer copy overwritten: %q", got)
	}

	// The host logs in again later: its copy wins.
	writeHosts(t, dir, "github.com:\n    oauth_token: gho_host2\n", hostTime.Add(time.Hour))
	Lend(context.Background(), env, nil)
	if got, _ := os.ReadFile(inside); !strings.Contains(string(got), "gho_host2") {
		t.Fatalf("newer host copy not lent: %q", got)
	}
}

// A token with no file behind it (`gh auth token`, GH_TOKEN) is stamped
// with the current time, so it is lent again at every task and a login made
// inside the container does not survive it. That is what SETUP.md promises;
// the keep-newer rule only holds for the host's own hosts.yml.
func TestLendReLendsASynthesizedToken(t *testing.T) {
	hostConfig(t)
	env, home, _ := newShEnv(t, environment.KindDocker, nil)
	t.Setenv("GH_TOKEN", "gho_env")
	inside := filepath.Join(home, ".config", "gh")
	writeHosts(t, inside, "github.com:\n    oauth_token: gho_inside\n", time.Now().Add(-time.Minute))

	Lend(context.Background(), env, nil)

	got, err := os.ReadFile(filepath.Join(inside, "hosts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "gho_env") {
		t.Fatalf("hosts.yml = %q, want the synthesized token lent again", got)
	}
}

func TestLendHonoursGHConfigDir(t *testing.T) {
	dir := hostConfig(t)
	writeHosts(t, dir, hostsWithToken, time.Now())
	cfg := filepath.Join(t.TempDir(), "gh-config")
	env, _, _ := newShEnv(t, environment.KindDocker, map[string]string{"GH_CONFIG_DIR": cfg})

	Lend(context.Background(), env, nil)

	if _, err := os.Stat(filepath.Join(cfg, "hosts.yml")); err != nil {
		t.Fatalf("not lent into GH_CONFIG_DIR: %v", err)
	}
}

// recEnv records whether anything was executed.
type recEnv struct {
	environment.Environment
	kind  environment.Kind
	execs int
}

func (e *recEnv) Kind() environment.Kind { return e.kind }
func (e *recEnv) Exec(ctx context.Context, name string, args ...string) (environment.Process, error) {
	e.execs++
	return nil, io.ErrUnexpectedEOF
}

func TestLendSkips(t *testing.T) {
	dir := hostConfig(t)
	writeHosts(t, dir, hostsWithToken, time.Now())
	cases := []struct {
		name string
		kind environment.Kind
		env  map[string]string
	}{
		{"local", environment.KindLocal, nil},
		{"ssh", environment.KindSSH, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := &recEnv{kind: c.kind}
			Lend(context.Background(), env, c.env)
			if env.execs != 0 {
				t.Errorf("executed %d commands, want none", env.execs)
			}
		})
	}
}

// noCLI takes gh off PATH so HostLogin cannot reach a login this machine
// happens to have.
func noCLI(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestHostLoginPrefersHostsFile(t *testing.T) {
	dir := hostConfig(t)
	path := writeHosts(t, dir, hostsWithToken, time.Now())
	t.Setenv("GH_TOKEN", "gho_env")

	login, err := HostLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(login.Hosts) != hostsWithToken || login.Source != path {
		t.Fatalf("login = %q from %s", login.Hosts, login.Source)
	}
}

func TestHostLoginFallsBackToEnvToken(t *testing.T) {
	hostConfig(t)
	noCLI(t)
	t.Setenv("GH_TOKEN", "gho_env")

	login, err := HostLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "github.com:\n    oauth_token: gho_env\n    git_protocol: https\n"
	if string(login.Hosts) != want {
		t.Fatalf("login = %q, want %q", login.Hosts, want)
	}
	if login.Source != "GH_TOKEN" {
		t.Errorf("source = %q", login.Source)
	}
}

// A hosts.yml can name the account while the token itself lives in the
// system keyring; that file is not a login and must not be lent as one.
func TestHostLoginIgnoresTokenlessHostsFile(t *testing.T) {
	dir := hostConfig(t)
	writeHosts(t, dir, "github.com:\n    user: octocat\n    git_protocol: ssh\n", time.Now())
	noCLI(t)
	t.Setenv("GITHUB_TOKEN", "gho_env")

	login, err := HostLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(login.Hosts), "gho_env") {
		t.Fatalf("login = %q, want the env token", login.Hosts)
	}
}

func TestHostLoginWithoutAnyLogin(t *testing.T) {
	hostConfig(t)
	noCLI(t)
	if login, err := HostLogin(context.Background()); err == nil {
		t.Fatalf("login = %q, want an error", login.Hosts)
	}
}

func TestHostLoginRejectsUnusableToken(t *testing.T) {
	hostConfig(t)
	noCLI(t)
	t.Setenv("GH_TOKEN", "not a token\nfoo: bar")
	if _, err := HostLogin(context.Background()); err == nil {
		t.Fatal("a token that would not survive the file was accepted")
	}
}

func TestHostsPathFollowsGHLookup(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", "/cfg/gh")
	if got := HostsPath(); got != "/cfg/gh/hosts.yml" {
		t.Errorf("HostsPath = %q", got)
	}
	t.Setenv("GH_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := HostsPath(); got != "/xdg/gh/hosts.yml" {
		t.Errorf("HostsPath = %q", got)
	}
}
