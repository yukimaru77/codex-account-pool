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
	RoundRobin         map[string]Route `json:"round_robin_endpoints"`
	TLSCert            string           `json:"tls_cert,omitempty"`
	TLSKey             string           `json:"tls_key,omitempty"`
	ProxyURL           string           `json:"proxy_url,omitempty"`
}

func DefaultConfig() Config {
	return Config{Listen: "127.0.0.1:18473", StateDir: "state", ReservePercent: 10, QuotaPollSeconds: 60, QuotaMaxAgeSeconds: 180, RoundRobin: map[string]Route{
		"/_pool/rr/images/generations": {Path: "/backend-api/codex/images/generations"},
		"/_pool/rr/images/edits":       {Path: "/backend-api/codex/images/edits"},
		"/_pool/rr/responses/compact":  {Path: "/backend-api/codex/responses/compact"},
		"/_pool/rr/responses":          {Path: "/backend-api/codex/responses"},
	}}
}

func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	defaultRoutes := c.RoundRobin
	c.RoundRobin = nil
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
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
		c.StateDir = filepath.Join(filepath.Dir(path), c.StateDir)
	}
	c.StateDir, err = filepath.Abs(c.StateDir)
	if err != nil {
		return c, err
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
