package pool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRRConfiguredHeadersOverrideCallerOnlyOnRR(t *testing.T) {
	h, accounts := relayFixture(t, 90)
	path := "/_pool/rr/responses"
	route := h.Config.RoundRobin[path]
	route.Headers = map[string]string{"User-Agent": "captured-codex-agent", "originator": "codex-tui", "Accept": "text/event-stream"}
	h.Config.RoundRobin[path] = route
	for _, rr := range []bool{true, false} {
		h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			wantUA, wantOrigin := "Python-urllib/3.12", "caller"
			if rr {
				wantUA, wantOrigin = "captured-codex-agent", "codex-tui"
			}
			if r.Header.Get("User-Agent") != wantUA || r.Header.Get("originator") != wantOrigin {
				t.Fatal(r.Header.Get("User-Agent"), r.Header.Get("originator"))
			}
			if r.Header.Get("Chatgpt-Account-Id") != accounts[0].AccountID || r.Header.Get("Authorization") == "Bearer client-secret" {
				t.Fatal("pool authentication was not retained")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"input":[{"type":"compaction_trigger"}]}` {
				t.Fatal("body changed")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})
		target := path
		if !rr {
			target = "/backend-api/codex/responses"
		}
		req := httptest.NewRequest("POST", target, strings.NewReader(`{"input":[{"type":"compaction_trigger"}]}`))
		req.Header.Set("Authorization", "Bearer client-secret")
		req.Header.Set("User-Agent", "Python-urllib/3.12")
		req.Header.Set("originator", "caller")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
}

func TestRRHeaderConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		header string
		valid  bool
	}{
		{`"User-Agent":"codex-tui/0.155.1","originator":"codex-tui"`, true},
		{`"Authorization":"secret"`, false},
		{`"User-Agent":"bad\r\nheader"`, false},
	} {
		p := filepath.Join(t.TempDir(), "pool.json")
		os.WriteFile(p, []byte(`{"round_robin_endpoints":{"/_pool/rr/responses":{"upstream_path":"/backend-api/codex/responses","headers":{`+tc.header+`}}}}`), 0600)
		c, err := LoadConfig(p)
		if (err == nil) != tc.valid {
			t.Fatalf("valid=%v err=%v", tc.valid, err)
		}
		if tc.valid && c.RoundRobin["/_pool/rr/responses"].Headers["originator"] != "codex-tui" {
			t.Fatal("headers not loaded")
		}
	}
}
