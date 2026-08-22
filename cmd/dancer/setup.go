package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cleanunicorn/dancer/internal/config"
)

// runSetup is the interactive first-time wizard. It writes the config file
// and ends by running doctor.
func runSetup(cfgPath string) error {
	in := bufio.NewReader(os.Stdin)
	ask := func(label, def string) string {
		if def != "" {
			fmt.Printf("%s [%s]: ", label, def)
		} else {
			fmt.Printf("%s: ", label)
		}
		line, _ := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		return line
	}
	yes := func(label string, def bool) bool {
		d := "y/N"
		if def {
			d = "Y/n"
		}
		a := strings.ToLower(ask(label, d))
		if a == "y/n" {
			return def
		}
		return strings.HasPrefix(a, "y")
	}

	fmt.Println("dancer setup")
	fmt.Println("------------")
	if _, err := os.Stat(cfgPath); err == nil {
		if !yes(fmt.Sprintf("%s exists — overwrite?", cfgPath), false) {
			return fmt.Errorf("setup: aborted")
		}
	}
	base := filepath.Dir(cfgPath)
	cfg := &config.Config{}

	fmt.Println("\n1/4  Storage")
	cfg.Server.DB = ask("SQLite database path", filepath.Join(base, "dancer.db"))
	cfg.Server.WorkdirRoot = ask("Root for per-task working directories", filepath.Join(base, "work"))
	cfg.Server.IdleTimeout = config.Duration{Duration: 10 * time.Minute}
	cfg.Server.DrainTimeout = config.Duration{Duration: 2 * time.Minute}

	fmt.Println("\n2/4  Claude Code")
	cfg.Claude.Binary = ask("claude binary", "claude")
	if _, err := exec.LookPath(cfg.Claude.Binary); err != nil {
		fmt.Printf("  ! %s not found in PATH — install Claude Code first: https://code.claude.com/docs/en/setup\n", cfg.Claude.Binary)
	} else {
		fmt.Println("  ✔ found. Make sure it is logged in (run `claude` once, then /login) under the user that will run dancer.")
	}

	fmt.Println("\n3/4  Slack (Socket Mode)")
	fmt.Println("  Create the app from deploy/slack-manifest.yaml (see SETUP.md), then paste the tokens.")
	if yes("Configure Slack now?", true) {
		cfg.Slack.AppToken = ask("App-level token (xapp-…)", "")
		cfg.Slack.BotToken = ask("Bot token (xoxb-…)", "")
		if u := ask("Allowed Slack user IDs, comma-separated (empty = everyone)", ""); u != "" {
			for _, s := range strings.Split(u, ",") {
				if s = strings.TrimSpace(s); s != "" {
					cfg.Slack.AllowedUsers = append(cfg.Slack.AllowedUsers, s)
				}
			}
		}
		cfg.Server.Transports = []string{"slack"}
	} else {
		cfg.Server.Transports = []string{"terminal"}
		fmt.Println("  Using the terminal transport; add [slack] to the config later.")
	}

	fmt.Println("\n4/4  First agent definition")
	name := ask("Agent name", "coder")
	model := ask("Model (haiku/sonnet/opus/fable or full id)", "sonnet")
	envKind := strings.ToLower(ask("Environment kind (local/docker/ssh)", "local"))
	def := config.Definition{Name: name, Kind: "claude", Model: model}
	def.Environment.Kind = envKind
	switch envKind {
	case "docker":
		def.Environment.Image = ask("Docker image with claude installed", "")
		def.Environment.Workdir = ask("Host directory to mount at /work (empty = per-task dir)", "")
		def.PermissionMode = ask("Permission mode", "acceptEdits")
	case "ssh":
		def.Environment.Host = ask("SSH host (user@host or ssh-config alias)", "")
		def.Environment.KeyPath = ask("Private key path (empty = ssh agent/config)", "")
		def.Environment.Workdir = ask("Remote working directory", "")
		def.PermissionMode = ask("Permission mode", "manual")
	default:
		def.Environment.Kind = "local"
		def.Environment.Workdir = ask("Working directory (empty = per-task dir under workdir_root)", "")
		def.PermissionMode = ask("Permission mode (manual/acceptEdits/auto/bypassPermissions)", "manual")
	}
	if tools := ask("Pre-approved tools, comma-separated (e.g. Read,Edit,Bash(git:*))", "Read,Glob,Grep"); tools != "" {
		for _, t := range strings.Split(tools, ",") {
			if t = strings.TrimSpace(t); t != "" {
				def.AllowedTools = append(def.AllowedTools, t)
			}
		}
	}
	def.SystemPrompt = ask("Extra system prompt (optional)", "")
	cfg.Definitions = []config.Definition{def}
	cfg.Server.DefaultAgent = name

	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s\n\n", cfgPath)
	if err := runDoctor(cfgPath); err != nil {
		fmt.Println("\nfix the failed checks, then run `dancer doctor` again")
		return nil
	}
	fmt.Println("\nnext: `dancer run` (or `dancer run -terminal` to try it in the terminal)")
	return nil
}
