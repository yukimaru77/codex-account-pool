package pool

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/tidwall/gjson"
)

// Keep the client-visible history (without injected KB) for the latest response
// on this socket. A compact delta must not reference a server history with KB.
// Normal deltas retain previous_response_id and reuse the upstream KB prefix.
type kbConversation struct {
	mu                   sync.Mutex
	store                *remoteKBStore
	headers              http.Header
	path                 string
	lastID, lastKB       string
	lastInput            []json.RawMessage
	pendingInput, output []json.RawMessage
	pendingKB            string
}

func (s *kbConversation) transform(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) || gjson.GetBytes(body, "type").String() != "response.create" {
		return body, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := kbRequestIdentity(s.headers, body)
	kb, err := s.store.resolve(m)
	if err != nil {
		return nil, err
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	var native []json.RawMessage
	if err = json.Unmarshal([]byte(input.Raw), &native); err != nil {
		return body, nil
	}
	wanted := ""
	if kb != nil && !kbIsCompact(s.path, m, body) {
		wanted = kb.ID
	}
	previous := gjson.GetBytes(body, "previous_response_id")
	expand := false
	if previous.String() != "" {
		if previous.String() != s.lastID {
			return nil, errors.New("remote KB cannot resolve previous_response_id on this connection")
		}
		native = append(append([]json.RawMessage(nil), s.lastInput...), native...)
		expand = s.lastKB != wanted
		if expand {
			// null has the same absence semantics as omitting this optional ID.
			body = kbReplace(body, previous, []byte("null"))
			full, _ := json.Marshal(native)
			body = kbReplace(body, gjson.GetBytes(body, "input"), full)
		}
	}
	s.pendingInput, s.pendingKB, s.output = native, wanted, nil
	if wanted != "" && (previous.String() == "" || expand) {
		body = kbInject(body, kb)
	}
	return body, nil
}

func (s *kbConversation) event(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	event := gjson.ParseBytes(body)
	switch event.Get("type").String() {
	case "response.output_item.done":
		if item := event.Get("item"); item.IsObject() {
			s.output = append(s.output, json.RawMessage(item.Raw))
		}
	case "response.completed", "response.incomplete":
		if output := event.Get("response.output"); len(s.output) == 0 && output.IsArray() {
			_ = json.Unmarshal([]byte(output.Raw), &s.output)
		}
		s.lastID = event.Get("response.id").String()
		s.lastKB = s.pendingKB
		s.lastInput = append(s.pendingInput, s.output...)
		s.pendingInput, s.output = nil, nil
	case "response.failed", "error":
		s.pendingInput, s.output = nil, nil
	}
}

// Frame assembly is used only after opting into Remote KB. Compression is not
// negotiated on these sockets, so the relay can edit messages without changing
// the ordinary relay or implementing a second WebSocket compression stack.
type kbFrameStream struct {
	header, payload, wire, message []byte
	remaining                      uint64
	active                         bool
	opcode                         byte
	emit                           func(byte, []byte, []byte) error
}

func (f *kbFrameStream) feed(b []byte) error {
	for len(b) > 0 || (f.active && f.remaining == 0) {
		if !f.active {
			f.header = append(f.header, b[0])
			b = b[1:]
			if len(f.header) < 2 || len(f.header) < frameHeaderSize(f.header) {
				continue
			}
			if f.header[0]&112 != 0 {
				return errors.New("unexpected WebSocket extension on remote KB connection")
			}
			f.remaining = framePayloadSize(f.header)
			f.active = true
		}
		n := min(uint64(len(b)), f.remaining)
		f.payload = append(f.payload, b[:int(n)]...)
		b = b[int(n):]
		f.remaining -= n
		if f.remaining > 0 {
			continue
		}
		wire := append(append([]byte(nil), f.header...), f.payload...)
		if f.header[1]&128 != 0 {
			mask := f.header[len(f.header)-4:]
			for i := range f.payload {
				f.payload[i] ^= mask[i%4]
			}
		}
		opcode, final := f.header[0]&15, f.header[0]&128 != 0
		if opcode >= 8 {
			if err := f.emit(opcode, wire, f.payload); err != nil {
				return err
			}
		} else {
			if opcode != 0 {
				f.opcode = opcode
			}
			f.wire = append(f.wire, wire...)
			f.message = append(f.message, f.payload...)
			if final {
				if err := f.emit(f.opcode, f.wire, f.message); err != nil {
					return err
				}
				f.wire, f.message = nil, nil
			}
		}
		f.header, f.payload, f.active = nil, nil, false
	}
	return nil
}

func kbMaskedFrame(payload []byte) ([]byte, error) {
	header := []byte{129}
	switch {
	case len(payload) < 126:
		header = append(header, 128|byte(len(payload)))
	case len(payload) <= 65535:
		header = binary.BigEndian.AppendUint16(append(header, 128|126), uint16(len(payload)))
	default:
		header = binary.BigEndian.AppendUint64(append(header, 128|127), uint64(len(payload)))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return nil, err
	}
	header = append(header, mask[:]...)
	for i, c := range payload {
		header = append(header, c^mask[i%4])
	}
	return header, nil
}

type kbWebsocket struct {
	io.ReadWriteCloser
	requests, responses kbFrameStream
}

func newKBWebsocket(rwc io.ReadWriteCloser, conversation *kbConversation) *kbWebsocket {
	w := &kbWebsocket{ReadWriteCloser: rwc}
	w.requests.emit = func(opcode byte, wire, payload []byte) error {
		if opcode == 1 {
			out, err := conversation.transform(payload)
			if err != nil {
				return err
			}
			if !bytes.Equal(out, payload) {
				wire, err = kbMaskedFrame(out)
				if err != nil {
					return err
				}
			}
		}
		n, err := rwc.Write(wire)
		if err == nil && n != len(wire) {
			err = io.ErrShortWrite
		}
		return err
	}
	w.responses.emit = func(opcode byte, _, payload []byte) error {
		if opcode == 1 {
			conversation.event(payload)
		}
		return nil
	}
	return w
}

func (w *kbWebsocket) Write(b []byte) (int, error) { err := w.requests.feed(b); return len(b), err }
func (w *kbWebsocket) Read(b []byte) (int, error) {
	n, err := w.ReadWriteCloser.Read(b)
	if parseErr := w.responses.feed(b[:n]); parseErr != nil {
		return n, parseErr
	}
	return n, err
}
