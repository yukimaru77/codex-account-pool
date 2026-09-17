package pool

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/cpa"
)

type Detail struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}
type Failure struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"-"`
}
type Record struct {
	AuthID              string        `json:"auth_id"`
	AuthIndex           string        `json:"auth_index"`
	Endpoint            string        `json:"endpoint"`
	Policy              Policy        `json:"policy"`
	SessionID           string        `json:"session_id,omitempty"`
	ThreadID            string        `json:"thread_id,omitempty"`
	Model               string        `json:"model,omitempty"`
	ServiceTier         string        `json:"service_tier,omitempty"`
	ReasoningEffort     string        `json:"reasoning_effort,omitempty"`
	ResponseServiceTier string        `json:"response_service_tier,omitempty"`
	Generate            *bool         `json:"generate,omitempty"`
	Stream              bool          `json:"stream"`
	RequestedAt         time.Time     `json:"requested_at"`
	Latency             time.Duration `json:"latency_ns"`
	TTFT                time.Duration `json:"ttft_ns"`
	Failed              bool          `json:"failed"`
	Fail                Failure       `json:"failure"`
	Detail              Detail        `json:"tokens"`
	ResponseHeaders     http.Header   `json:"-"`
}

type Handler struct {
	Config    Config
	Store     *Store
	Scheduler *Scheduler
	Transport http.RoundTripper
	ClientKey string
	AdminKey  string
	Publish   func(context.Context, Record)
	logMu     sync.Mutex
}

func NewHandler(cfg Config, store *Store, transport http.RoundTripper, clientKey, adminKey string) *Handler {
	h := &Handler{Config: cfg, Store: store, Scheduler: NewScheduler(cfg), Transport: transport, ClientKey: clientKey, AdminKey: adminKey}
	h.Publish = h.writeRecord
	return h
}

func (h *Handler) writeRecord(_ context.Context, r Record) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	f, err := os.OpenFile(filepath.Join(h.Config.StateDir, "usage.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		log.Print("pool: usage log unavailable")
		return
	}
	defer func() { _ = f.Close() }()
	if err = json.NewEncoder(f).Encode(r); err != nil {
		log.Print("pool: usage record write failed")
	}
}

func authorized(r *http.Request, key string) bool {
	return key != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+key)) == 1
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_pool/status" {
		if !authorized(r, h.AdminKey) {
			http.Error(w, "invalid management credential", 401)
			return
		}
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.Scheduler.Status())
		return
	}
	if !authorized(r, h.ClientKey) {
		http.Error(w, "invalid pool credential", 401)
		return
	}
	path := r.URL.Path
	rawPath := r.URL.RawPath
	policy := FillFirst
	if route, ok := h.Config.RoundRobin[path]; ok {
		path = route.Path
		rawPath = ""
		policy = RoundRobin
	}
	id, err := h.Scheduler.Select(policy, path, "")
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	c, err := h.Store.Token(r.Context(), id, false)
	if err != nil {
		h.Scheduler.Failure(id, "credential unavailable; inspect login status")
		http.Error(w, "selected account credential unavailable", 503)
		return
	}
	generate := r.Method == "POST" && strings.HasPrefix(path, "/backend-api/codex/")
	o := newObservation(r.Context(), Record{AuthID: id, AuthIndex: id, Endpoint: r.URL.Path, Policy: policy, SessionID: r.Header.Get("Session-Id"), ThreadID: r.Header.Get("Thread-Id"), RequestedAt: time.Now(), Generate: &generate}, h.Publish)
	o.onEvent = func(b []byte) {
		headers := cpa.ParseCodexQuotaEventHeaders(b)
		if q, ok := QuotaFromHeaders(headers, time.Now()); ok {
			h.Scheduler.Observe(id, q)
		}
		switch cpa.CodexWebsocketErrorStatus(b) {
		case 401:
			h.Scheduler.Failure(id, "upstream authentication rejected; waiting for quota refresh")
		case 429:
			h.observeCooldown(id, path, headers)
		}
	}
	var requestBody *observedBody
	if r.Body != nil {
		requestBody = &observedBody{ReadCloser: r.Body, feed: o.request, finish: o.requestDone}
		r.Body = requestBody
	}
	defer o.finish()
	p := httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = "chatgpt.com"
			pr.Out.Host = "chatgpt.com"
			pr.Out.URL.Path = path
			pr.Out.URL.RawPath = rawPath
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			stripIdentity(pr.Out.Header)
			pr.Out.Header.Set("Authorization", "Bearer "+c.AccessToken)
			pr.Out.Header.Set("Chatgpt-Account-Id", c.AccountID)
			pr.Out.Trailer = pr.In.Trailer.Clone()
			if pr.Out.Trailer == nil {
				pr.Out.Trailer = make(http.Header)
			}
			stripIdentity(pr.Out.Trailer)
			if requestBody != nil {
				// Incoming trailer values are populated only after the body is
				// read. Copy them at EOF before the transport writes trailers.
				requestBody.onEOF = func() {
					for name, values := range pr.In.Trailer {
						if !identityHeader(name) {
							pr.Out.Trailer[name] = values
						}
					}
				}
			}
		},
		Transport: h.Transport, FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			o.failed(502)
			http.Error(w, "upstream connection failed", 502)
		},
		ModifyResponse: func(resp *http.Response) error {
			o.status(resp.StatusCode, resp.Header)
			if q, ok := QuotaFromHeaders(resp.Header, time.Now()); ok {
				h.Scheduler.Observe(id, q)
			}
			if resp.StatusCode == 401 {
				h.Scheduler.Failure(id, "upstream authentication rejected; waiting for quota refresh")
			}
			if resp.StatusCode == 429 {
				h.observeCooldown(id, path, resp.Header)
			}
			resp.Header.Del("Set-Cookie")
			resp.Header.Del("Set-Cookie2")
			if resp.StatusCode == 101 {
				rwc, ok := resp.Body.(io.ReadWriteCloser)
				if !ok {
					return errors.New("upgraded response is not duplex")
				}
				responses := newFrames(o.event)
				requests := newFrames(o.websocketRequest)
				resp.Body = &gatedWebsocket{
					ReadWriteCloser: &observedDuplex{ReadWriteCloser: rwc, feed: responses.feed, sent: requests.feed},
					check: func() error {
						// RR chooses at the HTTP upgrade. Checking availability must
						// not advance the cursor before the socket's first message.
						pinned := ""
						if policy == RoundRobin {
							pinned = id
						}
						selected, err := h.Scheduler.Select(policy, path, pinned)
						if err != nil {
							return err
						}
						if selected != id {
							return errors.New("account rotation requires a new websocket")
						}
						return nil
					},
				}
			} else if resp.Header.Get("Content-Encoding") == "" {
				o.sse = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
				resp.Body = &observedBody{ReadCloser: resp.Body, feed: o.response, finish: o.responseDone, readFailed: o.readFailed}
			}
			return nil
		},
	}
	p.ServeHTTP(w, r)
}

func (h *Handler) observeCooldown(id, path string, headers http.Header) {
	now := time.Now()
	until := now.Add(time.Minute)
	if n, err := strconv.ParseFloat(headers.Get("Retry-After"), 64); err == nil && n >= 0 {
		until = now.Add(time.Duration(n * float64(time.Second)))
	} else if t, err := http.ParseTime(headers.Get("Retry-After")); err == nil {
		until = t
	}
	h.Scheduler.Cooldown(id, path, until)
}

func identityHeader(name string) bool {
	n := strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	switch n {
	case "authorization", "cookie", "cookie2", "x-api-key", "api-key",
		"account", "account-id", "chatgpt-account-id", "openai-account-id", "x-account-id",
		"workspace", "workspace-id", "chatgpt-workspace-id", "openai-workspace-id", "x-workspace-id",
		"organization", "organization-id", "openai-organization", "openai-organization-id", "x-organization-id",
		"project", "project-id", "openai-project", "openai-project-id", "x-project-id",
		"actor", "actor-id", "openai-actor", "openai-actor-id", "x-openai-actor", "x-openai-actor-id",
		"forwarded", "x-real-ip":
		return true
	}
	return strings.HasPrefix(n, "x-pool-") || strings.HasPrefix(n, "x-forwarded-")
}
func stripIdentity(headers http.Header) {
	for k := range headers {
		if identityHeader(k) {
			delete(headers, k)
		}
	}
}

type observedBody struct {
	io.ReadCloser
	feed       func([]byte)
	finish     func()
	onEOF      func()
	readFailed func()
	once       sync.Once
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.feed(p[:n])
	if err != nil && err != io.EOF && b.readFailed != nil {
		b.readFailed()
	}
	if err == io.EOF {
		if b.onEOF != nil {
			b.onEOF()
		}
		b.once.Do(b.finish)
	}
	return n, err
}
func (b *observedBody) Close() error { b.once.Do(b.finish); return b.ReadCloser.Close() }

type observedDuplex struct {
	io.ReadWriteCloser
	feed func([]byte)
	sent func([]byte)
}

func (b *observedDuplex) Read(p []byte) (int, error) {
	n, err := b.ReadWriteCloser.Read(p)
	b.feed(p[:n])
	return n, err
}
func (b *observedDuplex) Write(p []byte) (int, error) { b.sent(p); return b.ReadWriteCloser.Write(p) }
