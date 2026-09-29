package pool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitRoundRobinRoutesReplaceDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		count      int
	}{
		{"omitted", `{"reserve_percent":37}`, 4},
		{"empty", `{"reserve_percent":37,"round_robin_endpoints":{}}`, 0},
		{"custom", `{"reserve_percent":37,"round_robin_endpoints":{"/_pool/rr/custom-compact":{"upstream_path":"/backend-api/codex/responses/compact"}}}`, 1},
		{"unknown_upstream", `{"reserve_percent":37,"round_robin_endpoints":{"/_pool/rr/future":{"upstream_path":"/future-api/operation"}}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pool.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil || len(cfg.RoundRobin) != tc.count || cfg.ReservePercent != 37 {
				t.Fatalf("route config count=%d err=%v", len(cfg.RoundRobin), err)
			}
		})
	}
}

func TestRoundRobinRouteHeaders(t *testing.T) {
	load := func(t *testing.T, body string) (Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "pool.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(path)
	}
	t.Run("valid", func(t *testing.T) {
		cfg, err := load(t, `{"round_robin_endpoints":{"/_pool/rr/responses":{"upstream_path":"/backend-api/codex/responses","headers":{"Originator":"kb-pool","User-Agent":"pool-ua"}}}}`)
		if err != nil {
			t.Fatal(err)
		}
		got := cfg.RoundRobin["/_pool/rr/responses"].Headers
		if got["Originator"] != "kb-pool" || got["User-Agent"] != "pool-ua" || len(got) != 2 {
			t.Fatalf("headers = %v", got)
		}
	})
	for _, tc := range []struct{ name, key string }{
		{"empty", ``},
		{"colon", `Bad:Name`},
		{"space", `Bad Name`},
		{"tab", "Bad\\tName"},
	} {
		t.Run("rejects_"+tc.name, func(t *testing.T) {
			_, err := load(t, `{"round_robin_endpoints":{"/_pool/rr/responses":{"upstream_path":"/backend-api/codex/responses","headers":{"`+tc.key+`":"v"}}}}`)
			want := `invalid header name in route "/_pool/rr/responses"`
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %s", err, want)
			}
		})
	}
}

func TestLoadConfigExpandsAccountsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	load := func(t *testing.T, body string) (Config, string) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "pool.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg, dir
	}
	t.Run("tilde", func(t *testing.T) {
		cfg, _ := load(t, `{"accounts_dir":"~/x"}`)
		if cfg.AccountsDir != filepath.Join(home, "x") {
			t.Fatalf("accounts_dir = %q", cfg.AccountsDir)
		}
		if !cfg.UsesCodexHome() {
			t.Fatal("UsesCodexHome false with accounts_dir set")
		}
		if cfg.CodexHome != filepath.Join(home, ".codex") {
			t.Fatalf("codex_home default = %q", cfg.CodexHome)
		}
	})
	t.Run("relative", func(t *testing.T) {
		cfg, dir := load(t, `{"accounts_dir":"accounts","codex_home":"~/custom-codex"}`)
		want, _ := filepath.Abs(filepath.Join(dir, "accounts"))
		if cfg.AccountsDir != want {
			t.Fatalf("accounts_dir = %q, want %q", cfg.AccountsDir, want)
		}
		if cfg.CodexHome != filepath.Join(home, "custom-codex") {
			t.Fatalf("codex_home = %q", cfg.CodexHome)
		}
	})
	t.Run("unset", func(t *testing.T) {
		cfg, _ := load(t, `{}`)
		if cfg.UsesCodexHome() || cfg.AccountsDir != "" || cfg.CodexHome != "" {
			t.Fatalf("codex-home fields set without accounts_dir: %+v", cfg)
		}
	})
	t.Run("codex_bin", func(t *testing.T) {
		for _, ok := range []string{"codex", "/usr/local/bin/codex"} {
			cfg, _ := load(t, `{"accounts_dir":"a","codex_bin":"`+ok+`"}`)
			if cfg.CodexBin != ok {
				t.Fatalf("codex_bin = %q", cfg.CodexBin)
			}
		}
		cfg, _ := load(t, `{"accounts_dir":"a","codex_bin":"~/bin/codex"}`)
		if cfg.CodexBin != filepath.Join(home, "bin", "codex") {
			t.Fatalf("codex_bin = %q", cfg.CodexBin)
		}
		path := filepath.Join(t.TempDir(), "pool.json")
		if err := os.WriteFile(path, []byte(`{"accounts_dir":"a","codex_bin":"bin/codex"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("relative codex_bin path accepted")
		}
	})
}
