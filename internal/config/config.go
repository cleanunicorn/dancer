// Package config loads the dancer TOML configuration.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cleanunicorn/dancer/internal/agent"
	"github.com/cleanunicorn/dancer/internal/environment"
)

// Config is the on-disk configuration.
type Config struct {
	Server      Server       `toml:"server"`
	Claude      Claude       `toml:"claude"`
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
}

type Claude struct {
	Binary string `toml:"binary"`
}

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
	if c.Claude.Binary == "" {
		c.Claude.Binary = "claude"
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
	if c.Server.DefaultAgent != "" && !seen[c.Server.DefaultAgent] {
		return fmt.Errorf("config: server.default_agent %q is not a defined agent", c.Server.DefaultAgent)
	}
	for _, ch := range c.Channels {
		if ch.ID == "" {
			return fmt.Errorf("config: channel without id")
		}
	}
	// Only the effective default per channel must exist: earlier, overridden
	// [[channels]] blocks may name an agent that has since been deleted.
	for key, agent := range c.ChannelAgents() {
		if !seen[agent] {
			return fmt.Errorf("config: channel %s: unknown agent %q", key, agent)
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

	snippet, err := definitionSnippet(d)
	if err != nil {
		return err
	}
	return appendBlock(path, "# added from chat on "+time.Now().Format("2006-01-02"), snippet)
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

// ReplaceDefinition rewrites the definition named d.Name in the config
// file at path: its [[definitions]] block (and the comment lines right
// above it) is removed and the new one is appended, so the rest of the file
// keeps its comments and formatting. The result is validated as a whole
// before the file is touched and restored if the new file fails to load.
func ReplaceDefinition(path string, d Definition) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	i := indexDefinition(cfg, d.Name)
	if i < 0 {
		return fmt.Errorf("config: no definition named %q", d.Name)
	}
	cfg.Definitions[i] = d
	cfg.applyDefaults(path)
	if err := cfg.validate(); err != nil {
		return err
	}
	snippet, err := definitionSnippet(d)
	if err != nil {
		return err
	}
	return rewrite(path, func(src []byte) ([]byte, error) {
		out, ok := cutDefinition(src, d.Name)
		if !ok {
			return nil, fmt.Errorf("config: definition %q is not a [[definitions]] block in %s; edit the file by hand", d.Name, path)
		}
		return appendSnippet(out, "# updated from chat on "+time.Now().Format("2006-01-02"), snippet), nil
	})
}

// RemoveDefinition deletes the [[definitions]] block named name from the
// config file at path. A definition that is still the server default or a
// channel default is refused by validation.
func RemoveDefinition(path, name string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	i := indexDefinition(cfg, name)
	if i < 0 {
		return fmt.Errorf("config: no definition named %q", name)
	}
	cfg.Definitions = append(cfg.Definitions[:i], cfg.Definitions[i+1:]...)
	if err := cfg.validate(); err != nil {
		return err
	}
	return rewrite(path, func(src []byte) ([]byte, error) {
		out, ok := cutDefinition(src, name)
		if !ok {
			return nil, fmt.Errorf("config: definition %q is not a [[definitions]] block in %s; edit the file by hand", name, path)
		}
		return out, nil
	})
}

func indexDefinition(cfg *Config, name string) int {
	for i, d := range cfg.Definitions {
		if d.Name == name {
			return i
		}
	}
	return -1
}

// definitionSnippet encodes one definition as a [[definitions]] block.
func definitionSnippet(d Definition) ([]byte, error) {
	var snippet bytes.Buffer
	enc := toml.NewEncoder(&snippet)
	enc.Indent = ""
	if err := enc.Encode(struct {
		Definitions []Definition `toml:"definitions"`
	}{[]Definition{d}}); err != nil {
		return nil, err
	}
	return snippet.Bytes(), nil
}

var (
	tableHeaderRE = regexp.MustCompile(`^\s*\[`)
	defHeaderRE   = regexp.MustCompile(`^\s*\[\[\s*definitions\s*\]\]`)
	defSubTableRE = regexp.MustCompile(`^\s*\[\s*definitions\.`)
	nameKeyRE     = regexp.MustCompile(`^\s*name\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// cutDefinition removes the [[definitions]] block whose name key is name,
// together with the comment lines directly above its header and one blank
// line before those. ok is false when no such block exists (for example a
// definition written as an inline table).
func cutDefinition(src []byte, name string) (out []byte, ok bool) {
	lines := strings.SplitAfter(string(src), "\n")
	for h := 0; h < len(lines); h++ {
		if !defHeaderRE.MatchString(lines[h]) {
			continue
		}
		// The block runs up to the next header that is not one of its
		// sub-tables; blank and comment lines right before that header
		// belong to the next block and stay.
		end := h + 1
		for end < len(lines) && (!tableHeaderRE.MatchString(lines[end]) || defSubTableRE.MatchString(lines[end])) {
			end++
		}
		for end > h+1 && isBlankOrComment(lines[end-1]) {
			end--
		}
		// Its own keys are the lines before the first sub-table.
		found := false
		for i := h + 1; i < end && !tableHeaderRE.MatchString(lines[i]); i++ {
			if m := nameKeyRE.FindStringSubmatch(lines[i]); m != nil && (m[1] == name || m[2] == name) {
				found = true
				break
			}
		}
		if !found {
			h = end - 1
			continue
		}
		start := h
		for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
			start--
		}
		if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
			start--
		}
		if start == 0 {
			// First block in the file: do not leave it starting with blank lines.
			for end < len(lines) && strings.TrimSpace(lines[end]) == "" {
				end++
			}
		}
		return []byte(strings.Join(append(lines[:start:start], lines[end:]...), "")), true
	}
	return nil, false
}

func isBlankOrComment(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

// appendSnippet adds a commented TOML snippet to the end of src.
func appendSnippet(src []byte, comment string, snippet []byte) []byte {
	var out bytes.Buffer
	out.Write(src)
	if len(src) > 0 && src[len(src)-1] != '\n' {
		out.WriteByte('\n')
	}
	fmt.Fprintf(&out, "\n%s\n", comment)
	out.Write(snippet)
	return out.Bytes()
}

// rewrite replaces the file at path with edit(original) and restores the
// original if the result does not load.
func rewrite(path string, edit func([]byte) ([]byte, error)) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := edit(orig)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	if _, err := Load(path); err != nil {
		_ = os.WriteFile(path, orig, 0o600)
		return fmt.Errorf("rewritten config does not load, restored original: %w", err)
	}
	return nil
}

// appendBlock adds a commented TOML snippet to the end of the file at path
// and restores the original if the result does not load.
func appendBlock(path, comment string, snippet []byte) error {
	return rewrite(path, func(src []byte) ([]byte, error) {
		return appendSnippet(src, comment, snippet), nil
	})
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
