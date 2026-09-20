package pool

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestImageRRContract(t *testing.T) {
	h, _ := relayFixture(t, 90)
	calls := 0
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]string{"model": "gpt-image-2", "quality": "auto", "size": "auto", "background": "auto", "prompt": "test"} {
			if b[k] != want {
				t.Fatalf("%s = %v", k, b[k])
			}
		}
		if _, ok := b["n"]; ok {
			t.Fatal("n must be omitted")
		}
		if r.URL.Path != "/backend-api/codex/images/generations" && r.URL.Path != "/backend-api/codex/images/edits" {
			t.Fatal(r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"b64_json":"unchanged"}]}`))}, nil
	})
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"generations", `{"prompt":"test"}`, 200},
		{"edits", `{"prompt":"test","images":[{"image_url":"data:image/png;base64,AA=="}]}`, 200},
		{"generations", `{"prompt":"test","model":"gpt-image-2"}`, 400},
		{"generations", `{"prompt":"test","quality":"auto"}`, 400},
		{"generations", `{"prompt":"test","size":"auto"}`, 400},
		{"generations", `{"prompt":"test","background":"auto"}`, 400},
		{"generations", `{"prompt":"test","n":1}`, 400},
		{"generations", `{"prompt":"test"} {}`, 400},
		{"generations?model=other", `{"prompt":"test"}`, 400},
		{"edits", `{"prompt":"test","images":[]}`, 400},
		{"edits", `{"prompt":"test","images":[{"image_url":"x","extra":1}]}`, 400},
		{"generations", `{"prompt":" "}`, 400},
	} {
		w := call(h, "POST", "/_pool/rr/images/"+tc.path, tc.body)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, w.Code, w.Body.String())
		}
		if w.Code == 200 && w.Body.String() != `{"data":[{"b64_json":"unchanged"}]}` {
			t.Fatal("response changed")
		}
	}
	if calls != 2 {
		t.Fatalf("unexpected upstream requests: %d", calls)
	}
	if w := call(h, "GET", "/_pool/rr/images/generations", ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
	h.Config.RoundRobin["/_pool/rr/images/generations"] = Route{Path: "/new-image-api"}
	if w := call(h, "POST", "/_pool/rr/images/generations", `{"prompt":"test"}`); w.Code != 503 {
		t.Fatal(w.Code)
	}
}
