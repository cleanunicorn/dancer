// Command dancer is the agent orchestration server.
//
//	dancer run    [-config path] [-terminal]   start the coordinator (default)
//	dancer setup  [-config path]               interactive first-time setup
//	dancer doctor [-config path]               check config, claude, docker, ssh, slack
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cleanunicorn/dancer/internal/agent"
	agentclaude "github.com/cleanunicorn/dancer/internal/agent/claude"
	"github.com/cleanunicorn/dancer/internal/config"
	"github.com/cleanunicorn/dancer/internal/coordinator"
	"github.com/cleanunicorn/dancer/internal/environment"
	envdocker "github.com/cleanunicorn/dancer/internal/environment/docker"
	envlocal "github.com/cleanunicorn/dancer/internal/environment/local"
	envssh "github.com/cleanunicorn/dancer/internal/environment/ssh"
	execlocal "github.com/cleanunicorn/dancer/internal/executor/local"
	"github.com/cleanunicorn/dancer/internal/store/sqlite"
	"github.com/cleanunicorn/dancer/internal/surface"
	"github.com/cleanunicorn/dancer/internal/surface/chat"
	"github.com/cleanunicorn/dancer/internal/surface/feed"
	"github.com/cleanunicorn/dancer/internal/transport"
	trslack "github.com/cleanunicorn/dancer/internal/transport/slack"
	"github.com/cleanunicorn/dancer/internal/transport/terminal"
)

func main() {
	sub := "run"
	args := os.Args[1:]
	if len(args) > 0 && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("dancer "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file (or $DANCER_CONFIG)")
	termFlag := fs.Bool("terminal", false, "run: use the terminal transport with a chat surface instead of the configured ones")
	fs.Parse(args)

	var err error
	switch sub {
	case "run":
		err = runServer(*cfgPath, *termFlag)
	case "setup":
		err = runSetup(*cfgPath)
	case "doctor":
		err = runDoctor(*cfgPath)
	case "help", "-h", "--help":
		fmt.Println("usage: dancer [run|setup|doctor] [-config path] [-terminal]")
	default:
		err = fmt.Errorf("unknown command %q (run|setup|doctor)", sub)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dancer:", err)
		os.Exit(1)
	}
}

func runServer(cfgPath string, forceTerminal bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := os.MkdirAll(cfg.Server.WorkdirRoot, 0o755); err != nil {
		return err
	}
	st, err := sqlite.Open(cfg.Server.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for _, d := range cfg.AgentDefinitions() {
		if err := st.PutDefinition(ctx, d); err != nil {
			return fmt.Errorf("seed definition %s: %w", d.Name, err)
		}
	}

	ex := execlocal.New(
		map[agent.Kind]agent.Agent{agent.KindClaude: &agentclaude.Agent{Binary: cfg.Claude.Binary}},
		map[environment.Kind]environment.Factory{
			environment.KindLocal:  envlocal.Factory{},
			environment.KindDocker: envdocker.Factory{},
			environment.KindSSH:    envssh.Factory{},
		},
		cfg.Server.IdleTimeout.Duration,
	)
	ex.DrainTimeout = cfg.Server.DrainTimeout.Duration

	transportNames := cfg.Server.Transports
	surfaceCfgs := cfg.Surfaces
	if forceTerminal {
		transportNames = []string{"terminal"}
		surfaceCfgs = []config.Surface{{Name: "chat-terminal", Kind: "chat", Transport: "terminal", Verbose: cfg.Server.Verbose}}
	}
	var transports []transport.Transport
	for _, name := range transportNames {
		switch name {
		case "terminal":
			transports = append(transports, terminal.New())
		case "slack":
			sc, err := trslack.New(cfg.Slack.AppToken, cfg.Slack.BotToken, cfg.Slack.AllowedUsers, log)
			if err != nil {
				return err
			}
			transports = append(transports, sc)
		default:
			return fmt.Errorf("unknown transport %q", name)
		}
	}
	var surfaces []surface.Surface
	for _, s := range surfaceCfgs {
		switch s.Kind {
		case "chat":
			surfaces = append(surfaces, chat.New(s.Name, s.Transport, s.Verbose || cfg.Server.Verbose))
		case "feed":
			surfaces = append(surfaces, feed.New(s.Name, s.Transport, transport.ThreadID(s.Thread), s.Approvals))
		default:
			return fmt.Errorf("unknown surface kind %q", s.Kind)
		}
	}

	c := coordinator.New(st, ex, transports, surfaces, log)
	c.DefaultDefinition = cfg.Server.DefaultAgent
	c.ChannelAgents = cfg.ChannelAgents()
	c.SaveChannelAgent = func(_ context.Context, transportName, channel, agent string) error {
		return config.AppendChannel(cfgPath, config.Channel{Transport: transportName, ID: channel, Agent: agent})
	}
	c.WorkdirRoot = cfg.Server.WorkdirRoot
	c.DrainTimeout = cfg.Server.DrainTimeout.Duration
	c.SaveDefinition = func(_ context.Context, d agent.Definition) error {
		return config.AppendDefinition(cfgPath, config.DefinitionFromAgent(d))
	}
	log.Info("dancer starting", "config", cfgPath, "db", cfg.Server.DB, "transports", transportNames, "surfaces", len(surfaces), "definitions", len(cfg.Definitions))
	err = c.Run(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("dancer stopped")
		return nil
	}
	return err
}
