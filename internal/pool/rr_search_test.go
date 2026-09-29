package pool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStandaloneSearchRoundRobinPreservesNativeRequest(t *testing.T) {
	h, _ := relayFixture(t, 90, 90)
	body := `{"id":"search-session","model":"gpt-5.6-luna","commands":{"search_query":[{"q":"a paper title"}]},"settings":{"external_web_access":true}}`
	seen := []string{}
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/backend-api/codex/alpha/search" || r.Method != "POST" {
			t.Fatalf("incorrect native search route: %s %s", r.Method, r.URL.Path)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != body {
			t.Fatal("native search payload was changed")
		}
		if r.Header.Get("Authorization") == "Bearer client-secret" {
			t.Fatal("pool credential leaked upstream")
		}
		seen = append(seen, r.Header.Get("Chatgpt-Account-Id"))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":"search result","results":[]}`))}, nil
	})
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/_pool/rr/alpha/search", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer client-secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "search result") {
			t.Fatalf("unexpected search response: %d %s", w.Code, w.Body.String())
		}
	}
	if seen[0] == seen[1] {
		t.Fatal("standalone search did not rotate accounts")
	}
}
