package pool

import (
	"encoding/binary"
	"io"
)

func frameHeaderSize(header []byte) int {
	need := 2
	switch header[1] & 127 {
	case 126:
		need += 2
	case 127:
		need += 8
	}
	if header[1]&128 != 0 {
		need += 4
	}
	return need
}

func framePayloadSize(header []byte) uint64 {
	switch header[1] & 127 {
	case 126:
		return uint64(binary.BigEndian.Uint16(header[2:4]))
	case 127:
		return binary.BigEndian.Uint64(header[2:10])
	default:
		return uint64(header[1] & 127)
	}
}

// gatedWebsocket rechecks routing before a new text/binary message. It buffers
// at most the 14-byte frame header, then streams the original bytes. A rejected
// message never reaches the upstream; ReverseProxy closes both connections.
// Continuations and control frames stay on their existing account unchanged.
type gatedWebsocket struct {
	io.ReadWriteCloser
	check     func() error
	drain     *websocketDrain
	header    []byte
	remaining uint64
}

func (g *gatedWebsocket) Read(p []byte) (int, error) {
	if g.drain != nil {
		g.drain.delivered()
	}
	return g.ReadWriteCloser.Read(p)
}

func (g *gatedWebsocket) Close() error {
	if g.drain != nil {
		g.drain.close()
	}
	return g.ReadWriteCloser.Close()
}

func (g *gatedWebsocket) Write(p []byte) (int, error) {
	accepted := 0
	for len(p) > 0 {
		if g.remaining == 0 {
			g.header = append(g.header, p[0])
			p = p[1:]
			accepted++
			if len(g.header) < 2 || len(g.header) < frameHeaderSize(g.header) {
				continue
			}
			if opcode := g.header[0] & 15; opcode == 1 || opcode == 2 {
				if err := g.check(); err != nil {
					if g.drain != nil {
						g.drain.wait()
					}
					return accepted, err
				}
			}
			n, err := g.ReadWriteCloser.Write(g.header)
			if err != nil {
				return accepted, err
			}
			if n != len(g.header) {
				return accepted, io.ErrShortWrite
			}
			g.remaining = framePayloadSize(g.header)
			g.header = g.header[:0]
		}
		n := len(p)
		if uint64(n) > g.remaining {
			n = int(g.remaining)
		}
		if n == 0 {
			continue
		}
		written, err := g.ReadWriteCloser.Write(p[:n])
		accepted += written
		g.remaining -= uint64(written)
		p = p[written:]
		if err != nil {
			return accepted, err
		}
		if written != n {
			return accepted, io.ErrShortWrite
		}
	}
	return accepted, nil
}
