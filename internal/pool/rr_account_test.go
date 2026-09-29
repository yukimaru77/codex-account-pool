package pool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRRExplicitAccount(t *testing.T) {
	h, accounts := relayFixture(t, 90, 90)
	seen := []string{}
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Pool-Account") != "" {
			t.Fatal("pool routing header leaked upstream")
		}
		seen = append(seen, r.Header.Get("Chatgpt-Account-Id"))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	request := func(path string, ids ...string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"prompt":"test"}`))
		r.Header.Set("Authorization", "Bearer client-secret")
		for _, id := range ids {
			r.Header.Add("X-Pool-Account", id)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, path := range []string{"/_pool/rr/responses", "/_pool/rr/images/generations", "/_pool/rr/responses/compact"} {
		for i := 0; i < 2; i++ {
			if code := request(path, accounts[1].ID()); code != 200 || seen[len(seen)-1] != accounts[1].AccountID {
				t.Fatalf("fixed route %s returned %d: %v", path, code, seen)
			}
		}
	}
	// A fixed choice must not change the automatic RR cursor.
	if len(h.Scheduler.last) != 0 {
		t.Fatal("fixed requests changed the automatic cursor")
	}
	if code := request("/_pool/rr/responses"); code != 200 {
		t.Fatal(code)
	}
	previous := seen[len(seen)-1]
	if code := request("/_pool/rr/responses"); code != 200 || seen[len(seen)-1] == previous {
		t.Fatal("automatic RR did not rotate")
	}
	before := len(seen)
	for _, ids := range [][]string{{"unknown"}, {""}, {accounts[0].ID(), accounts[1].ID()}} {
		want := 400
		if ids[0] == "unknown" {
			want = 503
		}
		if code := request("/_pool/rr/responses", ids...); code != want {
			t.Fatalf("got %d want %d", code, want)
		}
	}
	h.Scheduler.Failure(accounts[1].ID(), "test unavailable")
	if code := request("/_pool/rr/responses", accounts[1].ID()); code != 503 || len(seen) != before {
		t.Fatal("unavailable fixed account fell back")
	}
	// The header has no account selection effect on native routes.
	if code := request("/backend-api/codex/responses", accounts[1].ID()); code != 200 || seen[len(seen)-1] != accounts[0].AccountID {
		t.Fatal("native selection changed")
	}
}
