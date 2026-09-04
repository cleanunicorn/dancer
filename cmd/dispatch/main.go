// Command dispatch is the agent orchestration server.
//
//	dispatch run    [-config path] [-terminal] [-web]   start the coordinator (default)
//	dispatch setup  [-config path]               interactive first-time setup
//	dispatch doctor [-config path]               check config, agent CLIs, docker, ssh, slack
//	dispatch user   add|passwd|rm|list [name]    accounts of the web UI
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
	"strings"
	"syscall"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	agentclaude "github.com/cleanunicorn/dispatch/internal/agent/claude"
	agentcodex "github.com/cleanunicorn/dispatch/internal/agent/codex"
	"github.com/cleanunicorn/dispatch/internal/config"
	"github.com/cleanunicorn/dispatch/internal/coordinator"
	"github.com/cleanunicorn/dispatch/internal/decider"
	"github.com/cleanunicorn/dispatch/internal/environment"
	envdocker "github.com/cleanunicorn/dispatch/internal/environment/docker"
	envlocal "github.com/cleanunicorn/dispatch/internal/environment/local"
	envssh "github.com/cleanunicorn/dispatch/internal/environment/ssh"
	execlocal "github.com/cleanunicorn/dispatch/internal/executor/local"
	"github.com/cleanunicorn/dispatch/internal/store/sqlite"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/surface/chat"
	"github.com/cleanunicorn/dispatch/internal/surface/feed"
	"github.com/cleanunicorn/dispatch/internal/transport"
	trslack "github.com/cleanunicorn/dispatch/internal/transport/slack"
	"github.com/cleanunicorn/dispatch/internal/transport/terminal"
	trweb "github.com/cleanunicorn/dispatch/internal/transport/web"
)

func main() {
	sub := "run"
	args := os.Args[1:]
	if len(args) > 0 && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("dispatch "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file (or $DISPATCH_CONFIG)")
	termFlag := fs.Bool("terminal", false, "run: use the terminal transport with a chat surface instead of the configured ones")
	webFlag := fs.Bool("web", false, "run: use the web transport (the browser UI) instead of the configured ones")
	fs.Parse(args)

	var err error
	switch sub {
	case "run":
		err = runServer(*cfgPath, *termFlag, *webFlag)
	case "setup":
		err = runSetup(*cfgPath)
	case "doctor":
		err = runDoctor(*cfgPath)
	case "user":
		err = runUser(*cfgPath, fs.Args())
	case "help", "-h", "--help":
		fmt.Println("usage: dispatch [run|setup|doctor|user] [-config path] [-terminal|-web]")
	default:
		err = fmt.Errorf("unknown command %q (run|setup|doctor|user)", sub)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatch:", err)
		os.Exit(1)
	}
}

func runServer(cfgPath string, forceTerminal, forceWeb bool) error {
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

	agents := drivers(cfg)
	for _, d := range cfg.Definitions {
		if _, ok := agents[agent.Kind(d.Kind)]; !ok {
			return fmt.Errorf("definition %q: agent kind %q has no driver in this build (available: %s)", d.Name, d.Kind, kindList(agents))
		}
	}
	ex := execlocal.New(
		agents,
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
	if forceWeb {
		if err := cfg.CheckWeb(); err != nil {
			return err
		}
		transportNames = []string{"web"}
		surfaceCfgs = []config.Surface{{Name: "chat-web", Kind: "chat", Transport: "web", Verbose: cfg.Server.Verbose}}
	}
	var transports []transport.Transport
	var web *trweb.Transport
	for _, name := range transportNames {
		switch name {
		case "terminal":
			transports = append(transports, terminal.New())
		case "slack":
			sc, err := trslack.New(cfg.Slack.AppToken, cfg.Slack.BotToken, cfg.Slack.AllowedUsers, log)
			if err != nil {
				return err
			}
			for _, ch := range cfg.Channels {
				if ch.Transport == "" || ch.Transport == "slack" {
					sc.KnownChannels = append(sc.KnownChannels, ch.ID)
				}
			}
			transports = append(transports, sc)
		case "web":
			web = trweb.New(cfg.Web.Listen, cfg.Web.Channels, st, log)
			transports = append(transports, web)
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
	if web != nil {
		web.History = c // the lists and the past come from the log, not from the browser's memory
	}
	c.DefaultDefinition = cfg.Server.DefaultAgent
	c.AgentKinds = registeredKinds(agents)
	c.ChannelAgents = cfg.ChannelAgents()
	c.SaveChannelAgent = func(_ context.Context, transportName, channel, agent string) error {
		return config.AppendChannel(cfgPath, config.Channel{Transport: transportName, ID: channel, Agent: agent})
	}
	c.WorkdirRoot = cfg.Server.WorkdirRoot
	c.DrainTimeout = cfg.Server.DrainTimeout.Duration
	if cfg.Decider.Enabled() {
		switch cfg.Decider.Kind {
		case "claude":
			c.Decider = decider.Claude{Binary: cfg.Claude.Binary, Model: cfg.Decider.Model, Timeout: cfg.Decider.Timeout.Duration}
		case "openai":
			c.Decider = decider.OpenAI{BaseURL: cfg.Decider.OpenAI.BaseURL, APIKey: cfg.Decider.OpenAI.APIKey,
				Model: cfg.Decider.Model, Timeout: cfg.Decider.Timeout.Duration}
		default:
			return fmt.Errorf("unknown decider kind %q", cfg.Decider.Kind)
		}
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

	log.Info("dispatch starting", "config", cfgPath, "db", cfg.Server.DB, "transports", transportNames, "surfaces", len(surfaces), "definitions", len(cfg.Definitions), "auto_resume", c.AutoResume, "decider", cfg.Decider.Kind)
	err = c.Run(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("dispatch stopped")
		return nil
	}
	return err
}

// drivers is the agent registry: every kind this build can run, keyed by
// the definition kind that selects it. Config accepts every kind in
// agent.Kinds; a definition whose kind is missing here is refused at
// startup with this list, not at its first task.
func drivers(cfg *config.Config) map[agent.Kind]agent.Agent {
	return map[agent.Kind]agent.Agent{
		agent.KindClaude: &agentclaude.Agent{Binary: cfg.Claude.Binary},
		agent.KindCodex:  &agentcodex.Agent{Binary: cfg.Codex.Binary},
		// opencode driver: issue #46.
	}
}

// registeredKinds lists the kinds in agents, in agent.Kinds order.
func registeredKinds(agents map[agent.Kind]agent.Agent) []agent.Kind {
	var out []agent.Kind
	for _, k := range agent.Kinds() {
		if _, ok := agents[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// kindList is registeredKinds as an error message lists them.
func kindList(agents map[agent.Kind]agent.Agent) string {
	var names []string
	for _, k := range registeredKinds(agents) {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
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
