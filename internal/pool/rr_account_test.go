package pool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRRSelectsEachRequestAndIgnoresClientPin(t *testing.T) {
	h, accounts := relayFixture(t, 90, 90)
	seen := []string{}
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Pool-Account") != "" {
			t.Fatal("pool routing header leaked upstream")
		}
		seen = append(seen, r.Header.Get("Chatgpt-Account-Id"))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	for i := 0; i < 4; i++ {
		r := httptest.NewRequest("POST", "/_pool/rr/responses", strings.NewReader(`{"prompt":"test"}`))
		r.Header.Set("Authorization", "Bearer client-secret")
		r.Header.Set("X-Pool-Account", accounts[1].ID())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("request %d returned %d", i, w.Code)
		}
	}
	if len(seen) != 4 || seen[0] == seen[1] || seen[1] == seen[2] || seen[2] == seen[3] {
		t.Fatalf("RR did not rotate each request: %v", seen)
	}
}
