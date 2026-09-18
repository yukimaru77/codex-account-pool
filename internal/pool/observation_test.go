package pool

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestObservationSSEUnknownMultilineAndBoundedCapture(t *testing.T) {
	var records []Record
	o := newObservation(context.Background(), Record{AuthID: "a", RequestedAt: time.Now()}, func(_ context.Context, r Record) { records = append(records, r) })
	o.sse = true
	stream := "event: future\ndata: {\"unknown\":1}\n\n" +
		"data: " + strings.Repeat("x", observationLimit+1) + "\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\n" +
		"data: \"response\":{\"id\":\"r1\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}}\n\n"
	for i := 0; i < len(stream); i += 117 {
		end := i + 117
		if end > len(stream) {
			end = len(stream)
		}
		o.response([]byte(stream[i:end]))
	}
	o.finish()
	if len(records) != 1 || records[0].Detail.TotalTokens != 7 || records[0].AuthID != "a" || records[0].Model != "m" {
		t.Fatalf("records=%+v", records)
	}
	if len(o.line) > observationLimit || len(o.eventData) > observationLimit {
		t.Fatal("unbounded observation")
	}
}

func wsFrame(fin bool, opcode byte, payload []byte, masked bool) []byte {
	first := opcode
	if fin {
		first |= 128
	}
	header := []byte{first, 0}
	switch {
	case len(payload) < 126:
		header[1] = byte(len(payload))
	case len(payload) <= 65535:
		header[1] = 126
		header = binary.BigEndian.AppendUint16(header, uint16(len(payload)))
	default:
		header[1] = 127
		header = binary.BigEndian.AppendUint64(header, uint64(len(payload)))
	}
	b := append([]byte(nil), payload...)
	if masked {
		header[1] |= 128
		mask := []byte{1, 7, 9, 3}
		header = append(header, mask...)
		for i := range b {
			b[i] ^= mask[i%4]
		}
	}
	return append(header, b...)
}

func TestWebSocketObservationFragmentationMaskingAndControl(t *testing.T) {
	for _, masked := range []bool{false, true} {
		for _, size := range []int{0, 20, 126, 65536} {
			payload := bytes.Repeat([]byte("x"), size)
			split := size / 2
			wire := wsFrame(false, 1, payload[:split], masked)
			wire = append(wire, wsFrame(true, 9, []byte("ping"), masked)...)
			wire = append(wire, wsFrame(true, 0, payload[split:], masked)...)
			var got [][]byte
			f := newFrames(func(b []byte) { got = append(got, append([]byte{}, b...)) })
			for i := 0; i < len(wire); i += 31 {
				end := i + 31
				if end > len(wire) {
					end = len(wire)
				}
				f.feed(wire[i:end])
			}
			if len(got) != 1 || !bytes.Equal(got[0], payload) {
				t.Fatalf("masked=%v size=%d messages=%d", masked, size, len(got))
			}
		}
	}
}

func TestWebSocketObservationSkipsLargeCompressedAndResumes(t *testing.T) {
	var got [][]byte
	f := newFrames(func(b []byte) { got = append(got, append([]byte{}, b...)) })
	f.feed(wsFrame(true, 1, bytes.Repeat([]byte("a"), observationLimit+1), false))
	compressed := wsFrame(true, 1, []byte("compressed"), false)
	compressed[0] |= 64
	f.feed(compressed)
	f.feed(wsFrame(true, 1, []byte("visible"), false))
	if len(got) != 1 || string(got[0]) != "visible" {
		t.Fatalf("messages=%q", got)
	}
}

func TestObservationDeduplicatesAndRecordsFailureWithoutErrorBody(t *testing.T) {
	var records []Record
	o := newObservation(context.Background(), Record{}, func(_ context.Context, r Record) { records = append(records, r) })
	event := []byte(`{"type":"response.failed","response":{"id":"r","error":{"message":"secret"}}}`)
	o.event(event)
	o.event(event)
	o.finish()
	if len(records) != 1 || !records[0].Failed || records[0].Fail.Body != "" {
		t.Fatalf("records=%+v", records)
	}
}

func TestWebSocketRequestObservationDoesNotChangeBytes(t *testing.T) {
	o := newObservation(context.Background(), Record{}, func(context.Context, Record) {})
	wire := wsFrame(true, 1, []byte(`{"type":"response.create","model":"m","unknown":true}`), true)
	before := bytes.Clone(wire)
	f := newFrames(o.websocketRequest)
	f.feed(wire)
	if !bytes.Equal(wire, before) || o.record.Model != "m" || !o.record.Stream || (o.record.Generate == nil || !*o.record.Generate) {
		t.Fatal("request observation changed bytes or lost metadata")
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenBody) Close() error             { return nil }

func TestObservationReportsTruncatedStreamWithoutChangingReadError(t *testing.T) {
	var records []Record
	o := newObservation(context.Background(), Record{}, func(_ context.Context, r Record) { records = append(records, r) })
	body := &observedBody{ReadCloser: brokenBody{}, feed: o.response, finish: o.responseDone, readFailed: o.readFailed}
	_, err := io.ReadAll(body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("stream error was hidden")
	}
	_ = body.Close()
	o.finish()
	if len(records) != 1 || !records[0].Failed {
		t.Fatalf("records=%+v", records)
	}
}

func TestCleanEOFDoesNotMeanResponseCompleted(t *testing.T) {
	for _, terminal := range []string{"", "response.completed", "response.incomplete", "response.failed"} {
		t.Run(terminal, func(t *testing.T) {
			var records []Record
			o := newObservation(context.Background(), Record{}, func(_ context.Context, r Record) { records = append(records, r) })
			o.sse = true
			o.response([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
			if terminal != "" {
				o.response([]byte("data: {\"type\":\"" + terminal + "\",\"response\":{\"id\":\"r\"}}\n\n"))
			}
			o.responseDone()
			o.finish()
			if len(records) != 1 {
				t.Fatalf("records=%+v", records)
			}
			r := records[0]
			if r.Failed != (terminal != "response.completed") || r.TerminalEvent != terminal || r.UsageObserved {
				t.Fatalf("incorrect terminal accounting: %+v", r)
			}
		})
	}
}

func TestObservedZeroUsageIsDistinctFromMissingUsage(t *testing.T) {
	var records []Record
	o := newObservation(context.Background(), Record{}, func(_ context.Context, r Record) { records = append(records, r) })
	o.event([]byte(`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`))
	o.finish()
	if len(records) != 1 || !records[0].UsageObserved || records[0].Detail.TotalTokens != 0 || records[0].Failed {
		t.Fatalf("records=%+v", records)
	}
}

func TestLargeTerminalRecordsCompletionWithoutInventingUsage(t *testing.T) {
	var records []Record
	o := newObservation(context.Background(), Record{}, func(_ context.Context, r Record) { records = append(records, r) })
	o.largeLifecycle(lifecycleEvent{typ: "response.create"})
	o.largeLifecycle(lifecycleEvent{typ: "response.created", id: "r"})
	o.largeLifecycle(lifecycleEvent{typ: "response.completed", id: "r"})
	o.finish()
	if len(records) != 1 || records[0].Failed || records[0].TerminalEvent != "response.completed" || records[0].UsageObserved {
		t.Fatalf("records=%+v", records)
	}
}

func TestSkippedSSEEventDoesNotProveAnIncompleteResponse(t *testing.T) {
	var records []Record
	o := newObservation(context.Background(), Record{}, func(_ context.Context, r Record) { records = append(records, r) })
	o.sse = true
	o.response([]byte("data: {\"type\":\"response.created\"}\n\n"))
	o.response([]byte("data: {\"type\":\"response.completed\",\"large\":\"" + strings.Repeat("a", observationLimit) + "\"}\n\n"))
	o.finish()
	if len(records) != 1 || records[0].Failed || records[0].TerminalEvent != "" || records[0].UsageObserved {
		t.Fatalf("unknown completion/usage was treated as proven failure: %+v", records)
	}
}
