package pool

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

const testKBItems = `[{"type":"compaction","encrypted_content":"KB-one","extra":7},{"type":"compaction_summary","encrypted_content":"KB-two"},{"role":"user","content":"KB charter"}]`

func registerTestKB(t *testing.T, h *Handler, id string) *kbSnapshot {
	t.Helper()
	kb, err := h.RemoteKB.bind(id, json.RawMessage(testKBItems))
	if err != nil {
		t.Fatal(err)
	}
	return kb
}

func TestRemoteKBRegistrationPersistsImmutableSnapshotAndInheritance(t *testing.T) {
	h, _ := relayFixture(t)
	w := call(h, "POST", "/_pool/kb/bind", `{"session_id":"root","items":`+testKBItems+`}`)
	if w.Code != 200 || gjson.Get(w.Body.String(), "item_count").Int() != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	hash := gjson.Get(w.Body.String(), "snapshot_id").String()
	if w := call(h, "POST", "/_pool/kb/bind", `{"session_id":"root","items":`+testKBItems+`}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call(h, "POST", "/_pool/kb/bind", `{"session_id":"root","items":[{}]}`); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := call(h, "POST", "/_pool/kb/bind", `{"session_id":"root","items":[]}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/_pool/kb/bind", strings.NewReader(`{"session_id":"other","items":[{}]}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unauthorized registration", w.Code)
	}
	// Restart, inherit, restart again: neither descendants nor a resume need the
	// parent process or original Git checkout to remain available.
	h.RemoteKB = newRemoteKBStore(h.Config.StateDir)
	alias, err := h.RemoteKB.resolve(kbIdentity{session: "root", thread: "root-window"})
	if err != nil || alias.ID != hash {
		t.Fatal(alias, err)
	}
	h.RemoteKB = newRemoteKBStore(h.Config.StateDir)
	child, err := h.RemoteKB.resolve(kbIdentity{session: "child", thread: "child-window", parent: "root-window"})
	if err != nil || child.ID != hash {
		t.Fatal(child, err)
	}
	h.RemoteKB = newRemoteKBStore(h.Config.StateDir)
	grandchild, err := h.RemoteKB.resolve(kbIdentity{thread: "grandchild", parent: "child-window"})
	if err != nil || grandchild.ID != hash {
		t.Fatal(grandchild, err)
	}
	if w := call(h, "GET", "/_pool/kb/bind?session_id=grandchild", ""); w.Code != 200 || strings.Contains(w.Body.String(), "KB-one") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call(h, "GET", "/_pool/kb/bind?session_id=missing", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	entries, _ := os.ReadDir(filepath.Join(h.Config.StateDir, "remote-kb", "snapshots"))
	if len(entries) != 1 {
		t.Fatal("duplicated snapshots", len(entries))
	}
	_ = filepath.WalkDir(filepath.Join(h.Config.StateDir, "remote-kb"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		info, _ := entry.Info()
		if !entry.IsDir() && info.Mode().Perm() != 0600 {
			t.Fatal(path, info.Mode())
		}
		return nil
	})
}

func TestRemoteKBConcurrentSameBindingAndExplicitChild(t *testing.T) {
	h, _ := relayFixture(t)
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() { registerTestKB(t, h, "root") })
	}
	group.Wait()
	other, err := h.RemoteKB.bind("child", json.RawMessage(`[{"role":"user","content":"different"}]`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.RemoteKB.resolve(kbIdentity{session: "child", parent: "root"})
	if err != nil || got.ID != other.ID {
		t.Fatal(got, err)
	}
}

func TestRemoteKBPrefixEditsOnlyInsertionAndKeepsEveryBlob(t *testing.T) {
	h, _ := relayFixture(t)
	kb := registerTestKB(t, h, "root")
	for _, input := range []string{`[]`, `[ ]`, `[{"role":"developer","content":"rules"}]`, `[{"role":"system"},{"role":"developer"},{"type":"compaction","encrypted_content":"conversation"},{"role":"user"}]`} {
		body := []byte("{\n\"unknown\":1e+03, \"input\":" + input + ", \"tools\":[{\"future\":true}], \"instructions\":\"base\" }")
		out := kbInject(body, kb)
		if !json.Valid(out) {
			t.Fatalf("invalid insertion: %s", out)
		}
		if len(gjson.GetBytes(out, "input").Array()) != len(gjson.Parse(input).Array())+3 {
			t.Fatal(string(out))
		}
		if !bytes.Contains(out, []byte(`"unknown":1e+03`)) || !bytes.Contains(out, []byte(`"tools":[{"future":true}]`)) {
			t.Fatal("unknown bytes changed")
		}
		if bytes.Index(out, []byte("KB-one")) > bytes.Index(out, []byte("KB-two")) {
			t.Fatal("blob order changed")
		}
		if strings.Contains(input, "conversation") && bytes.Index(out, []byte("KB-two")) > bytes.Index(out, []byte("conversation")) {
			t.Fatal("KB after conversation compaction")
		}
		if strings.Contains(input, "rules") && bytes.Index(out, []byte("rules")) > bytes.Index(out, []byte("KB-one")) {
			t.Fatal("KB before developer instructions")
		}
		if !bytes.Equal(kbInject(body, kb), out) {
			t.Fatal("unstable prefix")
		}
	}
}

func remoteHTTPCall(h *Handler, path, id string, body []byte, encoding string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, bytes.NewReader(body))
	r.Header = headers.Clone()
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Set("Authorization", "Bearer client-secret")
	if id != "" {
		r.Header.Set("Session-Id", id)
	}
	if encoding != "" {
		r.Header.Set("Content-Encoding", encoding)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRemoteKBHTTPPassthroughCompactionAndUnknownRoutes(t *testing.T) {
	h, accounts := relayFixture(t, 90)
	registerTestKB(t, h, "root")
	var got []byte
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Chatgpt-Account-Id") != accounts[0].AccountID {
			t.Error("routing changed")
		}
		got, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"unfamiliar":1e+00}`))}, nil
	})
	for _, test := range []struct {
		path, id, body, metadata string
		inject                   bool
	}{
		{"/backend-api/codex/responses", "root", `{"input":[{"role":"user","content":"hi"}],"new_field":1e+00}`, "", true},
		{"/backend-api/codex/responses", "other", `opaque:not-json`, "", false},
		{"/backend-api/codex/responses", "root", `{"input":{"future":"shape"}}`, "", false},
		{"/backend-api/codex/images/generations", "root", `{"input":[]}`, "", false},
		{"/future/responses", "root", `{"input":[]}`, "", false},
		{"/backend-api/codex/responses/compact", "root", `{"input":[{"role":"user"}]}`, "", false},
		{"/_pool/rr/responses/compact", "root", `{"input":[]}`, "", false},
		{"/backend-api/codex/responses", "root", `{"input":[{"type":"compaction_trigger"}],"future":true}`, "", false},
		{"/backend-api/codex/responses", "root", `{"input":[{"role":"user"}]}`, `{"request_kind":"compaction"}`, false},
		{"/backend-api/codex/responses", "root", `{"input":[],"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\"}"}}`, "", false},
	} {
		headers := http.Header{}
		headers.Set("X-Codex-Turn-Metadata", test.metadata)
		w := remoteHTTPCall(h, test.path, test.id, []byte(test.body), "", headers)
		if w.Code != 200 || w.Body.String() != `{"unfamiliar":1e+00}` {
			t.Fatal(w.Code, w.Body.String())
		}
		if test.inject {
			if !bytes.Contains(got, []byte("KB-one")) {
				t.Fatal("KB missing")
			}
		} else if string(got) != test.body {
			t.Fatalf("passthrough changed: %s", got)
		}
	}
}

func TestRemoteKBHTTPCompressionAndChildMetadata(t *testing.T) {
	h, _ := relayFixture(t, 90)
	registerTestKB(t, h, "root")
	for _, encoding := range []string{"gzip", "zstd"} {
		body := []byte(`{"input":[{"role":"user","content":"` + strings.Repeat("large", 300000) + `"}],"unknown":1e+03}`)
		var encoded []byte
		if encoding == "gzip" {
			var b bytes.Buffer
			z := gzip.NewWriter(&b)
			_, _ = z.Write(body)
			_ = z.Close()
			encoded = b.Bytes()
		} else {
			z, _ := zstd.NewWriter(nil)
			encoded = z.EncodeAll(body, nil)
			z.Close()
		}
		h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Encoding") != "" || !bytes.Contains(got, []byte("KB-one")) || !bytes.Contains(got, []byte(`"unknown":1e+03`)) {
				t.Error("compressed injection failed")
			}
			if r.ContentLength != int64(len(got)) {
				t.Error("stale length")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})
		headers := http.Header{"X-Codex-Turn-Metadata": {`{"session_id":"child","thread_id":"child-window","parent_thread_id":"root"}`}}
		w := remoteHTTPCall(h, "/backend-api/codex/responses", "", encoded, encoding, headers)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		// Compaction retains even the original compressed bytes.
		h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got, _ := io.ReadAll(r.Body)
			if !bytes.Equal(got, encoded) || r.Header.Get("Content-Encoding") != encoding {
				t.Error("compressed compaction changed")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})
		w = remoteHTTPCall(h, "/backend-api/codex/responses/compact", "root", encoded, encoding, nil)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
}

func TestRemoteKBWebSocketDeltaCompactionAndReinjection(t *testing.T) {
	h, _ := relayFixture(t)
	registerTestKB(t, h, "root")
	s := &kbConversation{store: h.RemoteKB, headers: http.Header{"Session-Id": {"root"}}, path: "/backend-api/codex/responses"}
	full := `{"type":"response.create","generate":false,"input":[{"role":"developer","content":"rules"},{"role":"user","content":"question"}],"unknown":1e+00}`
	out, err := s.transform([]byte(full))
	if err != nil || !bytes.Contains(out, []byte("KB-one")) {
		t.Fatal(string(out), err)
	}
	s.event([]byte(`{"type":"response.completed","response":{"id":"prewarm","output":[]}}`))
	delta := `{"type":"response.create","previous_response_id":"prewarm","input":[{"role":"user","content":"second"}]}`
	out, err = s.transform([]byte(delta))
	if err != nil || string(out) != delta {
		t.Fatal("normal delta changed", string(out), err)
	}
	s.event([]byte(`{"type":"response.output_item.done","item":{"role":"assistant","content":"answer"}}`))
	s.event([]byte(`{"type":"response.completed","response":{"id":"inference","output":[]}}`))
	compact := `{"type":"response.create","previous_response_id":"inference","input":[{"type":"compaction_trigger"}],"unknown":1e+00}`
	out, err = s.transform([]byte(compact))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("KB-one")) || gjson.GetBytes(out, "previous_response_id").String() != "" || len(gjson.GetBytes(out, "input").Array()) != 5 || !bytes.Contains(out, []byte(`"unknown":1e+00`)) {
		t.Fatal("compact retained KB or lost history", string(out))
	}
	s.event([]byte(`{"type":"response.completed","response":{"id":"compact","output":[{"type":"compaction_summary","encrypted_content":"conversation-blob"}]}}`))
	// Switching back to inference on a reused connection must restore the KB.
	out, err = s.transform([]byte(`{"type":"response.create","previous_response_id":"compact","input":[{"role":"user","content":"next"}]}`))
	if err != nil || !bytes.Contains(out, []byte("KB-one")) || bytes.Contains(out, []byte("compaction_trigger")) || gjson.GetBytes(out, "previous_response_id").String() != "" {
		t.Fatal(string(out), err)
	}
	// Native post-compact requests send a new full context.
	out, err = s.transform([]byte(`{"type":"response.create","input":[{"role":"developer"},{"type":"compaction_summary","encrypted_content":"new-conversation"},{"role":"user"}]}`))
	if err != nil || bytes.Index(out, []byte("KB-two")) > bytes.Index(out, []byte("new-conversation")) {
		t.Fatal(string(out), err)
	}
	if _, err = s.transform([]byte(`{"type":"response.create","previous_response_id":"unseen","input":[]}`)); err == nil {
		t.Fatal("unknown delta silently lost prefix")
	}
}

func TestRemoteKBFramesFragmentationControlAndUnknownMessages(t *testing.T) {
	h, _ := relayFixture(t)
	registerTestKB(t, h, "root")
	for _, chunk := range []int{1, 7, 100000} {
		out := &memoryDuplex{}
		s := &kbConversation{store: h.RemoteKB, headers: http.Header{"Session-Id": {"root"}}}
		w := newKBWebsocket(out, s)
		payload := []byte(`{"type":"response.create","input":[],"unknown":1e+03}`)
		wire := wsFrame(false, 1, payload[:20], true)
		ping := wsFrame(true, 9, []byte("ping"), true)
		wire = append(wire, ping...)
		wire = append(wire, wsFrame(true, 0, payload[20:], true)...)
		for i := 0; i < len(wire); i += chunk {
			if _, err := w.Write(wire[i:min(i+chunk, len(wire))]); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.HasPrefix(out.Bytes(), ping) {
			t.Fatal("control frame lost")
		}
		var got []byte
		f := kbFrameStream{emit: func(opcode byte, _, b []byte) error {
			if opcode == 1 {
				got = append([]byte(nil), b...)
			}
			return nil
		}}
		if err := f.feed(out.Bytes()); err != nil || !bytes.Contains(got, []byte("KB-one")) {
			t.Fatal(string(got), err)
		}
		for _, frame := range [][]byte{wsFrame(true, 1, []byte(`{"type":"future","data":1e+00}`), true), wsFrame(true, 2, []byte("binary"), true), wsFrame(true, 1, nil, true), wsFrame(true, 8, []byte{3, 232}, true)} {
			out.Reset()
			if _, err := w.Write(frame); err != nil || !bytes.Equal(out.Bytes(), frame) {
				t.Fatal("opaque frame changed", err)
			}
		}
	}
}

func TestRemoteKBWebSocketIntegrationKeepsResponsesAndNegotiatesPlainFrames(t *testing.T) {
	h, _ := relayFixture(t, 90)
	registerTestKB(t, h, "root")
	attachUpstream(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-WebSocket-Extensions") != "" {
			t.Error("compression negotiated for editor")
		}
		up := websocket.Upgrader{EnableCompression: true, CheckOrigin: func(*http.Request) bool { return true }}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for i := 0; i < 3; i++ {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			if (i == 0) != bytes.Contains(b, []byte("KB-one")) {
				t.Errorf("wrong injection on message %d", i)
			}
			if i == 2 && (gjson.GetBytes(b, "previous_response_id").String() != "" || !bytes.Contains(b, []byte("first"))) {
				t.Error("compact delta still references KB history")
			}
			event := fmt.Sprintf(`{"type":"response.completed","response":{"id":"r%d","output":[{"role":"assistant","content":"answer"}]},"future":1e+03}`, i)
			if c.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
				return
			}
		}
	}))
	server := httptest.NewServer(h)
	defer server.Close()
	dialer := websocket.Dialer{EnableCompression: true}
	c, resp, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/backend-api/codex/responses", http.Header{"Authorization": {"Bearer client-secret"}, "Session-Id": {"root"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		t.Fatal("compressed frames enabled")
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i, body := range []string{`{"type":"response.create","input":[{"role":"user","content":"first"}]}`, `{"type":"response.create","previous_response_id":"r0","input":[{"role":"user","content":"second"}]}`, `{"type":"response.create","previous_response_id":"r1","input":[{"type":"compaction_trigger"}]}`} {
		if err = c.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
			t.Fatal(err)
		}
		_, got, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf(`{"type":"response.completed","response":{"id":"r%d","output":[{"role":"assistant","content":"answer"}]},"future":1e+03}`, i)
		if string(got) != want {
			t.Fatal("response changed", string(got))
		}
	}
}
