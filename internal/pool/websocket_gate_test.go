package pool

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

type memoryDuplex struct{ bytes.Buffer }

func (*memoryDuplex) Close() error { return nil }

func TestWebSocketGatePreservesFragmentedMaskedCompressedAndBinaryBytes(t *testing.T) {
	for _, size := range []int{0, 126, 65536} {
		for _, chunk := range []int{1, 13, 1001} {
			t.Run(fmt.Sprintf("size=%d/chunk=%d", size, chunk), func(t *testing.T) {
				payload := bytes.Repeat([]byte("opaque"), size)
				wire := wsFrame(false, 1, payload, true)
				wire = append(wire, wsFrame(true, 9, []byte("ping"), true)...)
				wire = append(wire, wsFrame(true, 0, []byte("continuation"), true)...)
				compressed := wsFrame(true, 2, []byte("opaque compressed bytes"), false)
				compressed[0] |= 64
				wire = append(wire, compressed...)
				wire = append(wire, wsFrame(true, 8, []byte{3, 232}, true)...)
				calls := 0
				out := &memoryDuplex{}
				gate := &gatedWebsocket{ReadWriteCloser: out, check: func() error { calls++; return nil }}
				for i := 0; i < len(wire); i += chunk {
					end := min(i+chunk, len(wire))
					if n, err := gate.Write(wire[i:end]); err != nil || n != end-i {
						t.Fatalf("write n=%d err=%v", n, err)
					}
					if len(gate.header) > 14 {
						t.Fatal("gate buffered beyond frame header")
					}
				}
				if !bytes.Equal(out.Bytes(), wire) || calls != 2 {
					t.Fatalf("frame data changed or gated control/continuation: checks=%d", calls)
				}
			})
		}
	}
}

func TestWebSocketGateFinishesCurrentMessageAndRejectsNextBeforeUpstream(t *testing.T) {
	out := &memoryDuplex{}
	available := true
	rejected := errors.New("quota reserve reached")
	gate := &gatedWebsocket{ReadWriteCloser: out, check: func() error {
		if !available {
			return rejected
		}
		return nil
	}}
	first := wsFrame(false, 1, []byte("first"), true)
	if _, err := gate.Write(first); err != nil {
		t.Fatal(err)
	}
	available = false
	continuation := wsFrame(true, 9, []byte("ping"), true)
	continuation = append(continuation, wsFrame(true, 0, []byte("end"), true)...)
	if _, err := gate.Write(continuation); err != nil {
		t.Fatal("interrupted an already admitted message", err)
	}
	if _, err := gate.Write(wsFrame(true, 1, []byte("must not reach upstream"), true)); !errors.Is(err, rejected) {
		t.Fatal("new message admitted after reserve reached")
	}
	if !bytes.Equal(out.Bytes(), append(first, continuation...)) {
		t.Fatal("rejected message leaked bytes or previous message was changed")
	}
}
