// Package config loads the dancer TOML configuration.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
)

// Config is the on-disk configuration.
type Config struct {
	Server      Server       `toml:"server"`
	Claude      Claude       `toml:"claude"`
	Decider     Decider      `toml:"decider"`
	Slack       Slack        `toml:"slack"`
	Surfaces    []Surface    `toml:"surfaces"`
	Channels    []Channel    `toml:"channels"`
	Definitions []Definition `toml:"definitions"`
}

// Channel sets per-channel defaults: messages in that channel without an
// agent name run Agent instead of server.default_agent. Several entries for
// one channel are allowed; the last one wins, so `default <agent>` in chat
// can append rather than rewrite the file.
type Channel struct {
	Transport string `toml:"transport,omitempty"` // defaults to "slack"
	ID        string `toml:"id"`                  // Slack channel id (C0123…)
	Agent     string `toml:"agent"`
}

// Key identifies the channel across transports: "<transport>/<id>".
func (ch Channel) Key() string { return ch.Transport + "/" + ch.ID }

// Surface binds an interaction style to a transport. Several surfaces may
// share one transport.
type Surface struct {
	Name      string `toml:"name"`
	Kind      string `toml:"kind"`      // "chat" | "feed"
	Transport string `toml:"transport"` // "slack" | "terminal"
	// Thread is the fixed thread a feed posts to (slack: "<channel_id>/").
	Thread string `toml:"thread"`
	// Approvals: feed also posts permission prompts with buttons.
	Approvals bool `toml:"approvals"`
	// Verbose: chat also posts tool calls.
	Verbose bool `toml:"verbose"`
}

type Server struct {
	DB          string   `toml:"db"`
	WorkdirRoot string   `toml:"workdir_root"`
	IdleTimeout Duration `toml:"idle_timeout"`
	// DrainTimeout: on shutdown, how long to let in-flight tool calls finish.
	DrainTimeout Duration `toml:"drain_timeout"`
	Verbose      bool     `toml:"verbose"`
	// DefaultAgent is used by `run <prompt>` without an agent name.
	DefaultAgent string `toml:"default_agent"`
	// Transports to start: "slack", "terminal". Defaults to slack when
	// tokens are set, else terminal.
	Transports []string `toml:"transports"`
	// AutoResume continues tasks that a restart cut short as soon as dancer
	// is back, instead of waiting for a message on their thread. Defaults
	// to true; set `auto_resume = false` to go back to waiting.
	AutoResume *bool `toml:"auto_resume,omitempty"`
	// ResumePrompt is the turn given to an auto-resumed session. Empty uses
	// the built-in one.
	ResumePrompt string `toml:"resume_prompt,omitempty"`
	// AutoResumeWithin skips tasks last touched longer ago than this, so a
	// restart after a long stop does not relaunch stale work (default 12h).
	AutoResumeWithin Duration `toml:"auto_resume_within,omitempty"`
	// MaxAutoResumes caps consecutive automatic resumes of one task, so a
	// task that keeps taking dancer down cannot restart-loop (default 3).
	MaxAutoResumes int `toml:"max_auto_resumes,omitempty"`
}

type Claude struct {
	Binary string `toml:"binary"`
}

// Decider configures the small model that answers dancer's policy
// questions (see DECIDER.md). It is off by default: dancer's own rules
// answer everything, which is also the fallback whenever the decider
// fails, times out or answers something unacceptable.
type Decider struct {
	Kind  string `toml:"kind"`  // "off" (default) | "claude"
	Model string `toml:"model"` // default "haiku"
	// Uses lists the question kinds the decider may answer ("resume").
	// Empty means none, so switching it on is deliberate per kind.
	Uses       []string `toml:"uses"`
	Timeout    Duration `toml:"timeout"`
	MaxPerTask int      `toml:"max_per_task"`
}

// Enabled reports whether a decider other than the built-in rules is set.
func (d Decider) Enabled() bool { return d.Kind != "" && d.Kind != "off" }

type Slack struct {
	AppToken string `toml:"app_token"`
	BotToken string `toml:"bot_token"`
	// AllowedUsers restricts who may issue commands (Slack user IDs). Empty = anyone in the workspace.
	AllowedUsers []string `toml:"allowed_users"`
}

type Definition struct {
	Name           string            `toml:"name"`
	Kind           string            `toml:"kind"`
	Model          string            `toml:"model,omitempty"`
	SystemPrompt   string            `toml:"system_prompt,omitempty"`
	AllowedTools   []string          `toml:"allowed_tools,omitempty"`
	PermissionMode string            `toml:"permission_mode,omitempty"`
	MCPConfig      string            `toml:"mcp_config,omitempty"`
	SubAgents      map[string]any    `toml:"sub_agents,omitempty"`
	Environment    EnvironmentConfig `toml:"environment"`
}

type EnvironmentConfig struct {
	Kind    string            `toml:"kind"`
	Workdir string            `toml:"workdir,omitempty"`
	Image   string            `toml:"image,omitempty"`
	Host    string            `toml:"host,omitempty"`
	KeyPath string            `toml:"key_path,omitempty"`
	Env     map[string]string `toml:"env,omitempty"`
}

// Duration is a TOML-friendly time.Duration ("10m").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.Duration.String()), nil }

// DefaultPath returns the config path: $DANCER_CONFIG, else
// ~/.config/dancer/config.toml.
func DefaultPath() string {
	if p := os.Getenv("DANCER_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "dancer", "config.toml")
}

// Load reads and validates the config at path.
func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.applyDefaults(path)
	return &c, c.validate()
}

func (c *Config) applyDefaults(path string) {
	base := filepath.Dir(path)
	if c.Server.DB == "" {
		c.Server.DB = filepath.Join(base, "dancer.db")
	}
	if c.Server.WorkdirRoot == "" {
		c.Server.WorkdirRoot = filepath.Join(base, "work")
	}
	if c.Server.IdleTimeout.Duration == 0 {
		c.Server.IdleTimeout.Duration = 10 * time.Minute
	}
	if c.Server.DrainTimeout.Duration == 0 {
		c.Server.DrainTimeout.Duration = 2 * time.Minute
	}
	if c.Server.AutoResume == nil {
		on := true
		c.Server.AutoResume = &on
	}
	if c.Server.AutoResumeWithin.Duration == 0 {
		c.Server.AutoResumeWithin.Duration = 12 * time.Hour
	}
	if c.Server.MaxAutoResumes == 0 {
		c.Server.MaxAutoResumes = 3
	}
	if c.Claude.Binary == "" {
		c.Claude.Binary = "claude"
	}
	if c.Decider.Kind == "" {
		c.Decider.Kind = "off"
	}
	if c.Decider.Model == "" {
		c.Decider.Model = "haiku"
	}
	if c.Decider.Timeout.Duration == 0 {
		c.Decider.Timeout.Duration = 15 * time.Second
	}
	if c.Decider.MaxPerTask == 0 {
		c.Decider.MaxPerTask = 20
	}
	if len(c.Server.Transports) == 0 {
		if c.Slack.AppToken != "" {
			c.Server.Transports = []string{"slack"}
		} else {
			c.Server.Transports = []string{"terminal"}
		}
	}
	if len(c.Surfaces) == 0 {
		// One chat surface per transport.
		for _, t := range c.Server.Transports {
			c.Surfaces = append(c.Surfaces, Surface{Name: "chat-" + t, Kind: "chat", Transport: t, Verbose: c.Server.Verbose})
		}
	}
	for i := range c.Surfaces {
		if c.Surfaces[i].Kind == "" {
			c.Surfaces[i].Kind = "chat"
		}
		if c.Surfaces[i].Name == "" {
			c.Surfaces[i].Name = c.Surfaces[i].Kind + "-" + c.Surfaces[i].Transport
		}
	}
	for i := range c.Definitions {
		d := &c.Definitions[i]
		if d.Kind == "" {
			d.Kind = string(agent.KindClaude)
		}
		if d.Environment.Kind == "" {
			d.Environment.Kind = string(environment.KindLocal)
		}
		if d.PermissionMode == "" {
			switch environment.Kind(d.Environment.Kind) {
			case environment.KindDocker:
				d.PermissionMode = string(agent.PermissionAcceptEdits)
			default:
				d.PermissionMode = string(agent.PermissionManual)
			}
		}
	}
	if c.Server.DefaultAgent == "" && len(c.Definitions) > 0 {
		c.Server.DefaultAgent = c.Definitions[0].Name
	}
	for i := range c.Channels {
		if c.Channels[i].Transport == "" {
			c.Channels[i].Transport = "slack"
		}
	}
}

// ChannelAgents returns the per-channel default agents keyed by
// Channel.Key(); later entries override earlier ones.
func (c *Config) ChannelAgents() map[string]string {
	out := map[string]string{}
	for _, ch := range c.Channels {
		out[ch.Key()] = ch.Agent
	}
	return out
}

func (c *Config) validate() error {
	switch c.Decider.Kind {
	case "off", "claude":
	default:
		return fmt.Errorf("config: unknown decider kind %q (off|claude)", c.Decider.Kind)
	}
	for _, u := range c.Decider.Uses {
		if u != "resume" {
			return fmt.Errorf("config: decider cannot answer %q yet (resume)", u)
		}
	}
	seen := map[string]bool{}
	for _, d := range c.Definitions {
		if d.Name == "" {
			return fmt.Errorf("config: definition without name")
		}
		if seen[d.Name] {
			return fmt.Errorf("config: duplicate definition %q", d.Name)
		}
		seen[d.Name] = true
		switch environment.Kind(d.Environment.Kind) {
		case environment.KindLocal:
		case environment.KindDocker:
			if d.Environment.Image == "" {
				return fmt.Errorf("config: definition %q: docker environment needs image", d.Name)
			}
		case environment.KindSSH:
			if d.Environment.Host == "" {
				return fmt.Errorf("config: definition %q: ssh environment needs host", d.Name)
			}
		default:
			return fmt.Errorf("config: definition %q: unknown environment kind %q", d.Name, d.Environment.Kind)
		}
	}
	for _, ch := range c.Channels {
		if ch.ID == "" {
			return fmt.Errorf("config: channel without id")
		}
		if !seen[ch.Agent] {
			return fmt.Errorf("config: channel %q: unknown agent %q", ch.ID, ch.Agent)
		}
	}
	transports := map[string]bool{}
	for _, t := range c.Server.Transports {
		switch t {
		case "slack":
			if c.Slack.AppToken == "" || c.Slack.BotToken == "" {
				return fmt.Errorf("config: slack transport needs slack.app_token and slack.bot_token")
			}
		case "terminal":
		default:
			return fmt.Errorf("config: unknown transport %q", t)
		}
		transports[t] = true
	}
	names := map[string]bool{}
	for _, s := range c.Surfaces {
		if names[s.Name] {
			return fmt.Errorf("config: duplicate surface %q", s.Name)
		}
		names[s.Name] = true
		if !transports[s.Transport] {
			return fmt.Errorf("config: surface %q uses transport %q which is not in server.transports", s.Name, s.Transport)
		}
		switch s.Kind {
		case "chat":
		case "feed":
			if s.Thread == "" {
				return fmt.Errorf("config: feed surface %q needs thread", s.Name)
			}
		default:
			return fmt.Errorf("config: surface %q: unknown kind %q", s.Name, s.Kind)
		}
	}
	return nil
}

// AgentDefinitions converts config definitions to agent.Definition.
func (c *Config) AgentDefinitions() []agent.Definition {
	out := make([]agent.Definition, 0, len(c.Definitions))
	for _, d := range c.Definitions {
		out = append(out, agent.Definition{
			Name:           d.Name,
			Kind:           agent.Kind(d.Kind),
			Model:          d.Model,
			SystemPrompt:   d.SystemPrompt,
			AllowedTools:   d.AllowedTools,
			PermissionMode: agent.PermissionMode(d.PermissionMode),
			SubAgents:      d.SubAgents,
			MCPConfig:      d.MCPConfig,
			Environment: environment.Spec{
				Kind:    environment.Kind(d.Environment.Kind),
				Workdir: d.Environment.Workdir,
				Image:   d.Environment.Image,
				Host:    d.Environment.Host,
				KeyPath: d.Environment.KeyPath,
				Env:     d.Environment.Env,
			},
		})
	}
	return out
}

// DefinitionFromAgent converts a stored definition back to its config form.
func DefinitionFromAgent(d agent.Definition) Definition {
	return Definition{
		Name:           d.Name,
		Kind:           string(d.Kind),
		Model:          d.Model,
		SystemPrompt:   d.SystemPrompt,
		AllowedTools:   d.AllowedTools,
		PermissionMode: string(d.PermissionMode),
		MCPConfig:      d.MCPConfig,
		SubAgents:      d.SubAgents,
		Environment: EnvironmentConfig{
			Kind:    string(d.Environment.Kind),
			Workdir: d.Environment.Workdir,
			Image:   d.Environment.Image,
			Host:    d.Environment.Host,
			KeyPath: d.Environment.KeyPath,
			Env:     d.Environment.Env,
		},
	}
}

// AppendDefinition adds a definition to the config file at path without
// rewriting what is already there (comments and formatting survive). The
// result is validated as a whole before the file is touched, and the
// original is restored if the new file fails to load.
func AppendDefinition(path string, d Definition) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	cfg.Definitions = append(cfg.Definitions, d)
	cfg.applyDefaults(path)
	if err := cfg.validate(); err != nil {
		return err
	}

	var snippet bytes.Buffer
	enc := toml.NewEncoder(&snippet)
	enc.Indent = ""
	if err := enc.Encode(struct {
		Definitions []Definition `toml:"definitions"`
	}{[]Definition{d}}); err != nil {
		return err
	}

	return appendBlock(path, "# added from chat on "+time.Now().Format("2006-01-02"), snippet.Bytes())
}

// AppendChannel records a per-channel default agent by appending a
// [[channels]] block to the config file at path (later blocks win, see
// Channel). The result is validated before the file is touched.
func AppendChannel(path string, ch Channel) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	cfg.Channels = append(cfg.Channels, ch)
	cfg.applyDefaults(path)
	if err := cfg.validate(); err != nil {
		return err
	}
	var snippet bytes.Buffer
	enc := toml.NewEncoder(&snippet)
	enc.Indent = ""
	if err := enc.Encode(struct {
		Channels []Channel `toml:"channels"`
	}{[]Channel{ch}}); err != nil {
		return err
	}
	return appendBlock(path, fmt.Sprintf("# default agent for channel %s set from chat on %s", ch.ID, time.Now().Format("2006-01-02")), snippet.Bytes())
}

// appendBlock adds a commented TOML snippet to the end of the file at path
// and restores the original if the result does not load.
func appendBlock(path, comment string, snippet []byte) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	out.Write(orig)
	if len(orig) > 0 && orig[len(orig)-1] != '\n' {
		out.WriteByte('\n')
	}
	fmt.Fprintf(&out, "\n%s\n", comment)
	out.Write(snippet)
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		return err
	}
	if _, err := Load(path); err != nil {
		_ = os.WriteFile(path, orig, 0o600)
		return fmt.Errorf("appended config does not load, restored original: %w", err)
	}
	return nil
}

// Save writes the config to path, creating parent directories.
func Save(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(c)
}
