package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/cpa"

	"github.com/gorilla/websocket"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func relayFixture(t *testing.T, remaining ...float64) (*Handler, []Credential) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.StateDir = t.TempDir()
	store, err := OpenStore(cfg.StateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, store, nil, "client-secret", "admin-secret")
	h.Publish = func(context.Context, Record) {}
	var accounts []Credential
	for i := range remaining {
		c := credential(fmt.Sprintf("account-%d", i))
		if _, err = store.Login(c); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, c)
	}
	h.Scheduler.Sync(accounts)
	for i, r := range remaining {
		q := quota(r, time.Duration(i+1)*time.Hour)
		q.Observed = time.Now()
		q.Weekly.Reset = time.Now().Add(time.Duration(i+1) * time.Hour)
		h.Scheduler.Observe(accounts[i].ID(), q)
	}
	return h, accounts
}
func call(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAllNormalEndpointsUseFillFirstAndOnlyCustomURLsUseRoundRobin(t *testing.T) {
	h, accounts := relayFixture(t, 5, 60, 70)
	var mu sync.Mutex
	var selected []string
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		selected = append(selected, r.Header.Get("Chatgpt-Account-Id"))
		mu.Unlock()
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	for _, path := range []string{"/backend-api/codex/responses", "/backend-api/codex/images/generations", "/backend-api/codex/images/edits", "/backend-api/codex/responses/compact", "/backend-api/codex/future", "/future-api/anything", "/_pool/rr/unconfigured"} {
		for range 3 {
			w := call(h, "POST", path, `{"unknown":"kept"}`)
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			if selected[len(selected)-1] != accounts[1].AccountID {
				t.Fatal("normal path was not weekly fill-first")
			}
		}
	}
	for path := range h.Config.RoundRobin {
		selected = nil
		for range 6 {
			body := `{"input":[{"type":"compaction_trigger"}],"future":"unchanged"}`
			if path == "/_pool/rr/images/generations" {
				body = `{"prompt":"test"}`
			}
			if path == "/_pool/rr/images/edits" {
				body = `{"prompt":"test","images":[{"image_url":"data:image/png;base64,AA=="}]}`
			}
			w := call(h, "POST", path, body)
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
		}
		counts := map[string]int{}
		for _, id := range selected {
			counts[id]++
		}
		for _, a := range accounts {
			if counts[a.AccountID] != 2 {
				t.Fatalf("RR did not include reserve account: %v", counts)
			}
		}
	}
}

func TestEveryRequestUsesCurrentSelectedCredentialsAfterRefresh(t *testing.T) {
	h, accounts := relayFixture(t, 90, 80)
	for i := range accounts {
		accounts[i].IDToken = "id-token-" + accounts[i].AccountID
		if _, err := h.Store.Login(accounts[i]); err != nil {
			t.Fatal(err)
		}
	}
	refreshCalls := 0
	otherStore, err := OpenStore(h.Store.Dir, func(_ context.Context, refreshToken string) (*cpa.CodexTokenData, error) {
		refreshCalls++
		if refreshToken != accounts[0].RefreshToken {
			t.Fatal("refresh used another account's credential")
		}
		return &cpa.CodexTokenData{AccountID: accounts[0].AccountID, AccessToken: "rotated-access", RefreshToken: "rotated-refresh", IDToken: "rotated-id-token", Expire: time.Now().Add(time.Hour).Format(time.RFC3339)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := accounts[0]
	upstreamCalls := 0
	responseBody := " {\"rate_limit\":{\"future\":true},\"unknown\":1} \n"
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamCalls++
		if r.Header.Get("Authorization") != "Bearer "+want.AccessToken || r.Header.Get("Chatgpt-Account-Id") != want.AccountID {
			t.Fatal("request used stale credentials or mixed accounts")
		}
		for _, values := range r.Header {
			for _, value := range values {
				if strings.Contains(value, want.IDToken) || strings.Contains(value, want.RefreshToken) {
					t.Fatal("identity or refresh token was added to normal API headers")
				}
			}
		}
		if r.Method != "GET" || r.URL.RequestURI() != "/backend-api/wham/usage?future=unchanged" {
			t.Fatal("quota request changed")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(responseBody))}, nil
	})
	for step := range 3 {
		switch step {
		case 1:
			want, err = otherStore.Token(context.Background(), accounts[0].ID(), true)
			if err != nil || want.IDToken != "rotated-id-token" {
				t.Fatal("refresh did not update the stored ID token")
			}
		case 2:
			q := quota(5, time.Hour)
			q.Observed = time.Now()
			q.Weekly.Reset = time.Now().Add(time.Hour)
			h.Scheduler.Observe(accounts[0].ID(), q)
			want = accounts[1]
		}
		w := call(h, "GET", "/backend-api/wham/usage?future=unchanged", "")
		if w.Code != 200 || w.Body.String() != responseBody {
			t.Fatal("quota response was not forwarded unchanged")
		}
	}
	if upstreamCalls != 3 || refreshCalls != 1 {
		t.Fatalf("upstream calls=%d refresh calls=%d", upstreamCalls, refreshCalls)
	}
}

func TestOpaquePayloadHeadersPathQueryAndErrorsPassThrough(t *testing.T) {
	h, accounts := relayFixture(t, 90)
	requestBody := " {\"model\":\"future-model\",\"tools\":[{\"type\":\"future-tool\"}],\"unknown\":[1,2]} \n"
	responseBody := "event: never-seen-before\ndata: {\"unknown\":true}\n\ndata: [DONE]\n\n"
	for _, status := range []int{200, 302, 401, 403, 429, 500} {
		// Restore a fresh quota between independent error scenarios.
		q := quota(90, time.Hour)
		q.Observed = time.Now()
		q.Weekly.Reset = time.Now().Add(time.Hour)
		h.Scheduler.ObserveVerified(accounts[0].ID(), q)
		h.Scheduler.Cooldown(accounts[0].ID(), "/backend-api/codex/responses", time.Time{})
		calls := 0
		h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			b, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || r.URL.String() != "https://chatgpt.com/backend-api/codex/responses?x=a%2Fb&x=2" || !bytes.Equal(b, []byte(requestBody)) {
				t.Fatalf("request changed %s %q", r.URL, b)
			}
			if r.Header.Get("Authorization") != "Bearer "+accounts[0].AccessToken || r.Header.Get("Chatgpt-Account-Id") != accounts[0].AccountID {
				t.Fatal("credential pair wrong")
			}
			for _, k := range []string{"Cookie", "X-OpenAI-Actor", "OpenAI-Project", "X-Workspace-ID", "X-Hop", "X-API-Key"} {
				if r.Header.Get(k) != "" {
					t.Fatalf("identity/header leak %s", k)
				}
			}
			if r.Header.Get("X-Unknown-Feature") != "preserved" {
				t.Fatal("unknown header dropped")
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}, "Set-Cookie": {"upstream"}, "Location": {"https://untrusted.invalid"}}, Body: io.NopCloser(strings.NewReader(responseBody))}, nil
		})
		r := httptest.NewRequest("POST", "/backend-api/codex/responses?x=a%2Fb&x=2", strings.NewReader(requestBody))
		r.Header.Set("Authorization", "Bearer client-secret")
		for _, k := range []string{"Cookie", "X-OpenAI-Actor", "OpenAI-Project", "X-Workspace-ID", "X-Hop", "X-API-Key"} {
			r.Header.Set(k, "original-secret")
		}
		r.Header.Set("Connection", "X-Hop")
		r.Header.Set("X-Unknown-Feature", "preserved")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status || w.Body.String() != responseBody || calls != 1 || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("response changed/retried: %d calls=%d", w.Code, calls)
		}
	}
}

func TestUnknownMetadataIsPreservedAndNativeSessionHeadersAreObserved(t *testing.T) {
	h, _ := relayFixture(t, 90)
	var records []Record
	h.Publish = func(_ context.Context, r Record) { records = append(records, r) }
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		for _, name := range []string{"X-Scale-Factor", "X-Future-Project-Feature", "Session-Id", "Thread-Id"} {
			if r.Header.Get(name) != "unchanged" {
				t.Fatalf("non-identity header %s was changed", name)
			}
		}
		if r.URL.RawQuery != "future_project_feature=unchanged" {
			t.Fatal("non-identity query was changed")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	r := httptest.NewRequest("POST", "/backend-api/codex/responses?future_project_feature=unchanged", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer client-secret")
	for _, name := range []string{"X-Scale-Factor", "X-Future-Project-Feature", "Session-Id", "Thread-Id"} {
		r.Header.Set(name, "unchanged")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || len(records) != 1 || records[0].SessionID != "unchanged" || records[0].ThreadID != "unchanged" {
		t.Fatal("native session metadata was lost")
	}
}

func TestLargeAndEncodedBodiesAreNotNormalized(t *testing.T) {
	h, _ := relayFixture(t, 90)
	for _, body := range []string{strings.Repeat("x", observationLimit+1000), "\x1f\x8b\x00binary-body"} {
		h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			if !bytes.Equal(b, []byte(body)) {
				t.Fatal("large/compressed body changed")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"gzip"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		w := call(h, "POST", "/backend-api/codex/images/edits", body)
		if w.Code != 200 || w.Body.String() != body {
			t.Fatal("encoded response changed")
		}
	}
}

func TestWrongKeyNeverContactsUpstream(t *testing.T) {
	h, _ := relayFixture(t, 90)
	h.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected upstream contact")
		return nil, errors.New("forbidden")
	})
	r := httptest.NewRequest("POST", "/backend-api/codex/responses", nil)
	r.Header.Set("Authorization", "Bearer admin-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("admin key accepted as client")
	}
	if w = call(h, "GET", "/_pool/status", ""); w.Code != 401 {
		t.Fatal("client key accepted as admin")
	}
}

func TestOrdinaryRequestsPreserveMethodRawTargetAndOpaqueBodyOnWire(t *testing.T) {
	h, accounts := relayFixture(t, 5, 80, 90)
	type seenRequest struct{ method, target, body, account string }
	seen := make(chan seenRequest, 1)
	const response = "opaque upstream response\x00\n"
	attachUpstream(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		seen <- seenRequest{r.Method, r.RequestURI, string(body), r.Header.Get("Chatgpt-Account-Id")}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(422)
		_, _ = io.WriteString(w, response)
	}))
	server := httptest.NewServer(h)
	defer server.Close()
	for _, tc := range []struct{ method, target, body string }{
		{"POST", "/backend-api/codex/future", "not JSON"},
		{"GET", "/backend-api/me?account_id=opaque&token=opaque", ""},
		{"PATCH", "/future-api/%66eature/%2fresource/../leaf?x=1;access_token=opaque&x=%zz&&x=2", "\x00\xffopaque"},
		{"DELETE", "/backend-api/files/future/sub/resource", "{malformed"},
		{"OPTIONS", "/not-a-known-api?bare&empty=&x=+&x=%20", ""},
		{"FUTURE", "/backend-api/codex/%72esponses?actor=opaque&workspace=opaque", " {\"tools\":[{\"type\":\"future\"}]}\n"},
		{"POST", "//untrusted.invalid/path?opaque=true", "URL-looking paths remain paths"},
	} {
		t.Run(tc.method+tc.target, func(t *testing.T) {
			r, err := http.NewRequest(tc.method, server.URL+tc.target, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer client-secret")
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != 422 || string(body) != response {
				t.Fatalf("upstream response changed: status=%d body=%q error=%v", resp.StatusCode, body, err)
			}
			select {
			case got := <-seen:
				want := seenRequest{tc.method, tc.target, tc.body, accounts[1].AccountID}
				if got != want {
					t.Fatalf("request changed: got=%+v want=%+v", got, want)
				}
			default:
				t.Fatal("request did not reach upstream")
			}
		})
	}
}

func TestDedicatedRoundRobinURLOnlyChangesPathAndAccount(t *testing.T) {
	h, accounts := relayFixture(t, 5, 60, 70)
	h.Config.RoundRobin = map[string]Route{"/_pool/rr/future": {Path: "/future-api/operation"}}
	counts := map[string]int{}
	const body = "\x00not JSON\n"
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "PATCH" || r.URL.String() != "https://chatgpt.com/future-api/operation?token=opaque&x=%zz;unchanged" || string(b) != body {
			t.Fatalf("dedicated request changed unexpectedly: %s %s %q", r.Method, r.URL, b)
		}
		counts[r.Header.Get("Chatgpt-Account-Id")]++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	for range 6 {
		if w := call(h, "PATCH", "/_pool/rr/future?token=opaque&x=%zz;unchanged", body); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	for _, account := range accounts {
		if counts[account.AccountID] != 2 {
			t.Fatalf("dedicated URL did not rotate through the reserve: %v", counts)
		}
	}
}

func TestRequestTrailersPreserveMetadataAndRemoveCredentials(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			h, accounts := relayFixture(t, 90)
			seen := make(chan http.Header, 1)
			attachUpstream(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "opaque request body" {
					t.Error("request body changed")
				}
				if r.Header.Get("Chatgpt-Account-Id") != accounts[0].AccountID {
					t.Error("incorrect selected identity")
				}
				seen <- r.Trailer.Clone()
				w.Header().Add("Trailer", "X-Upstream-Receipt")
				_, _ = io.WriteString(w, "opaque response body")
				w.Header().Set("X-Upstream-Receipt", "receipt")
			}))
			server := httptest.NewUnstartedServer(h)
			server.EnableHTTP2 = http2
			server.StartTLS()
			defer server.Close()
			r, err := http.NewRequest("POST", server.URL+"/future-api/trailers", strings.NewReader("opaque request body"))
			if err != nil {
				t.Fatal(err)
			}
			r.ContentLength = -1
			r.Header.Set("Authorization", "Bearer client-secret")
			r.Trailer = http.Header{"X-Future-Checksum": {"one", "two"}, "Chatgpt-Account-Id": {"original-account"}, "X-Openai-Actor": {"original-actor"}}
			resp, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if (resp.ProtoMajor == 2) != http2 {
				t.Fatal("test used the wrong HTTP version")
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 200 || string(body) != "opaque response body" || resp.Trailer.Get("X-Upstream-Receipt") != "receipt" {
				t.Fatal("response body or trailers changed")
			}
			got := <-seen
			if values := got.Values("X-Future-Checksum"); len(values) != 2 || values[0] != "one" || values[1] != "two" {
				t.Fatalf("request metadata trailers were dropped: %v", got)
			}
			if got.Get("Chatgpt-Account-Id") != "" || got.Get("X-Openai-Actor") != "" {
				t.Fatal("original credentials survived in request trailers")
			}
		})
	}
}

func attachUpstream(t *testing.T, h *Handler, handler http.Handler) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	target, _ := url.Parse(up.URL)
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "chatgpt.com" || r.URL.Scheme != "https" {
			t.Error("upstream authority was not fixed")
		}
		copy := r.Clone(r.Context())
		// The body populates this map at EOF; the test transport must retain
		// the same live trailers as the original outgoing request.
		copy.Trailer = r.Trailer
		u := *r.URL
		copy.URL = &u
		u.Host = target.Host
		u.Scheme = target.Scheme
		return http.DefaultTransport.RoundTrip(copy)
	})
}

func TestSSEFlushAndCancel(t *testing.T) {
	h, _ := relayFixture(t, 90)
	done := make(chan struct{})
	attachUpstream(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: early\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(done)
	}))
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/backend-api/codex/responses", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer client-secret")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b := make([]byte, len("data: early\n\n"))
	if _, err = io.ReadFull(resp.Body, b); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not cancelled")
	}
}

func TestWebSocketPayloadPingCloseAndSingleAccount(t *testing.T) {
	h, accounts := relayFixture(t, 90, 90)
	seen := make(chan string, 1)
	closed := make(chan struct{})
	attachUpstream(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Chatgpt-Account-Id")
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(); close(closed) }()
		for {
			typ, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err = c.WriteMessage(typ, b); err != nil {
				return
			}
		}
	}))
	server := httptest.NewServer(h)
	defer server.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/backend-api/codex/responses", http.Header{"Authorization": {"Bearer client-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	pong := false
	c.SetPongHandler(func(s string) error { pong = s == "native-ping"; return nil })
	if err = c.WriteControl(websocket.PingMessage, []byte("native-ping"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`{"type":"response.create","model":"future","unknown":[1,2]}`, `{"type":"response.cancel","response_id":"old"}`} {
		if err = c.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		_, got, err := c.ReadMessage()
		if err != nil || string(got) != payload {
			t.Fatalf("websocket changed: %q %v", got, err)
		}
	}
	if !pong {
		t.Fatal("pong lost")
	}
	if got := <-seen; got != accounts[0].AccountID {
		t.Fatal("websocket did not use fill-first")
	}
	if err = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, "done"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != 1000 {
		t.Fatalf("close lost: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream websocket did not close")
	}
}

func TestDedicatedRoundRobinWebSocketKeepsAccountWithinEachConnection(t *testing.T) {
	h, accounts := relayFixture(t, 5, 60, 70)
	h.Config.RoundRobin = map[string]Route{"/_pool/rr/future-ws": {Path: "/future-api/websocket"}}
	selected := make(chan string, 6)
	attachUpstream(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/future-api/websocket" {
			t.Error("dedicated websocket did not use configured path")
		}
		selected <- r.Header.Get("Chatgpt-Account-Id")
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			kind, body, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(kind, body); err != nil {
				return
			}
		}
	}))
	server := httptest.NewServer(h)
	defer server.Close()
	counts := map[string]int{}
	for range 6 {
		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/_pool/rr/future-ws", http.Header{"Authorization": {"Bearer client-secret"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		for _, body := range []string{"first opaque message", "second opaque message"} {
			if err := c.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
				t.Fatal(err)
			}
			_, got, err := c.ReadMessage()
			if err != nil || string(got) != body {
				t.Fatalf("dedicated websocket did not relay: %q %v", got, err)
			}
		}
		_ = c.Close()
		counts[<-selected]++
	}
	for _, account := range accounts {
		if counts[account.AccountID] != 2 {
			t.Fatalf("websocket connections did not round-robin through the reserve: %v", counts)
		}
	}
}

func TestWebSocketRechecksWeeklyReserveAndUpstreamLimitsBeforeNextMessage(t *testing.T) {
	for _, event := range []string{
		`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":90,"window_minutes":10080,"reset_after_seconds":3600}},"future":true}`,
		`{"type":"error","status":401,"error":{"code":"token_expired"},"future":true}`,
		`{"type":"error","status_code":429,"headers":{"retry-after":"90"},"error":{"code":"rate_limit"},"future":true}`,
	} {
		t.Run(event, func(t *testing.T) {
			h, accounts := relayFixture(t, 90, 90)
			requests := make(chan string, 10)
			closed := make(chan string, 10)
			attachUpstream(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := r.Header.Get("Chatgpt-Account-Id")
				up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				c, err := up.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = c.Close(); closed <- id }()
				for {
					_, payload, err := c.ReadMessage()
					if err != nil {
						return
					}
					requests <- id + ":" + string(payload)
					if err := c.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
						return
					}
				}
			}))
			server := httptest.NewServer(h)
			defer server.Close()
			dial := func() *websocket.Conn {
				c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/backend-api/codex/responses", http.Header{"Authorization": {"Bearer client-secret"}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				return c
			}
			waitClosed := func(expected string) {
				select {
				case id := <-closed:
					if id != expected {
						t.Fatal("wrong upstream connection closed")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("upstream connection stayed open after rejected message")
				}
			}
			c := dial()
			if err := c.WriteMessage(websocket.TextMessage, []byte("first")); err != nil {
				t.Fatal(err)
			}
			_, received, err := c.ReadMessage()
			if err != nil || string(received) != event {
				t.Fatal("upstream quota/error event changed")
			}
			// This message must not be relayed to A or replayed onto B.
			_ = c.WriteMessage(websocket.TextMessage, []byte("rejected"))
			if _, _, err := c.ReadMessage(); err == nil {
				t.Fatal("existing websocket kept sending after account became unavailable")
			}
			waitClosed(accounts[0].AccountID)
			_ = c.Close()
			c = dial()
			if err := c.WriteMessage(websocket.TextMessage, []byte("fresh-connection")); err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.ReadMessage(); err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
			waitClosed(accounts[1].AccountID)
			if len(requests) != 2 || <-requests != accounts[0].AccountID+":first" || <-requests != accounts[1].AccountID+":fresh-connection" {
				t.Fatal("message replayed, wrong account used, or reserve message reached upstream")
			}
		})
	}
}

func TestHTTP401QuotaHeadersDoNotClearAuthenticationFailure(t *testing.T) {
	h, _ := relayFixture(t, 90)
	calls := 0
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 401, Header: http.Header{
			"X-Codex-Primary-Used-Percent":        {"1"},
			"X-Codex-Primary-Window-Minutes":      {"10080"},
			"X-Codex-Primary-Reset-After-Seconds": {"3600"},
		}, Body: io.NopCloser(strings.NewReader("unauthorized"))}, nil
	})
	if w := call(h, "POST", "/backend-api/codex/responses", "{}"); w.Code != 401 {
		t.Fatal("upstream error changed")
	}
	if w := call(h, "POST", "/backend-api/codex/responses", "{}"); w.Code != 503 || calls != 1 {
		t.Fatal("401 quota headers made failed credentials selectable")
	}
}
