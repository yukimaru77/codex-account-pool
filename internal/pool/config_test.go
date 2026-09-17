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
