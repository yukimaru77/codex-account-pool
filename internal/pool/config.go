package pool

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Route struct {
	Path    string            `json:"upstream_path"`
	Headers map[string]string `json:"headers,omitempty"`
}

type Config struct {
	Listen             string           `json:"listen"`
	StateDir           string           `json:"state_dir"`
	ReservePercent     float64          `json:"reserve_percent"`
	QuotaPollSeconds   int              `json:"quota_poll_seconds"`
	QuotaMaxAgeSeconds int              `json:"quota_max_age_seconds"`
	RoundRobin         map[string]Route `json:"round_robin_endpoints,omitzero"`
	TLSCert            string           `json:"tls_cert,omitempty"`
	TLSKey             string           `json:"tls_key,omitempty"`
	ProxyURL           string           `json:"proxy_url,omitempty"`
	// AccountsDir, when set, selects the Codex-home store: each subdirectory
	// is a genuine CODEX_HOME holding auth.json. "~/" is expanded and a
	// relative path is resolved against the config file's directory.
	AccountsDir string `json:"accounts_dir,omitempty"`
	// CodexHome is the user's own Codex home; it defaults to ~/.codex when
	// AccountsDir is set.
	CodexHome string `json:"codex_home,omitempty"`
	// CodexBin is the real codex executable (absolute path or bare name).
	CodexBin string `json:"codex_bin,omitempty"`
}

// UsesCodexHome reports whether accounts are read from Codex home directories.
func (c Config) UsesCodexHome() bool { return c.AccountsDir != "" }

// expandHome replaces a leading "~/" with the user's home directory.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// resolvePath expands "~/" and makes p absolute relative to base.
func resolvePath(base, p string) (string, error) {
	p = expandHome(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Abs(p)
}

func DefaultConfig() Config {
	return Config{Listen: "127.0.0.1:18473", StateDir: "state", ReservePercent: 10, QuotaPollSeconds: 60, QuotaMaxAgeSeconds: 180, RoundRobin: map[string]Route{
		"/_pool/rr/images/generations": {Path: "/backend-api/codex/images/generations"},
		"/_pool/rr/images/edits":       {Path: "/backend-api/codex/images/edits"},
		"/_pool/rr/responses/compact":  {Path: "/backend-api/codex/responses/compact"},
		"/_pool/rr/responses":          {Path: "/backend-api/codex/responses"},
		"/_pool/rr/alpha/search":       {Path: "/backend-api/codex/alpha/search"},
	}}
}

func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return ParseConfig(b, filepath.Dir(path))
}

// ParseConfig decodes and validates a config as LoadConfig does, resolving
// relative paths against base (the config file's directory). It touches no
// files, so a config can be checked before it is written.
func ParseConfig(b []byte, base string) (Config, error) {
	c := DefaultConfig()
	defaultRoutes := c.RoundRobin
	c.RoundRobin = nil
	err := json.Unmarshal(b, &c)
	if err != nil {
		return c, err
	}
	if c.RoundRobin == nil {
		c.RoundRobin = defaultRoutes
	}
	if c.ReservePercent < 0 || c.ReservePercent >= 100 {
		return c, fmt.Errorf("reserve_percent must be in [0,100)")
	}
	if c.QuotaPollSeconds <= 0 || c.QuotaMaxAgeSeconds < c.QuotaPollSeconds {
		return c, fmt.Errorf("quota age must be at least the positive poll interval")
	}
	if _, _, err = net.SplitHostPort(c.Listen); err != nil {
		return c, fmt.Errorf("listen: %w", err)
	}
	if c.StateDir == "" {
		return c, fmt.Errorf("state_dir is required")
	}
	if !filepath.IsAbs(c.StateDir) {
		c.StateDir = filepath.Join(base, c.StateDir)
	}
	c.StateDir, err = filepath.Abs(c.StateDir)
	if err != nil {
		return c, err
	}
	if c.AccountsDir != "" {
		if c.AccountsDir, err = resolvePath(base, c.AccountsDir); err != nil {
			return c, err
		}
		if c.CodexHome == "" {
			c.CodexHome = "~/.codex"
		}
	}
	if c.CodexHome != "" {
		if c.CodexHome, err = resolvePath(base, c.CodexHome); err != nil {
			return c, err
		}
	}
	if c.CodexBin != "" {
		c.CodexBin = expandHome(c.CodexBin)
		if !filepath.IsAbs(c.CodexBin) && strings.ContainsRune(c.CodexBin, filepath.Separator) {
			return c, fmt.Errorf("codex_bin must be an absolute path or a bare command name")
		}
	}
	for endpoint, route := range c.RoundRobin {
		for name, value := range route.Headers {
			switch strings.ToLower(name) {
			case "user-agent", "originator", "accept", "content-type":
			default:
				return c, fmt.Errorf("unsupported RR header %q", name)
			}
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
				return c, fmt.Errorf("invalid RR header %q", name)
			}
		}
		if !strings.HasPrefix(endpoint, "/_pool/rr/") || strings.ContainsAny(endpoint, "?#%") || !strings.HasPrefix(route.Path, "/") {
			return c, fmt.Errorf("invalid round-robin endpoint %q", endpoint)
		}
	}
	return c, nil
}

func (c Config) maxAge() time.Duration { return time.Duration(c.QuotaMaxAgeSeconds) * time.Second }
