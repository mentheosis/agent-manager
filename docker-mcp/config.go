package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the JSON configuration loaded from disk at startup.
//
// The config is the security boundary: it enumerates exactly which commands
// the MCP server is willing to run on the host. Claude (the MCP client) can
// only invoke profiles by name — it can never inject arguments or change
// the working directory.
type Config struct {
	// Listen is the address the HTTP server binds to. Defaults to
	// "127.0.0.1:9090". Bind to a non-loopback address only if you really
	// know what you're doing — there is no per-tenant authentication, only
	// the bearer token.
	Listen string `json:"listen"`

	// AuthTokenEnv is the name of the environment variable holding the
	// bearer token clients must present in the Authorization header.
	// Defaults to DOCKER_MCP_TOKEN. Required for HTTP mode.
	AuthTokenEnv string `json:"auth_token_env"`

	// LogDir is where per-job stdout/stderr log files are written. Defaults
	// to ~/.cache/docker-mcp/logs. Tilde is expanded.
	LogDir string `json:"log_dir"`

	// MaxConcurrentPerProfile caps how many jobs of the same profile may
	// run simultaneously. Defaults to 1 (sensible for builds — running two
	// `docker build`s of the same image at once just creates contention).
	// Set to 0 for no per-profile limit.
	MaxConcurrentPerProfile int `json:"max_concurrent_per_profile"`

	// Shell specifies the shell to use for running scripts. Defaults to
	// ["bash"]. Set to ["bash", "-l"] or ["zsh", "-l"] to use a login
	// shell that sources your profile (~/.zshrc, ~/.bash_profile, etc.).
	Shell []string `json:"shell"`

	// InheritEnv controls whether child processes inherit the host's full
	// environment. Defaults to false for security (only PATH, HOME, and
	// docker-related vars are passed). Set to true for convenience if you
	// need access to all your shell environment variables.
	InheritEnv bool `json:"inherit_env"`

	// Profiles enumerates the commands the server is willing to run.
	Profiles []Profile `json:"profiles"`
}

// Profile is a single named, fully-fixed command the MCP server will run on
// behalf of a client. The argv is taken literally — no shell interpolation,
// no client-supplied arguments.
type Profile struct {
	// Name is the identifier the MCP client uses to invoke this profile.
	// Must be unique within the config. Use kebab-case (e.g. "build-dev").
	Name string `json:"name"`

	// Description is human-readable text shown to the MCP client when it
	// lists profiles, so the model knows when to use which one.
	Description string `json:"description"`

	// Cwd is the absolute working directory the command runs in. Must
	// exist and must be a directory. Tilde is expanded.
	Cwd string `json:"cwd"`

	// Argv is the full command vector. argv[0] is the executable, the
	// rest are its arguments. Run without a shell — no string parsing,
	// no globbing, no env interpolation. Bring your own resolved values.
	// Exactly one of Argv, Script, or Command must be set.
	Argv []string `json:"argv"`

	// Script is a path to a shell script to run. Can be absolute or
	// relative to Cwd. Tilde is expanded. The script is run via the
	// configured shell. Exactly one of Argv, Script, or Command must be set.
	Script string `json:"script"`

	// Command is a shell command string to run (e.g. "docker logs foo --tail 30").
	// Run via the configured shell with -c flag. Exactly one of Argv, Script,
	// or Command must be set.
	Command string `json:"command"`

	// Env is an optional set of environment variables to set when running
	// the command. The host's environment is NOT inherited by default;
	// only PATH and the entries here are passed through.
	Env map[string]string `json:"env"`

	// TimeoutSeconds is the maximum wall time the job may run before it
	// is killed. 0 means no timeout (not recommended).
	TimeoutSeconds int `json:"timeout_seconds"`
}

// LoadConfig reads, parses, and validates a JSON config file. Path is
// expanded for ~. Returns a populated Config ready for use.
func LoadConfig(path string) (*Config, error) {
	expanded, err := expandTilde(path)
	if err != nil {
		return nil, fmt.Errorf("expand config path: %w", err)
	}

	raw, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", expanded, err)
	}

	cfg := &Config{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", expanded, err)
	}

	// Apply defaults
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:9090"
	}
	if cfg.AuthTokenEnv == "" {
		cfg.AuthTokenEnv = "DOCKER_MCP_TOKEN"
	}
	if cfg.LogDir == "" {
		cfg.LogDir = "~/.cache/docker-mcp/logs"
	}
	if cfg.MaxConcurrentPerProfile == 0 {
		cfg.MaxConcurrentPerProfile = 1
	}
	if len(cfg.Shell) == 0 {
		cfg.Shell = []string{"bash"}
	}

	cfg.LogDir, err = expandTilde(cfg.LogDir)
	if err != nil {
		return nil, fmt.Errorf("expand log_dir: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if len(c.Profiles) == 0 {
		return fmt.Errorf("config: no profiles defined — nothing for the MCP server to run")
	}
	seen := map[string]bool{}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		if p.Name == "" {
			return fmt.Errorf("profile #%d: name is required", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("profile %q: duplicate name", p.Name)
		}
		seen[p.Name] = true

		if p.Cwd == "" {
			return fmt.Errorf("profile %q: cwd is required (must be an absolute path)", p.Name)
		}
		expanded, err := expandTilde(p.Cwd)
		if err != nil {
			return fmt.Errorf("profile %q: expand cwd: %w", p.Name, err)
		}
		p.Cwd = expanded
		if !filepath.IsAbs(p.Cwd) {
			return fmt.Errorf("profile %q: cwd must be absolute, got %q", p.Name, p.Cwd)
		}
		st, err := os.Stat(p.Cwd)
		if err != nil {
			return fmt.Errorf("profile %q: cwd %q: %w", p.Name, p.Cwd, err)
		}
		if !st.IsDir() {
			return fmt.Errorf("profile %q: cwd %q is not a directory", p.Name, p.Cwd)
		}

		// Exactly one of argv, script, or command must be set
		hasArgv := len(p.Argv) > 0
		hasScript := p.Script != ""
		hasCommand := p.Command != ""
		setCount := 0
		if hasArgv {
			setCount++
		}
		if hasScript {
			setCount++
		}
		if hasCommand {
			setCount++
		}
		if setCount == 0 {
			return fmt.Errorf("profile %q: one of argv, script, or command is required", p.Name)
		}
		if setCount > 1 {
			return fmt.Errorf("profile %q: only one of argv, script, or command may be set", p.Name)
		}

		// If script is set, resolve it and convert to argv
		if hasScript {
			scriptPath, err := expandTilde(p.Script)
			if err != nil {
				return fmt.Errorf("profile %q: expand script: %w", p.Name, err)
			}
			// If not absolute, make it relative to cwd
			if !filepath.IsAbs(scriptPath) {
				scriptPath = filepath.Join(p.Cwd, scriptPath)
			}
			// Verify script exists
			if _, err := os.Stat(scriptPath); err != nil {
				return fmt.Errorf("profile %q: script %q: %w", p.Name, scriptPath, err)
			}
			// Convert to argv: <shell...> <script>
			p.Argv = append(append([]string{}, c.Shell...), scriptPath)
		}

		// If command is set, wrap it with shell -c
		if hasCommand {
			// Convert to argv: <shell...> -c <command>
			p.Argv = append(append([]string{}, c.Shell...), "-c", p.Command)
		}

		if p.TimeoutSeconds < 0 {
			return fmt.Errorf("profile %q: timeout_seconds must be >= 0", p.Name)
		}
	}
	return nil
}

// FindProfile returns the named profile, or nil if no such profile exists.
func (c *Config) FindProfile(name string) *Profile {
	for i := range c.Profiles {
		if c.Profiles[i].Name == name {
			return &c.Profiles[i]
		}
	}
	return nil
}

// expandTilde replaces a leading ~ with the user's home directory. Other
// tilde forms (~user) are not supported.
func expandTilde(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p, nil
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return "", fmt.Errorf("only ~/ tilde expansion is supported, got %q", p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
