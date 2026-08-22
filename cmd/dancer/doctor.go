package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/cleanunicorn/dancer/internal/config"
	"github.com/cleanunicorn/dancer/internal/environment"
)

type check struct {
	name string
	ok   bool
	info string
}

func (c check) String() string {
	mark := "✔"
	if !c.ok {
		mark = "✘"
	}
	return fmt.Sprintf("  %s %-22s %s", mark, c.name, c.info)
}

// checkDecider reports the policy decider: off (dancer's own rules decide)
// or the model and the question kinds it is allowed to answer.
func checkDecider(cfg *config.Config) check {
	if !cfg.Decider.Enabled() {
		return check{"decider", true, "off — dancer's rules decide"}
	}
	if len(cfg.Decider.Uses) == 0 {
		return check{"decider", false, fmt.Sprintf("kind %q but uses = [] — it is never asked anything", cfg.Decider.Kind)}
	}
	info := fmt.Sprintf("%s/%s for %v (timeout %s)", cfg.Decider.Kind, cfg.Decider.Model, cfg.Decider.Uses, cfg.Decider.Timeout.Duration)
	for _, u := range cfg.Decider.Uses {
		if u != "permission" {
			continue
		}
		if len(cfg.Decider.AutoAllow) == 0 {
			return check{"decider", false, info + "; permission is listed but auto_allow is empty, so every prompt still asks"}
		}
		info += fmt.Sprintf(", may allow %v", cfg.Decider.AutoAllow)
	}
	return check{"decider", true, info}
}

func runDoctor(cfgPath string) error {
	fmt.Println("dancer doctor")
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
		add(check{"config", false, err.Error()})
		fmt.Println("\nrun `dancer setup` to create a config")
		return fmt.Errorf("doctor: config")
	}
	add(check{"config", true, cfgPath})
	add(check{"workdir_root", dirWritable(cfg.Server.WorkdirRoot), cfg.Server.WorkdirRoot})

	add(checkClaude(cfg.Claude.Binary))
	add(checkDecider(cfg))

	needDocker, needSSH := false, false
	for _, d := range cfg.Definitions {
		switch environment.Kind(d.Environment.Kind) {
		case environment.KindDocker:
			needDocker = true
		case environment.KindSSH:
			needSSH = true
			add(checkSSH(d.Environment.Host, d.Environment.KeyPath, cfg.Claude.Binary))
		}
	}
	if needDocker {
		add(checkDocker())
	}
	_ = needSSH

	for _, ch := range cfg.Server.Transports {
		if ch == "slack" {
			add(checkSlack(cfg.Slack.AppToken, cfg.Slack.BotToken))
		}
	}
	add(check{"definitions", len(cfg.Definitions) > 0, fmt.Sprintf("%d configured (default: %s)", len(cfg.Definitions), cfg.Server.DefaultAgent)})
	add(check{"surfaces", len(cfg.Surfaces) > 0, describeSurfaces(cfg)})

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
		return check{"claude", false, bin + " not found in PATH"}
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
		return check{"claude", false, fmt.Sprintf("%s — not authenticated? run `claude` once and /login (%s)", strings.TrimSpace(string(ver)), truncate(msg, 120))}
	}
	return check{"claude", true, strings.TrimSpace(string(ver)) + " at " + path + ", authenticated"}
}

func checkDocker() check {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return check{"docker", false, "docker daemon not reachable: " + err.Error()}
	}
	return check{"docker", true, "server " + strings.TrimSpace(string(out))}
}

func checkSSH(host, key, claudeBin string) check {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if key != "" {
		args = append(args, "-i", key)
	}
	args = append(args, host, "--", "command -v "+claudeBin+" && "+claudeBin+" --version")
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		return check{"ssh " + host, false, truncate(strings.TrimSpace(string(out)), 160)}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return check{"ssh " + host, true, "claude " + lines[len(lines)-1]}
}

func checkSlack(appToken, botToken string) check {
	if appToken == "" || botToken == "" {
		return check{"slack", false, "app_token and bot_token required"}
	}
	if !strings.HasPrefix(appToken, "xapp-") || !strings.HasPrefix(botToken, "xoxb-") {
		return check{"slack", false, "app_token must start with xapp-, bot_token with xoxb-"}
	}
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	auth, err := api.AuthTest()
	if err != nil {
		return check{"slack", false, "auth.test: " + err.Error()}
	}
	return check{"slack", true, fmt.Sprintf("bot @%s in %s", auth.User, auth.Team)}
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
