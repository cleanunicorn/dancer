// Package config loads the dancer TOML configuration.
package config

import (
	"bytes"
	"errors"
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
	Docker      Docker       `toml:"docker"`
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

// Docker is host-wide container behaviour; per-agent settings live on the
// definition's [definitions.environment].
type Docker struct {
	// Binary is the docker CLI (default "docker").
	Binary string `toml:"binary,omitempty"`
	// RunArgs are appended to every `docker run` (e.g. "--network=host").
	RunArgs []string `toml:"run_args,omitempty"`
	// ReuseTTL is how long a reused container may sit unused before dancer
	// removes it (default 24h).
	ReuseTTL Duration `toml:"reuse_ttl,omitempty"`
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

	// Provision (docker) is "auto" (default) or "none". With "auto" dancer
	// makes the image agent-ready before first use: a plain `ubuntu:24.04`
	// gets git, node and the agent CLI. An image that already has the CLI
	// is used unchanged.
	Provision string `toml:"provision,omitempty"`
	// Packages (docker) are extra OS packages provisioning installs.
	Packages []string `toml:"packages,omitempty"`
	// Setup (docker) are extra shell commands provisioning runs as root.
	Setup []string `toml:"setup,omitempty"`
	// Reuse (docker) is the container lifetime: "task" (default),
	// "thread" (one container per conversation) or "definition".
	Reuse string `toml:"reuse,omitempty"`
}

// provisionSpec turns the config form into an environment.Provision. It is
// nil when provisioning is off or the environment is not a container.
func (e EnvironmentConfig) provisionSpec(agentKind string) *environment.Provision {
	if environment.Kind(e.Kind) != environment.KindDocker {
		return nil
	}
	if strings.EqualFold(e.Provision, "none") {
		return nil
	}
	if agentKind == "" {
		agentKind = string(agent.KindClaude)
	}
	return &environment.Provision{
		Agents:   []string{agentKind},
		Packages: e.Packages,
		Setup:    e.Setup,
	}
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
			switch strings.ToLower(d.Environment.Provision) {
			case "", "auto", "none":
			default:
				return fmt.Errorf("config: definition %q: provision must be auto or none, got %q", d.Name, d.Environment.Provision)
			}
			switch environment.Reuse(strings.ToLower(d.Environment.Reuse)) {
			case "", environment.ReuseTask, environment.ReuseThread, environment.ReuseDefinition:
			default:
				return fmt.Errorf("config: definition %q: reuse must be task, thread or definition, got %q", d.Name, d.Environment.Reuse)
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
				Kind:      environment.Kind(d.Environment.Kind),
				Workdir:   d.Environment.Workdir,
				Image:     d.Environment.Image,
				Host:      d.Environment.Host,
				KeyPath:   d.Environment.KeyPath,
				Env:       d.Environment.Env,
				Provision: d.Environment.provisionSpec(d.Kind),
				Reuse:     environment.Reuse(d.Environment.Reuse),
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
			Kind:      string(d.Environment.Kind),
			Workdir:   d.Environment.Workdir,
			Image:     d.Environment.Image,
			Host:      d.Environment.Host,
			KeyPath:   d.Environment.KeyPath,
			Env:       d.Environment.Env,
			Provision: provisionMode(d.Environment),
			Packages:  provisionPackages(d.Environment.Provision),
			Setup:     provisionSetup(d.Environment.Provision),
			Reuse:     string(d.Environment.Reuse),
		},
	}
}

// provisionMode is the config spelling of a *environment.Provision. The
// agent list is not written back: it is derived from the definition's kind.
func provisionMode(spec environment.Spec) string {
	if spec.Kind != environment.KindDocker || spec.Provision != nil {
		return ""
	}
	return "none"
}

func provisionPackages(p *environment.Provision) []string {
	if p == nil {
		return nil
	}
	return p.Packages
}

func provisionSetup(p *environment.Provision) []string {
	if p == nil {
		return nil
	}
	return p.Setup
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
	snippet, err := tomlSnippet(struct {
		Channels []Channel `toml:"channels"`
	}{[]Channel{ch}})
	if err != nil {
		return err
	}
	return appendBlock(path, fmt.Sprintf("# default agent for channel %s set from chat on %s", ch.ID, time.Now().Format("2006-01-02")), snippet)
}

// ErrNoDefinition is returned by RemoveDefinition when the config file has
// no definition of that name (it may still exist in the store).
var ErrNoDefinition = errors.New("config: no such definition")

// ReplaceDefinition rewrites the definition named d.Name in the config
// file at path in place: only its [[definitions]] block is replaced, so its
// position (which decides the implicit default agent), the comments above
// it and the rest of the file survive. A definition the file does not have
// is appended. The result is validated as a whole before the file is
// touched and restored if the new file fails to load.
func ReplaceDefinition(path string, d Definition) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	i := indexDefinition(cfg, d.Name)
	if i < 0 {
		return AppendDefinition(path, d)
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
		out, ok := spliceDefinition(src, d.Name, snippet)
		if !ok {
			return nil, fmt.Errorf("config: definition %q is not a [[definitions]] block in %s; edit the file by hand", d.Name, path)
		}
		return out, nil
	})
}

// RemoveDefinition deletes the [[definitions]] block named name from the
// config file at path. The default agent (server.default_agent, or the
// first definition when unset) and channel defaults are refused; a name the
// file does not have returns ErrNoDefinition.
func RemoveDefinition(path, name string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	i := indexDefinition(cfg, name)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNoDefinition, name)
	}
	if name == cfg.Server.DefaultAgent {
		return fmt.Errorf("config: %q is the default agent (server.default_agent); change that first", name)
	}
	cfg.Definitions = append(cfg.Definitions[:i], cfg.Definitions[i+1:]...)
	if err := cfg.validate(); err != nil {
		return err
	}
	return rewrite(path, func(src []byte) ([]byte, error) {
		out, ok := spliceDefinition(src, name, nil)
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

// tomlSnippet encodes v as unindented TOML.
func tomlSnippet(v any) ([]byte, error) {
	var snippet bytes.Buffer
	enc := toml.NewEncoder(&snippet)
	enc.Indent = ""
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return snippet.Bytes(), nil
}

// definitionSnippet encodes one definition as a [[definitions]] block.
func definitionSnippet(d Definition) ([]byte, error) {
	return tomlSnippet(struct {
		Definitions []Definition `toml:"definitions"`
	}{[]Definition{d}})
}

var (
	tableHeaderRE = regexp.MustCompile(`^\s*\[`)
	defHeaderRE   = regexp.MustCompile(`^\s*\[\[\s*definitions\s*\]\]`)
	defSubTableRE = regexp.MustCompile(`^\s*\[\[?\s*definitions\.`)
	nameKeyRE     = regexp.MustCompile(`^\s*name\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// defBlock locates the [[definitions]] block whose name key is name in
// lines: header is its header line, end the line after its last key or
// sub-table (blank and comment lines before the next header belong to the
// next block), comments the first of the comment lines directly above the
// header. Lines inside multi-line strings are never headers. ok is false
// when no such block exists (for example a definition written as an
// inline table).
func defBlock(lines []string, name string) (comments, header, end int, ok bool) {
	isHeader := make([]bool, len(lines))
	inString := false
	for i, l := range lines {
		isHeader[i] = !inString && tableHeaderRE.MatchString(l)
		if strings.Count(l, `"""`)%2 == 1 || strings.Count(l, `'''`)%2 == 1 {
			inString = !inString
		}
	}
	for h := 0; h < len(lines); h++ {
		if !isHeader[h] || !defHeaderRE.MatchString(lines[h]) {
			continue
		}
		end = h + 1
		for end < len(lines) && (!isHeader[end] || defSubTableRE.MatchString(lines[end])) {
			end++
		}
		for end > h+1 && isBlankOrComment(lines[end-1]) {
			end--
		}
		// Its own keys are the lines before the first sub-table.
		found := false
		for i := h + 1; i < end && !isHeader[i]; i++ {
			if m := nameKeyRE.FindStringSubmatch(lines[i]); m != nil && (m[1] == name || m[2] == name) {
				found = true
				break
			}
		}
		if !found {
			h = end - 1
			continue
		}
		comments = h
		for comments > 0 && strings.HasPrefix(strings.TrimSpace(lines[comments-1]), "#") {
			comments--
		}
		return comments, h, end, true
	}
	return 0, 0, 0, false
}

// spliceDefinition replaces the [[definitions]] block named name with
// replacement, keeping the comment lines above it. A nil replacement
// removes the block together with those comments and one blank line before
// them. ok is false when no such block exists.
func spliceDefinition(src []byte, name string, replacement []byte) (out []byte, ok bool) {
	lines := strings.SplitAfter(string(src), "\n")
	comments, header, end, ok := defBlock(lines, name)
	if !ok {
		return nil, false
	}
	var b strings.Builder
	if replacement != nil {
		b.WriteString(strings.Join(lines[:header], ""))
		b.Write(replacement)
		b.WriteString(strings.Join(lines[end:], ""))
		return []byte(b.String()), true
	}
	start := comments
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	if start == 0 {
		// First block in the file: do not leave it starting with blank lines.
		for end < len(lines) && strings.TrimSpace(lines[end]) == "" {
			end++
		}
	}
	b.WriteString(strings.Join(lines[:start], ""))
	b.WriteString(strings.Join(lines[end:], ""))
	return []byte(b.String()), true
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
