package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/config"
	"github.com/cleanunicorn/dispatch/internal/decider"
	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/gh"
	slackt "github.com/cleanunicorn/dispatch/internal/transport/slack"
)

type check struct {
	name string
	ok   bool
	info string
	// note marks a passing check whose info still deserves a look
	// (an optional Slack scope is missing): printed ℹ instead of ✔.
	note bool
}

func (c check) String() string {
	mark := "✔"
	switch {
	case !c.ok:
		mark = "✘"
	case c.note:
		mark = "ℹ"
	}
	return fmt.Sprintf("  %s %-22s %s", mark, c.name, c.info)
}

// checkDecider reports the policy decider: off (dispatch's own rules decide)
// or the model and the question kinds it is allowed to answer. For openai
// it also proves the endpoint answers with the configured key — the same
// standard checkClaude holds the CLI to — since a decider that 401s on
// every question falls back to the rules with nothing but a warn log.
func checkDecider(cfg *config.Config) check {
	if !cfg.Decider.Enabled() {
		return check{name: "decider", ok: true, info: "off — dispatch's rules decide"}
	}
	if len(cfg.Decider.Uses) == 0 {
		return check{name: "decider", ok: false, info: fmt.Sprintf("kind %q but uses = [] — it is never asked anything", cfg.Decider.Kind)}
	}
	info := fmt.Sprintf("%s/%s for %v (timeout %s)", cfg.Decider.Kind, cfg.Decider.Model, cfg.Decider.Uses, cfg.Decider.Timeout.Duration)
	if cfg.Decider.Kind == "openai" {
		o := cfg.Decider.OpenAI
		info += " @ " + o.BaseURL
		if o.APIKey == "" {
			info += ", no api_key"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		d := decider.OpenAI{BaseURL: o.BaseURL, APIKey: o.APIKey, Model: cfg.Decider.Model}
		if err := d.Ping(ctx); err != nil {
			hint := ""
			if msg := err.Error(); strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403") {
				hint = " — check [decider.openai] api_key"
			}
			return check{name: "decider", ok: false, info: info + "; endpoint check failed: " + truncate(strings.TrimPrefix(err.Error(), "decider: openai: "), 160) + hint}
		}
		info += ", endpoint answers"
	}
	for _, u := range cfg.Decider.Uses {
		if u != "permission" {
			continue
		}
		if len(cfg.Decider.AutoAllow) == 0 {
			return check{name: "decider", ok: false, info: info + "; permission is listed but auto_allow is empty, so every prompt still asks"}
		}
		info += fmt.Sprintf(", may allow %v", cfg.Decider.AutoAllow)
	}
	return check{name: "decider", ok: true, info: info}
}

func runDoctor(cfgPath string) error {
	fmt.Println("dispatch doctor")
	var checks []check
	failed := false
	add := func(c check) {
		checks = append(checks, c)
		fmt.Println(c)
		if !c.ok {
			failed = true
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		add(check{name: "config", ok: false, info: err.Error()})
		fmt.Println("\nrun `dispatch setup` to create a config")
		return fmt.Errorf("doctor: config")
	}
	add(check{name: "config", ok: true, info: cfgPath})
	add(check{name: "workdir_root", ok: dirWritable(cfg.Server.WorkdirRoot), info: cfg.Server.WorkdirRoot})

	// Only the CLIs the definitions actually run are probed: an install
	// that runs nothing but codex should not be told claude is missing,
	// and checkClaude spends a model turn. A kind with no driver in this
	// build is reported per definition below, so its CLI is not looked
	// for either — one failure, one cause.
	drv := drivers(cfg)
	for _, k := range kindsUsed(cfg) {
		if _, ok := drv[k]; !ok {
			continue
		}
		if k == agent.KindClaude {
			add(checkClaude(cfg.Claude.Binary))
			continue
		}
		add(checkAgent(k, cfg.AgentBinary(k)))
	}
	for _, d := range cfg.Definitions {
		if _, ok := drv[agent.Kind(d.Kind)]; !ok {
			add(check{name: "definition " + d.Name, ok: false, info: fmt.Sprintf("agent kind %q has no driver in this build (available: %s)", d.Kind, kindList(drv))})
		}
	}
	add(checkDecider(cfg))

	needDocker, needSSH := false, false
	for _, d := range cfg.Definitions {
		switch environment.Kind(d.Environment.Kind) {
		case environment.KindDocker:
			needDocker = true
		case environment.KindSSH:
			needSSH = true
			add(checkSSH(d.Environment.Host, d.Environment.KeyPath, agent.Kind(d.Kind), cfg.AgentBinary(agent.Kind(d.Kind))))
		}
	}
	if needDocker {
		add(checkDocker())
		add(checkGitHub())
	}
	_ = needSSH

	for _, ch := range cfg.Server.Transports {
		if ch == "slack" {
			for _, c := range checkSlack(cfg.Slack.AppToken, cfg.Slack.BotToken) {
				add(c)
			}
		}
	}
	add(check{name: "definitions", ok: len(cfg.Definitions) > 0, info: fmt.Sprintf("%d configured (default: %s)", len(cfg.Definitions), cfg.Server.DefaultAgent)})
	add(check{name: "surfaces", ok: len(cfg.Surfaces) > 0, info: describeSurfaces(cfg)})

	if failed {
		return fmt.Errorf("doctor: %d check(s) failed", countFailed(checks))
	}
	fmt.Println("\nall checks passed")
	return nil
}

func countFailed(cs []check) int {
	n := 0
	for _, c := range cs {
		if !c.ok {
			n++
		}
	}
	return n
}

func dirWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func checkClaude(bin string) check {
	path, err := exec.LookPath(bin)
	if err != nil {
		return check{name: "claude", ok: false, info: bin + " not found in PATH"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ver, _ := exec.CommandContext(ctx, path, "--version").Output()
	// A tiny print-mode call proves auth without a real model turn.
	cmd := exec.CommandContext(ctx, path, "-p", "--model", "haiku", "--output-format", "json", "--max-turns", "1", "Reply with OK")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil || strings.Contains(out.String(), "Not logged in") {
		msg := strings.TrimSpace(errb.String() + out.String())
		if msg == "" {
			msg = err.Error()
		}
		return check{name: "claude", ok: false, info: fmt.Sprintf("%s — not authenticated? run `claude` once and /login (%s)", strings.TrimSpace(string(ver)), truncate(msg, 120))}
	}
	return check{name: "claude", ok: true, info: strings.TrimSpace(string(ver)) + " at " + path + ", authenticated"}
}

func checkDocker() check {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return check{name: "docker", ok: false, info: "docker daemon not reachable: " + err.Error()}
	}
	return check{name: "docker", ok: true, info: "server " + strings.TrimSpace(string(out))}
}

// checkGitHub reports what dispatch would lend a container to work on GitHub
// (internal/gh): the login, and the identity its commits would carry.
// Nothing here is fatal — an agent that never touches GitHub needs neither
// — so a gap is a note rather than a failed check.
func checkGitHub() check {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	login, loginErr := gh.HostLogin(ctx)
	id, idErr := gh.HostIdentity(ctx)

	who := "no git identity to lend — set user.email on this host, or `git commit` in a container stops"
	if idErr == nil {
		who = "committing as " + id.String()
	}
	if loginErr != nil {
		return check{name: "github", ok: true, note: true, info: "no login to lend — `gh` in a container stays logged out; " +
			"run `gh auth login` on this host, or set GH_TOKEN in the definition's environment env; " + who}
	}
	return check{name: "github", ok: true, note: idErr != nil,
		info: "lending the login from " + login.Source + " to containers, " + who}
}

// kindsUsed lists the agent kinds the definitions run, in agent.Kinds
// order, each once. A config with no definitions yet — someone running
// doctor to find out why setup will not work — is answered with the
// default kind, so there is always one CLI to report on.
func kindsUsed(cfg *config.Config) []agent.Kind {
	if len(cfg.Definitions) == 0 {
		return []agent.Kind{agent.KindClaude}
	}
	used := map[agent.Kind]bool{}
	for _, d := range cfg.Definitions {
		used[agent.Kind(d.Kind)] = true
	}
	var out []agent.Kind
	for _, k := range agent.Kinds() {
		if used[k] {
			out = append(out, k)
		}
	}
	return out
}

// checkAgent reports a non-claude agent CLI: on PATH, its version, and
// whether it has a login or a key to run with. It does not spend a model
// turn the way checkClaude does; a version call proves the install.
func checkAgent(kind agent.Kind, bin string) check {
	name := string(kind)
	path, err := exec.LookPath(bin)
	if err != nil {
		return check{name: name, ok: false, info: bin + " not found in PATH"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ver, _ := exec.CommandContext(ctx, path, "--version").Output()
	info := strings.TrimSpace(string(ver)) + " at " + path
	switch kind {
	case agent.KindCodex:
		// dispatch uses the stateful app-server protocol, not `codex exec`:
		// a version alone is not enough proof that this Codex install can
		// stream approvals and follow-up turns.
		if err := exec.CommandContext(ctx, path, "app-server", "--help").Run(); err != nil {
			return check{name: name, ok: false, info: info + " — Codex app-server is unavailable; upgrade Codex"}
		}
		for _, k := range []string{"OPENAI_API_KEY", "CODEX_API_KEY"} {
			if os.Getenv(k) != "" {
				return check{name: name, ok: true, info: info + ", " + k + " in the environment"}
			}
		}
		if f := codexAuthFile(); fileExists(f) {
			return check{name: name, ok: true, info: info + ", logged in (" + f + ")"}
		}
		return check{name: name, ok: false, info: info + " — not logged in: run `codex login`, or put OPENAI_API_KEY in the definition's environment env"}
	case agent.KindOpenCode:
		if f := opencodeAuthFile(); fileExists(f) {
			return check{name: name, ok: true, info: info + ", logged in (" + f + ")"}
		}
		return check{name: name, ok: true, note: true, info: info + " — no auth.json: the definition's environment env must carry the provider's key (e.g. ZHIPU_API_KEY)"}
	}
	return check{name: name, ok: true, info: info}
}

// codexAuthFile is where `codex login` keeps its credentials:
// $CODEX_HOME/auth.json, default ~/.codex/auth.json.
func codexAuthFile() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "auth.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

// opencodeAuthFile is where `opencode auth login` keeps its keys:
// $XDG_DATA_HOME/opencode/auth.json, default ~/.local/share/opencode/auth.json.
func opencodeAuthFile() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode", "auth.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "opencode", "auth.json")
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

func checkSSH(host, key string, kind agent.Kind, bin string) check {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if key != "" {
		args = append(args, "-i", key)
	}
	args = append(args, host, "--", "command -v "+bin+" && "+bin+" --version")
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		return check{name: "ssh " + host, ok: false, info: truncate(strings.TrimSpace(string(out)), 160)}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return check{name: "ssh " + host, ok: true, info: string(kind) + " " + lines[len(lines)-1]}
}

// checkSlack proves both tokens with auth.test and then compares the scopes
// each one carries (the X-OAuth-Scopes header, see slack.AuthScopes) with
// the ones dispatch uses. An app created before a scope was added to the
// manifest is accepted by auth.test and fails one feature at a time later,
// with a log line per call; listing the missing scopes here turns that into
// one line with the fix in it.
func checkSlack(appToken, botToken string) []check {
	if appToken == "" || botToken == "" {
		return []check{{name: "slack", info: "app_token and bot_token required"}}
	}
	if !strings.HasPrefix(appToken, "xapp-") || !strings.HasPrefix(botToken, "xoxb-") {
		return []check{{name: "slack", info: "app_token must start with xapp-, bot_token with xoxb-"}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bot, err := slackt.AuthScopes(ctx, botToken)
	if err != nil {
		return []check{{name: "slack", info: "bot_token: " + err.Error()}}
	}
	out := []check{
		{name: "slack", ok: true, info: fmt.Sprintf("bot @%s in %s", bot.User, bot.Team)},
		checkSlackScopes(bot.Scopes),
	}
	app, err := slackt.AuthScopes(ctx, appToken)
	switch {
	case err != nil:
		out = append(out, check{name: "slack app_token", info: err.Error()})
	case app.Scopes == nil:
		out = append(out, check{name: "slack app_token", ok: true, note: true, info: "accepted; auth.test sent no X-OAuth-Scopes header, so " + slackt.AppScope + " could not be verified"})
	case len(slackt.MissingScopes(app.Scopes, []string{slackt.AppScope})) > 0:
		out = append(out, check{name: "slack app_token", info: fmt.Sprintf("has %v, not %s — generate the token again under Basic Information → App-Level Tokens with that scope", app.Scopes, slackt.AppScope)})
	default:
		out = append(out, check{name: "slack app_token", ok: true, info: slackt.AppScope})
	}
	return out
}

// checkSlackScopes is the bot's scope line: ✘ with the required scopes the
// token lacks, ℹ when only optional ones are missing, ✔ otherwise.
func checkSlackScopes(have []string) check {
	const fix = " — add under OAuth & Permissions and reinstall the app (docs/slack.md#scopes)"
	if have == nil {
		return check{name: "slack scopes", ok: true, note: true, info: "auth.test sent no X-OAuth-Scopes header, so the scopes could not be verified"}
	}
	required := slackt.MissingScopes(have, slackt.RequiredScopes)
	optional := slackt.MissingScopes(have, slackt.OptionalScopes)
	if len(required) > 0 {
		info := "missing: " + strings.Join(required, ", ")
		if len(optional) > 0 {
			info += "; optional: " + strings.Join(optional, ", ")
		}
		return check{name: "slack scopes", info: info + fix}
	}
	if len(optional) > 0 {
		return check{name: "slack scopes", ok: true, note: true, info: "all required; optional missing: " + strings.Join(optional, ", ") + fix}
	}
	return check{name: "slack scopes", ok: true, info: fmt.Sprintf("all %d required and %d optional", len(slackt.RequiredScopes), len(slackt.OptionalScopes))}
}

func describeSurfaces(cfg *config.Config) string {
	var parts []string
	for _, s := range cfg.Surfaces {
		parts = append(parts, fmt.Sprintf("%s(%s on %s)", s.Name, s.Kind, s.Transport))
	}
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
