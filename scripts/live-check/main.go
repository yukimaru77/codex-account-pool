// Live integration checks use the real credential store and an isolated relay.
// No production routing configuration or account enablement is changed.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/cpa"
	"codex-account-pool/internal/pool"
)

type check struct {
	cfg            pool.Config
	store          *pool.Store
	transport      *http.Transport
	accounts       []pool.Credential
	quotas         map[string]pool.Quota
	model, output  string
	mu             sync.Mutex
	failed, passed int
}

func main() {
	os.Exit(run())
}

func run() int {
	config := flag.String("config", "", "existing pool config (required)")
	mode := flag.String("mode", "routing", "routing, refresh, responses, compact, compact-v2, or images")
	model := flag.String("model", "gpt-6-astra", "model for live response/compaction probes")
	output := flag.String("output", "", "private directory for image artifacts")
	flag.Parse()
	if *config == "" {
		panic("--config is required")
	}
	cfg, err := pool.LoadConfig(*config)
	if err != nil {
		panic("cannot load config")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			panic("invalid proxy URL")
		}
		tr.Proxy = http.ProxyURL(u)
	}
	defer tr.CloseIdleConnections()
	auth := cpa.NewCodexAuth(&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	s, err := pool.OpenStore(cfg.StateDir, func(ctx context.Context, token string) (*cpa.CodexTokenData, error) {
		return auth.RefreshTokensWithRetry(ctx, token, 3)
	})
	if err != nil {
		panic("cannot open store")
	}
	accounts, err := s.List()
	if err != nil {
		panic("cannot read account metadata")
	}
	c := &check{cfg: cfg, store: s, transport: tr, accounts: accounts, quotas: map[string]pool.Quota{}, model: *model, output: *output}
	h := pool.NewHandler(cfg, s, tr, "live-check-client", "live-check-admin")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := h.Poll(ctx); err != nil {
		panic("quota poll failed")
	}
	for _, a := range h.Scheduler.Status() {
		c.quotas[a.ID] = a.Quota
		c.emit(map[string]any{"check": "quota", "auth_index": a.ID, "email": a.Email, "remaining": 100 - a.Quota.Weekly.Used, "reset": a.Quota.Weekly.Reset, "error": a.Error})
	}
	switch *mode {
	case "routing":
		c.routing()
	case "refresh":
		c.refresh()
	case "responses":
		for _, a := range accounts {
			h := c.handler(&a)
			c.request(h, "response-"+a.ID(), "POST", "/backend-api/codex/responses", c.responseBody(nil), "POOL_LIVE_OK")
		}
	case "compact":
		c.compact()
	case "compact-v2":
		c.compactV2()
	case "images":
		c.images()
	default:
		panic("unknown mode")
	}
	c.mu.Lock()
	failed, passed := c.failed, c.passed
	c.mu.Unlock()
	c.emit(map[string]any{"check": "summary", "passed": passed, "failed": failed})
	if failed > 0 || passed == 0 {
		return 1
	}
	return 0
}

func (c *check) emit(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if result, ok := v.(map[string]any); ok {
		if success, checked := result["ok"].(bool); checked {
			if success {
				c.passed++
			} else {
				c.failed++
			}
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(v)
}

func (c *check) handler(only *pool.Credential) *pool.Handler {
	h := pool.NewHandler(c.cfg, c.store, c.transport, "live-check-client", "live-check-admin")
	a := c.accounts
	if only != nil {
		a = []pool.Credential{*only}
	}
	h.Scheduler.Sync(a)
	for _, a := range a {
		h.Scheduler.Observe(a.ID(), c.quotas[a.ID()])
	}
	h.Publish = func(_ context.Context, r pool.Record) {
		c.emit(map[string]any{"check": "relay-record", "auth_index": r.AuthIndex, "endpoint": r.Endpoint, "policy": r.Policy, "status": r.Fail.StatusCode, "failed": r.Failed, "tokens": r.Detail})
	}
	return h
}

func (c *check) request(h *pool.Handler, name, method, path string, body any, expected string) (json.RawMessage, bool) {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer live-check-client")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Originator", "codex_cli_rs")
	r.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
	started := time.Now()
	resp, err := srv.Client().Do(r)
	if err != nil {
		c.emit(map[string]any{"check": name, "ok": false, "error": "request failed", "elapsed_seconds": time.Since(started).Seconds()})
		return nil, false
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 48<<20))
	var object map[string]json.RawMessage
	text := ""
	eventTypes := map[string]bool{}
	var outputItems []json.RawMessage
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") || bytes.HasPrefix(data, []byte("event:")) {
		scan := bufio.NewScanner(bytes.NewReader(data))
		scan.Buffer(make([]byte, 4096), 4<<20)
		for scan.Scan() {
			line := strings.TrimPrefix(scan.Text(), "data: ")
			var event struct {
				Type     string          `json:"type"`
				Delta    string          `json:"delta"`
				Response json.RawMessage `json:"response"`
				Item     json.RawMessage `json:"item"`
			}
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			eventTypes[event.Type] = true
			if event.Type == "response.output_text.delta" {
				text += event.Delta
			}
			if event.Type == "response.completed" {
				_ = json.Unmarshal(event.Response, &object)
			}
			if event.Type == "response.output_item.done" && len(event.Item) > 0 {
				outputItems = append(outputItems, event.Item)
			}
		}
	} else {
		_ = json.Unmarshal(data, &object)
	}
	ok := err == nil && resp.StatusCode == 200 && object != nil
	if expected != "" {
		ok = ok && strings.TrimSpace(text) == expected
	}
	result := map[string]any{"check": name, "status": resp.StatusCode, "ok": ok, "bytes": len(data), "elapsed_seconds": time.Since(started).Seconds(), "content_type": resp.Header.Get("Content-Type"), "event_types": eventTypes}
	if expected != "" {
		result["expected_text"] = strings.TrimSpace(text) == expected
	}
	if !ok {
		// Only a bounded, scrubbed excerpt is reported; never emit token values.
		excerpt := string(data)
		for _, a := range c.accounts {
			for _, v := range []string{a.AccessToken, a.RefreshToken, a.IDToken} {
				if v != "" {
					excerpt = strings.ReplaceAll(excerpt, v, "[redacted]")
				}
			}
		}
		if len(excerpt) > 320 {
			result["response_tail"] = excerpt[len(excerpt)-320:]
			excerpt = excerpt[:320]
		}
		result["response_excerpt"] = excerpt
	}
	c.emit(result)
	if len(outputItems) > 0 {
		output, _ := json.Marshal(outputItems)
		return output, ok
	}
	if len(object["output"]) > 0 {
		return object["output"], ok
	}
	return data, ok
}

func (c *check) routing() {
	h := c.handler(nil)
	records := make(chan pool.Record, 32)
	observe := func(h *pool.Handler) {
		publish := h.Publish
		h.Publish = func(ctx context.Context, r pool.Record) { publish(ctx, r); records <- r }
	}
	verify := func(expected string, policy pool.Policy) {
		var r pool.Record
		select {
		case r = <-records:
		default:
		}
		c.emit(map[string]any{"check": "routing-record", "expected": expected, "actual": r.AuthIndex, "policy": r.Policy, "ok": r.AuthIndex == expected && r.Policy == policy && !r.Failed})
	}
	observe(h)
	want := append([]pool.Credential(nil), c.accounts...)
	sort.Slice(want, func(i, j int) bool {
		return c.quotas[want[i].ID()].Weekly.Reset.Before(c.quotas[want[j].ID()].Weekly.Reset)
	})
	for _, a := range want {
		selected, err := h.Scheduler.Select(pool.FillFirst, "/backend-api/wham/usage", "")
		c.emit(map[string]any{"check": "fill-first-order", "expected": a.ID(), "actual": selected, "ok": err == nil && selected == a.ID()})
		c.request(h, "fill-first-quota", "GET", "/backend-api/wham/usage", nil, "")
		verify(a.ID(), pool.FillFirst)
		h.Scheduler.Failure(a.ID(), "excluded only inside validation process")
	}
	h = c.handler(nil)
	observe(h)
	// A private validation URL exercises the configurable RR mapping without
	// mutating the running server or spending image/compaction quota.
	routes := map[string]pool.Route{}
	for k, v := range h.Config.RoundRobin {
		routes[k] = v
	}
	routes["/_pool/rr/live-check-usage"] = pool.Route{Path: "/backend-api/wham/usage"}
	h.Config.RoundRobin = routes
	sort.Slice(want, func(i, j int) bool { return want[i].ID() < want[j].ID() })
	for i := range 2 * len(c.accounts) {
		c.request(h, "round-robin-quota", "GET", "/_pool/rr/live-check-usage", nil, "")
		verify(want[i%len(want)].ID(), pool.RoundRobin)
	}
}

func (c *check) refresh() {
	for _, old := range c.accounts {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		fresh, err := c.store.Token(ctx, old.ID(), true)
		cancel()
		if err != nil {
			c.emit(map[string]any{"check": "refresh", "auth_index": old.ID(), "ok": false})
			continue
		}
		list, err := c.store.List()
		persisted := false
		for _, a := range list {
			if a.ID() == fresh.ID() {
				persisted = a == fresh
			}
		}
		c.emit(map[string]any{"check": "refresh", "auth_index": old.ID(), "ok": err == nil && persisted, "access_changed": old.AccessToken != fresh.AccessToken, "refresh_changed": old.RefreshToken != fresh.RefreshToken, "id_token_changed": old.IDToken != fresh.IDToken, "id_token_present": fresh.IDToken != "", "identity_unchanged": fresh.ID() == old.ID(), "persisted": persisted})
		c.request(c.handler(&fresh), "quota-after-refresh", "GET", "/backend-api/wham/usage", nil, "")
	}
}

func (c *check) responseBody(input any) any {
	if input == nil {
		input = []any{map[string]any{"role": "user", "content": "Reply with exactly POOL_LIVE_OK."}}
	}
	return map[string]any{"model": c.model, "instructions": "Follow the user's short request.", "input": input, "stream": true, "store": false, "reasoning": map[string]any{"effort": "low"}}
}

func (c *check) compact() {
	h := c.handler(nil)
	input := []any{map[string]any{"role": "user", "content": "Remember the marker POOL_LIVE_OK."}, map[string]any{"role": "assistant", "content": "I will remember POOL_LIVE_OK."}}
	body := map[string]any{"model": c.model, "input": input, "instructions": "Preserve the marker.", "parallel_tool_calls": true}
	output, ok := c.request(h, "normal-compact", "POST", "/backend-api/codex/responses/compact", body, "")
	if ok {
		var items []any
		if json.Unmarshal(output, &items) == nil {
			items = append(items, map[string]any{"role": "user", "content": "Reply with exactly the remembered marker."})
			c.request(h, "continue-after-compact", "POST", "/backend-api/codex/responses", c.responseBody(items), "POOL_LIVE_OK")
		}
	}
	for range len(c.accounts) {
		c.request(h, "round-robin-compact", "POST", "/_pool/rr/responses/compact", body, "")
	}
}

func (c *check) compactV2() {
	h := c.handler(nil)
	// This validation-only mapping exercises the existing configurable router.
	h.Config.RoundRobin = map[string]pool.Route{"/_pool/rr/responses": {Path: "/backend-api/codex/responses"}}
	input := []any{map[string]any{"role": "user", "content": "Remember the marker POOL_LIVE_OK."}, map[string]any{"role": "assistant", "content": "I will remember POOL_LIVE_OK."}, map[string]any{"type": "compaction_trigger"}}
	for _, path := range []string{"/backend-api/codex/responses", "/_pool/rr/responses"} {
		output, ok := c.request(h, "compact-v2", "POST", path, c.responseBody(input), "")
		if !ok {
			continue
		}
		var items []map[string]any
		_ = json.Unmarshal(output, &items)
		var compacted []any
		for _, item := range items {
			if item["type"] == "compaction" || item["type"] == "context_compaction" || item["type"] == "compaction_summary" {
				if value, ok := item["encrypted_content"].(string); ok && value != "" {
					compacted = append(compacted, item)
				}
			}
		}
		c.emit(map[string]any{"check": "compact-v2-output", "path": path, "ok": len(compacted) > 0})
		if len(compacted) == 0 {
			continue
		}
		compacted = append(compacted, map[string]any{"role": "user", "content": "Reply with exactly the remembered marker."})
		// All four accounts consume the same returned compacted state. This tests
		// actual cross-account continuity without changing the live scheduler.
		for _, account := range c.accounts {
			c.request(c.handler(&account), "continue-compact-v2-"+account.ID(), "POST", "/backend-api/codex/responses", c.responseBody(compacted), "POOL_LIVE_OK")
		}
	}
}

func (c *check) images() {
	if c.output == "" {
		panic("images requires --output for private artifacts")
	}
	if err := os.MkdirAll(c.output, 0700); err != nil {
		panic("cannot create artifact directory")
	}
	h := c.handler(nil)
	body := map[string]any{"model": "gpt-image-2", "prompt": "A plain blue circle centered on a white square background. No text.", "quality": "low", "size": "1024x1024", "n": 1}
	for _, path := range []string{"/backend-api/codex/images/generations", "/_pool/rr/images/generations"} {
		data, ok := c.request(h, "image-generation", "POST", path, body, "")
		if !ok {
			continue
		}
		var obj struct {
			Data []struct {
				Base64 string `json:"b64_json"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &obj) != nil || len(obj.Data) == 0 {
			c.emit(map[string]any{"check": "image-artifact", "ok": false})
			continue
		}
		if !c.imageArtifact(obj.Data[0].Base64, "generation") {
			continue
		}
		edit := map[string]any{"model": "gpt-image-2", "prompt": "Change the circle to red. Keep the white background.", "images": []any{map[string]string{"image_url": "data:image/png;base64," + obj.Data[0].Base64}}, "quality": "low", "size": "1024x1024", "n": 1}
		editPath := "/backend-api/codex/images/edits"
		if strings.HasPrefix(path, "/_pool/") {
			editPath = "/_pool/rr/images/edits"
		}
		data, ok = c.request(h, "image-edit", "POST", editPath, edit, "")
		if ok {
			obj.Data = nil
			if json.Unmarshal(data, &obj) != nil || len(obj.Data) == 0 {
				c.emit(map[string]any{"check": "edit-artifact", "ok": false})
			} else {
				c.imageArtifact(obj.Data[0].Base64, "edit")
			}
		}
	}
}

func (c *check) imageArtifact(encoded, label string) bool {
	data, err := base64.StdEncoding.DecodeString(encoded)
	valid := false
	width, height := 0, 0
	if err == nil {
		im, decodeErr := png.Decode(bytes.NewReader(data))
		if decodeErr == nil {
			width, height = im.Bounds().Dx(), im.Bounds().Dy()
			valid = width > 0 && height > 0
		}
	}
	name := fmt.Sprintf("%s-%d.png", label, time.Now().UnixNano())
	if valid {
		err = os.WriteFile(filepath.Join(c.output, name), data, 0600)
	}
	c.emit(map[string]any{"check": label + "-artifact", "ok": valid && err == nil, "file": name, "bytes": len(data), "width": width, "height": height, "matches_requested_size": width == 1024 && height == 1024})
	return valid && err == nil
}
