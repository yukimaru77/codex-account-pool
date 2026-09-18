package pool

import (
	"bytes"
	"strings"
	"testing"
)

func TestWebsocketLifecycleLargeFragmentedMessagesAndFieldOrder(t *testing.T) {
	padding := strings.Repeat(`long {\"quoted\": [123]} string`, observationLimit/10)
	for _, payload := range []string{
		`{"type":"response.completed","response":{"id":"r","output":"` + padding + `"}}`,
		`{"response":{"output":"` + padding + `","id":"r"},"type":"response.completed"}`,
		`{"unknown":[{"type":"wrong","response":{"id":"wrong"}}],"response":{"output":"` + padding + `","id":"\u0072"},"ty\u0070e":"response.completed"}`,
		`{"response":{"output":"` + padding + `","id":"r"},"future":{"response":{"id":"wrong"},"labels":["a","b"]},"type":"response.completed"}`,
		`{"` + strings.Repeat("key", 500) + `":"type","unknown":"` + padding + `","type":"response.completed","response":{"id":"r"}}`,
	} {
		for _, masked := range []bool{false, true} {
			var events []lifecycleEvent
			f := newFrames(func([]byte) { t.Error("large payload should bypass usage capture") })
			f.lifecycle = func(event lifecycleEvent) { events = append(events, event) }
			split := len(payload) / 2
			wire := wsFrame(false, 1, []byte(payload[:split]), masked)
			wire = append(wire, wsFrame(true, 9, []byte("ping"), masked)...)
			wire = append(wire, wsFrame(true, 0, []byte(payload[split:]), masked)...)
			before := bytes.Clone(wire)
			for pos := 0; pos < len(wire); pos += 113 {
				end := min(pos+113, len(wire))
				f.feed(wire[pos:end])
				if end < len(wire) && len(events) != 0 {
					t.Fatal("terminal emitted before complete message")
				}
				if len(f.metadata.str) > 1024 || len(f.message) > observationLimit {
					t.Fatal("unbounded observation")
				}
			}
			if len(events) != 1 || events[0] != (lifecycleEvent{typ: "response.completed", id: "r"}) {
				t.Fatalf("metadata=%+v", events)
			}
			if !bytes.Equal(wire, before) {
				t.Fatal("wire bytes changed")
			}
		}
	}
}

func TestWebsocketLifecycleResetsAndReadsErrorResponseID(t *testing.T) {
	f := newFrames(func([]byte) {})
	var events []lifecycleEvent
	f.lifecycle = func(event lifecycleEvent) { events = append(events, event) }
	for _, payload := range []string{`{"type":"response.created","response":{"id":"a"}}`, `{"type":"error","response_id":"b"}`, `{"type":"response.create"}`} {
		f.feed(wsFrame(true, 1, []byte(payload), false))
	}
	if len(events) != 3 || events[1] != (lifecycleEvent{typ: "error", id: "b"}) || events[2] != (lifecycleEvent{typ: "response.create"}) {
		t.Fatalf("metadata leaked across messages: %+v", events)
	}
}
