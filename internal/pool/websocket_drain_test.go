package pool

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebsocketReserveWaitsForTerminalDeliveryBeforeClosing(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.failed", "response.incomplete", "large-completed"} {
		t.Run(terminal, func(t *testing.T) {
			h, accounts := relayFixture(t, 90, 90)
			finish := make(chan struct{})
			defer close(finish)
			received := make(chan string, 4)
			created := `{"type":"response.created","response":{"id":"r"}}`
			limit := `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":90,"window_minutes":10080,"reset_after_seconds":3600}}}`
			delta := `{"type":"response.output_text.delta","delta":"still streaming"}`
			completed := `{"type":"` + terminal + `","response":{"id":"r"},"future":true}`
			if terminal == "large-completed" {
				completed = `{"response":{"output":"` + strings.Repeat("a", observationLimit+1) + `","id":"r"},"type":"response.completed"}`
			}
			finishTurn := make(chan struct{})
			attachUpstream(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Sec-WebSocket-Extensions") != "" {
					t.Error("Responses compression still negotiated")
				}
				up := websocket.Upgrader{EnableCompression: true, CheckOrigin: func(*http.Request) bool { return true }}
				c, err := up.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				_, b, err := c.ReadMessage()
				if err != nil {
					return
				}
				received <- r.Header.Get("Chatgpt-Account-Id") + ":" + string(b)
				if err := c.WriteMessage(websocket.TextMessage, []byte(created)); err != nil {
					return
				}
				if err := c.WriteMessage(websocket.TextMessage, []byte(limit)); err != nil {
					return
				}
				select {
				case <-finishTurn:
				case <-finish:
					return
				}
				if err := c.WriteMessage(websocket.TextMessage, []byte(delta)); err != nil {
					return
				}
				if err := c.WriteMessage(websocket.TextMessage, []byte(completed)); err != nil {
					return
				}
				_, b, err = c.ReadMessage()
				if err == nil {
					received <- string(b)
				}
			}))
			server := httptest.NewServer(h)
			defer server.Close()
			dialer := *websocket.DefaultDialer
			dialer.EnableCompression = true
			c, resp, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/backend-api/codex/responses", http.Header{"Authorization": {"Bearer client-secret"}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
				t.Fatal("unexpected compressed response")
			}
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			first := `{"type":"response.create","input":[],"unknown":true}`
			if terminal == "large-completed" {
				first = `{"input":"` + strings.Repeat("a", observationLimit+1) + `","type":"response.create"}`
			}
			if err := c.WriteMessage(websocket.TextMessage, []byte(first)); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{created, limit} {
				_, b, err := c.ReadMessage()
				if err != nil || string(b) != want {
					t.Fatalf("first frames: %q %v", b, err)
				}
			}
			// Submit the next turn before the first terminal. Rotation must
			// neither admit it to A nor cut off A's remaining response frames.
			if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","input":"next"}`)); err != nil {
				t.Fatal(err)
			}
			time.AfterFunc(30*time.Millisecond, func() { close(finishTurn) })
			for _, want := range []string{delta, completed} {
				_, b, err := c.ReadMessage()
				if err != nil || string(b) != want {
					t.Fatalf("response interrupted: got=%q err=%v want=%s", b, err, want)
				}
			}
			if _, _, err := c.ReadMessage(); err == nil {
				t.Fatal("rotation did not close old connection after terminal")
			}
			if got := <-received; got != accounts[0].AccountID+":"+first {
				t.Fatalf("first request: %s", got)
			}
			select {
			case got := <-received:
				t.Fatalf("next request forwarded/replayed: %s", got)
			default:
			}
		})
	}
}

func TestWebsocketDrainTracksParallelResponsesAndOnlyDeliveredTerminals(t *testing.T) {
	d := newWebsocketDrain()
	d.request(lifecycleEvent{typ: "response.create"})
	d.request(lifecycleEvent{typ: "response.create"})
	d.event(lifecycleEvent{typ: "response.created", id: "a"})
	d.event(lifecycleEvent{typ: "response.created", id: "b"})
	d.event(lifecycleEvent{typ: "response.completed", id: "b"})
	d.delivered()
	if d.awaiting != 0 || len(d.active) != 1 || !d.active["a"] {
		t.Fatal("wrong pending response retired")
	}
	d.event(lifecycleEvent{typ: "response.completed", id: "a"})
	if len(d.active) != 1 {
		t.Fatal("parsed terminal treated as delivered")
	}
	d.delivered()
	d.wait()
	if len(d.active) != 0 {
		t.Fatal("terminal was not retired")
	}
}

func TestWebsocketDrainCloseUnblocksWaitingSender(t *testing.T) {
	d := newWebsocketDrain()
	d.request(lifecycleEvent{typ: "response.create"})
	done := make(chan struct{})
	go func() { d.wait(); close(done) }()
	d.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sender leaked after connection close")
	}
}

func TestWebsocketDrainUnrelatedTerminalDoesNotRetireAwaitingRequest(t *testing.T) {
	d := newWebsocketDrain()
	d.request(lifecycleEvent{typ: "response.create"})
	d.event(lifecycleEvent{typ: "response.completed", id: "older-response"})
	d.delivered()
	if d.awaiting != 1 {
		t.Fatal("unrelated terminal retired pending request")
	}
	d.event(lifecycleEvent{typ: "error"})
	d.delivered()
	if d.awaiting != 0 {
		t.Fatal("pre-created rejection did not retire request")
	}
}
