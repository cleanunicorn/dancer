package claude

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/environment/local"
)

// shEnv runs commands on the host with its own HOME, standing in for a
// container: the lend script only cares about $HOME and a POSIX sh.
type shEnv struct {
	environment.Environment
	kind environment.Kind
}

func (e shEnv) Kind() environment.Kind { return e.kind }

func newShEnv(t *testing.T, kind environment.Kind) (shEnv, string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	home := t.TempDir()
	env, err := local.Factory{}.New(environment.Spec{Workdir: t.TempDir(), Env: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	return shEnv{Environment: env, kind: kind}, home
}

func writeCreds(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestLendLoginCopiesHostCredentials(t *testing.T) {
	env, home := newShEnv(t, environment.KindDocker)
	host := filepath.Join(t.TempDir(), ".credentials.json")
	hostTime := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	writeCreds(t, host, `{"claudeAiOauth":{"accessToken":"a1"}}`, hostTime)

	a := &Agent{Credentials: host}
	if hint := a.lendLogin(context.Background(), env, agent.Definition{}); hint != "" {
		t.Fatalf("hint = %q, want none", hint)
	}
	dst := filepath.Join(home, ".claude", ".credentials.json")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("credentials not lent: %v", err)
	}
	if string(got) != `{"claudeAiOauth":{"accessToken":"a1"}}` {
		t.Fatalf("lent %q", got)
	}
	fi, _ := os.Stat(dst)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if !fi.ModTime().Equal(hostTime) {
		t.Errorf("mtime = %v, want the host's %v", fi.ModTime(), hostTime)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", ".dispatch-lend-ref")); err == nil {
		t.Error("reference file left behind")
	}
}

func TestLendLoginKeepsNewerCopy(t *testing.T) {
	env, home := newShEnv(t, environment.KindDocker)
	host := filepath.Join(t.TempDir(), ".credentials.json")
	hostTime := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	writeCreds(t, host, `{"host":true}`, hostTime)
	dst := filepath.Join(home, ".claude", ".credentials.json")
	// The container's CLI refreshed the token a minute after the host did.
	writeCreds(t, dst, `{"refreshed":true}`, hostTime.Add(time.Minute))

	a := &Agent{Credentials: host}
	a.lendLogin(context.Background(), env, agent.Definition{})
	if got, _ := os.ReadFile(dst); string(got) != `{"refreshed":true}` {
		t.Fatalf("newer copy overwritten: %q", got)
	}

	// The host logs in again later: its copy wins.
	writeCreds(t, host, `{"host":2}`, hostTime.Add(time.Hour))
	a.lendLogin(context.Background(), env, agent.Definition{})
	if got, _ := os.ReadFile(dst); string(got) != `{"host":2}` {
		t.Fatalf("newer host copy not lent: %q", got)
	}
}

func TestLendLoginHonoursConfigDir(t *testing.T) {
	env, home := newShEnv(t, environment.KindDocker)
	cfg := filepath.Join(home, "cfg")
	env.Environment, _ = local.Factory{}.New(environment.Spec{Workdir: t.TempDir(), Env: map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": cfg}})
	host := filepath.Join(t.TempDir(), ".credentials.json")
	writeCreds(t, host, `{}`, time.Now())
	(&Agent{Credentials: host}).lendLogin(context.Background(), env, agent.Definition{})
	if _, err := os.Stat(filepath.Join(cfg, ".credentials.json")); err != nil {
		t.Fatalf("not lent into CLAUDE_CONFIG_DIR: %v", err)
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

func TestLendLoginSkips(t *testing.T) {
	host := filepath.Join(t.TempDir(), ".credentials.json")
	writeCreds(t, host, `{}`, time.Now())
	cases := []struct {
		name string
		kind environment.Kind
		env  map[string]string
	}{
		{"local", environment.KindLocal, nil},
		{"ssh", environment.KindSSH, nil},
		{"api key", environment.KindDocker, map[string]string{"ANTHROPIC_API_KEY": "sk"}},
		{"oauth token", environment.KindDocker, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01"}},
		{"bedrock", environment.KindDocker, map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := &recEnv{kind: c.kind}
			def := agent.Definition{Environment: environment.Spec{Env: c.env}}
			if hint := (&Agent{Credentials: host}).lendLogin(context.Background(), env, def); hint != "" {
				t.Errorf("hint = %q", hint)
			}
			if env.execs != 0 {
				t.Errorf("executed %d commands, want none", env.execs)
			}
		})
	}
}

func TestLendLoginWithoutHostLoginHints(t *testing.T) {
	env := &recEnv{kind: environment.KindDocker}
	a := &Agent{Credentials: filepath.Join(t.TempDir(), "missing.json")}
	hint := a.lendLogin(context.Background(), env, agent.Definition{})
	if !strings.Contains(hint, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("hint = %q", hint)
	}
	if env.execs != 0 {
		t.Errorf("executed %d commands, want none", env.execs)
	}
}

func TestNotLoggedInResultCarriesHint(t *testing.T) {
	f := newFakeProc()
	r := &run{proc: f, events: make(chan agent.Event, 64), pending: map[string]pendingPerm{}, done: make(chan struct{}), loginHint: noLoginHint}
	go r.loop()
	f.say(`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login","session_id":"s1"}`)
	f.exit()
	var got agent.Event
	for ev := range r.events {
		if ev.Type == agent.EventError {
			got = ev
		}
	}
	if !strings.HasPrefix(got.Text, "Not logged in") || !strings.Contains(got.Text, "host has none to lend") {
		t.Fatalf("error text = %q", got.Text)
	}
}
