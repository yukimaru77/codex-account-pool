package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/cpa"
	"codex-account-pool/internal/pool"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResponseChecksRequireCompletionAndPreserveStreamedOutput(t *testing.T) {
	const delta = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"POOL_LIVE_OK\"}\n\n"
	const item = "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"opaque\"}}\n\n"
	const completed = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	for _, tc := range []struct {
		name, body, expected string
		status               int
		ok                   bool
	}{
		{"plain content type SSE", delta + item + completed, "POOL_LIVE_OK", 200, true},
		{"missing terminal event", delta + item, "POOL_LIVE_OK", 200, false},
		{"wrong text", delta + completed, "WRONG", 200, false},
		{"upstream error", `{"detail":"Not Found"}`, "", 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pool.DefaultConfig()
			cfg.StateDir = t.TempDir()
			s, err := pool.OpenStore(cfg.StateDir, nil)
			if err != nil {
				t.Fatal(err)
			}
			a := pool.Credential{CodexTokenData: cpa.CodexTokenData{AccountID: "fixture", AccessToken: "access-fixture", RefreshToken: "refresh-fixture", Expire: time.Now().Add(time.Hour).Format(time.RFC3339)}}
			if _, err = s.Login(a); err != nil {
				t.Fatal(err)
			}
			h := pool.NewHandler(cfg, s, transportFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}), "live-check-client", "live-check-admin")
			h.Publish = func(context.Context, pool.Record) {}
			h.Scheduler.Sync([]pool.Credential{a})
			h.Scheduler.Observe(a.ID(), pool.Quota{Allowed: true, Observed: time.Now(), Weekly: pool.Window{Seconds: 604800, Reset: time.Now().Add(time.Hour)}})
			c := &check{accounts: []pool.Credential{a}}
			output, ok := c.request(h, tc.name, "POST", "/backend-api/codex/responses", nil, tc.expected)
			if ok != tc.ok {
				t.Fatalf("ok=%t", ok)
			}
			if tc.ok && !bytes.Contains(output, []byte(`"encrypted_content":"opaque"`)) {
				t.Fatalf("streamed compaction output lost: %s", output)
			}
			if (c.failed == 0) != tc.ok {
				t.Fatal("failure not counted")
			}
		})
	}
}

func TestImageArtifactRequiresDecodableImageAndRecordsActualSize(t *testing.T) {
	c := &check{output: t.TempDir()}
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1254, 1254))); err != nil {
		t.Fatal(err)
	}
	if !c.imageArtifact(base64.StdEncoding.EncodeToString(b.Bytes()), "generation") {
		t.Fatal("valid upstream image rejected")
	}
	if c.imageArtifact(base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n")), "edit") {
		t.Fatal("truncated PNG accepted")
	}
	files, err := filepath.Glob(filepath.Join(c.output, "*.png"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	info, err := os.Stat(files[0])
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("artifact permissions")
	}
}
