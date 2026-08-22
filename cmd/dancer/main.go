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
	"path/filepath"
	"syscall"
	"time"

	"github.com/cleanunicorn/dancer/internal/agent"
	agentclaude "github.com/cleanunicorn/dancer/internal/agent/claude"
	"github.com/cleanunicorn/dancer/internal/config"
	"github.com/cleanunicorn/dancer/internal/coordinator"
	"github.com/cleanunicorn/dancer/internal/decider"
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
	unlock, err := lockInstance(cfg.Server.DB)
	if err != nil {
		return err
	}
	defer unlock()

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

	dockerFactory := envdocker.Factory{
		Binary:       cfg.Docker.Binary,
		ExtraRunArgs: cfg.Docker.RunArgs,
		StateDir:     filepath.Join(filepath.Dir(cfg.Server.DB), "docker"),
	}

	ex := execlocal.New(
		map[agent.Kind]agent.Agent{agent.KindClaude: &agentclaude.Agent{Binary: cfg.Claude.Binary}},
		map[environment.Kind]environment.Factory{
			environment.KindLocal:  envlocal.Factory{},
			environment.KindDocker: dockerFactory,
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
	if cfg.Decider.Enabled() {
		c.Decider = decider.Claude{Binary: cfg.Claude.Binary, Model: cfg.Decider.Model, Timeout: cfg.Decider.Timeout.Duration}
		c.DeciderUses = cfg.Decider.Uses
		c.DeciderTimeout = cfg.Decider.Timeout.Duration
		c.MaxDecisionsPerTask = cfg.Decider.MaxPerTask
		c.AutoAllow = cfg.Decider.AutoAllow
	}
	c.AutoResume = cfg.Server.AutoResume == nil || *cfg.Server.AutoResume
	c.ResumePrompt = cfg.Server.ResumePrompt
	c.AutoResumeWithin = cfg.Server.AutoResumeWithin.Duration
	c.MaxAutoResumes = cfg.Server.MaxAutoResumes
	c.SaveDefinition = func(_ context.Context, d agent.Definition) error {
		return config.AppendDefinition(cfgPath, config.DefinitionFromAgent(d))
	}
	c.UpdateDefinition = func(_ context.Context, d agent.Definition) error {
		return config.ReplaceDefinition(cfgPath, config.DefinitionFromAgent(d))
	}
	c.DeleteDefinition = func(_ context.Context, name string) error {
		// A definition only the store knows (removed from the file by hand)
		// has nothing to delete there.
		if err := config.RemoveDefinition(cfgPath, name); err != nil && !errors.Is(err, config.ErrNoDefinition) {
			return err
		}
		return nil
	}
	go reapContainers(ctx, dockerFactory, cfg.Docker.ReuseTTL.Duration, log)

	log.Info("dancer starting", "config", cfgPath, "db", cfg.Server.DB, "transports", transportNames, "surfaces", len(surfaces), "definitions", len(cfg.Definitions), "auto_resume", c.AutoResume, "decider", cfg.Decider.Kind)
	err = c.Run(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("dancer stopped")
		return nil
	}
	return err
}

// reapContainers retires reused containers nobody has touched for a while.
// It runs once at startup — containers outlive the process, so a restart is
// the first chance to notice one has gone cold — and hourly after that.
func reapContainers(ctx context.Context, f envdocker.Factory, ttl time.Duration, log *slog.Logger) {
	if ttl <= 0 {
		ttl = envdocker.DefaultReuseTTL
	}
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		if err := f.Reap(ctx, ttl); err != nil && ctx.Err() == nil {
			log.Debug("docker reap", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
