package pool

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const observationLimit = 1 << 20

// Observation is bounded and best effort. A large or unknown event is still
// relayed byte-for-byte, even when its usage cannot be decoded.
type observation struct {
	onEvent func([]byte)

	mu                                        sync.Mutex
	ctx                                       context.Context
	sourceContext                             context.Context
	record                                    Record
	publish                                   func(context.Context, Record)
	req, res, line, eventData                 []byte
	reqLarge, resLarge, lineLarge, eventLarge bool
	sse                                       bool
	count                                     int
	pending                                   bool
	lastResponse                              string
	sawResponse, terminal                     bool
	unobservedEvent                           bool
}

func newObservation(ctx context.Context, r Record, publish func(context.Context, Record)) *observation {
	return &observation{ctx: context.WithoutCancel(ctx), sourceContext: ctx, record: r, publish: publish}
}

func capture(dst *[]byte, large *bool, b []byte) {
	if *large {
		return
	}
	if len(*dst)+len(b) > observationLimit {
		*dst = nil
		*large = true
		return
	}
	*dst = append(*dst, b...)
}

func (o *observation) request(b []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	capture(&o.req, &o.reqLarge, b)
}
func (o *observation) requestDone() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.reqLarge && gjson.ValidBytes(o.req) {
		o.record.Model = gjson.GetBytes(o.req, "model").String()
		o.record.ServiceTier = gjson.GetBytes(o.req, "service_tier").String()
		o.record.ReasoningEffort = gjson.GetBytes(o.req, "reasoning.effort").String()
		o.record.Stream = gjson.GetBytes(o.req, "stream").Bool()
	}
	o.req = nil
}
func (o *observation) status(code int, headers http.Header) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.record.Fail.StatusCode = code
	o.record.Failed = code >= 400
	o.record.ResponseHeaders = headers.Clone()
	stripIdentity(o.record.ResponseHeaders)
	o.record.ResponseHeaders.Del("Set-Cookie")
	o.record.ResponseHeaders.Del("Set-Cookie2")
}
func (o *observation) failed(code int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.record.Failed = true
	o.record.Fail.StatusCode = code
}

func (o *observation) readFailed() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.record.Failed = true
}

func (o *observation) response(b []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(b) > 0 && o.record.TTFT == 0 {
		o.record.TTFT = time.Since(o.record.RequestedAt)
	}
	if !o.sse {
		capture(&o.res, &o.resLarge, b)
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			capture(&o.line, &o.lineLarge, b)
			return
		}
		capture(&o.line, &o.lineLarge, b[:i])
		if o.lineLarge {
			o.eventLarge = true
		} else {
			o.sseLine(bytes.TrimSuffix(o.line, []byte{'\r'}))
		}
		o.line = nil
		o.lineLarge = false
		b = b[i+1:]
	}
}

func (o *observation) sseLine(line []byte) {
	if len(line) == 0 {
		if !o.eventLarge {
			o.decode(o.eventData)
		} else {
			o.unobservedEvent = true
		}
		o.eventData = nil
		o.eventLarge = false
	} else if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimPrefix(line[5:], []byte{' '})
		if len(o.eventData) > 0 {
			capture(&o.eventData, &o.eventLarge, []byte{'\n'})
		}
		capture(&o.eventData, &o.eventLarge, data)
	}
}

func (o *observation) responseDone() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.sse && !o.resLarge {
		o.decode(o.res)
	}
	o.res = nil
}

func (o *observation) event(b []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(b) > 0 && o.record.TTFT == 0 {
		o.record.TTFT = time.Since(o.record.RequestedAt)
	}
	o.decode(b)
}

func (o *observation) websocketRequest(b []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !gjson.ValidBytes(b) || gjson.GetBytes(b, "type").String() != "response.create" {
		return
	}
	o.beginWebsocket(gjson.ParseBytes(b))
}

func (o *observation) beginWebsocket(v gjson.Result) {
	o.record.RequestedAt = time.Now()
	o.pending = true
	o.sawResponse, o.terminal = true, false
	o.record.TTFT = 0
	o.record.Stream = true
	generate := true
	o.record.Generate = &generate
	o.record.Model = v.Get("model").String()
	o.record.ServiceTier = v.Get("service_tier").String()
	o.record.ReasoningEffort = v.Get("reasoning.effort").String()
}

// Large messages still provide lifecycle metadata, but not invented usage.
func (o *observation) largeLifecycle(event lifecycleEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if event.typ == "response.create" {
		o.beginWebsocket(gjson.Result{})
	} else {
		o.decodeResponse(event.typ, event.id, gjson.Result{})
	}
}

func (o *observation) decode(b []byte) {
	if o.onEvent != nil {
		o.onEvent(b)
	}

	if !gjson.ValidBytes(b) {
		return
	}
	v := gjson.ParseBytes(b)
	typ := v.Get("type").String()
	root := v
	if v.Get("response").IsObject() {
		root = v.Get("response")
	}
	o.decodeResponse(typ, root.Get("id").String(), root)
}

func (o *observation) decodeResponse(typ, id string, root gjson.Result) {
	if typ == "response.created" || typ == "response.in_progress" {
		o.sawResponse, o.terminal = true, false
	}
	terminal := typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete" || typ == "error"
	u := root.Get("usage")
	failed := typ == "error" || typ == "response.failed" || typ == "response.incomplete" || root.Get("status").String() == "failed" || root.Get("status").String() == "incomplete"
	if !u.Exists() && !failed && !terminal {
		return
	}
	// Only terminal response events are accounted. Unknown events are forwarded.
	if strings.HasPrefix(typ, "response.") && typ != "response.completed" && typ != "response.failed" && typ != "response.incomplete" {
		return
	}
	if id != "" && id == o.lastResponse {
		return
	}
	o.lastResponse = id
	o.terminal = terminal
	r := o.record
	if terminal {
		r.TerminalEvent = typ
	}
	r.UsageObserved = u.IsObject()
	if model := root.Get("model").String(); model != "" {
		r.Model = model
	}
	r.Failed = r.Failed || failed
	r.Latency = time.Since(r.RequestedAt)
	r.Detail = Detail{InputTokens: u.Get("input_tokens").Int(), OutputTokens: u.Get("output_tokens").Int(), TotalTokens: u.Get("total_tokens").Int(), CachedTokens: u.Get("input_tokens_details.cached_tokens").Int(), ReasoningTokens: u.Get("output_tokens_details.reasoning_tokens").Int()}
	r.ResponseServiceTier = root.Get("service_tier").String()
	o.publish(o.ctx, r)
	o.count++
	o.pending = false
}

func (o *observation) finish() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.count == 0 || o.pending {
		r := o.record
		r.Failed = r.Failed || o.sourceContext.Err() != nil || (o.sawResponse && !o.terminal && !o.unobservedEvent)
		r.Latency = time.Since(r.RequestedAt)
		// No invented usage when observation is incomplete or this is a lookup.
		o.publish(o.ctx, r)
	}
}

// frames observes WebSocket messages without participating in
// the protocol. Compressed messages are skipped; raw bytes are never changed.
type frames struct {
	header            []byte
	remaining         uint64
	opcode            byte
	fin, skip, active bool
	mask              [4]byte
	masked            bool
	position          uint64
	message           []byte
	large             bool
	emit              func([]byte)
	lifecycle         func(lifecycleEvent)
	metadata          websocketLifecycle
}

func newFrames(emit func([]byte)) *frames { return &frames{emit: emit} }

func (f *frames) feed(b []byte) {
	for len(b) > 0 {
		if !f.active {
			f.header = append(f.header, b[0])
			b = b[1:]
			if len(f.header) < 2 {
				continue
			}
			need := frameHeaderSize(f.header)
			if len(f.header) < need {
				continue
			}
			f.remaining = framePayloadSize(f.header)
			f.opcode = f.header[0] & 15
			f.fin = f.header[0]&128 != 0
			if f.opcode == 1 || f.opcode == 2 {
				f.metadata.reset()
				f.message = nil
				f.large = false
				f.skip = f.header[0]&112 != 0
			}
			f.masked = f.header[1]&128 != 0
			if f.masked {
				copy(f.mask[:], f.header[need-4:need])
			}
			f.position = 0
			f.header = nil
			f.active = true
		}
		n := uint64(len(b))
		if n > f.remaining {
			n = f.remaining
		}
		if f.opcode < 8 && !f.skip {
			if f.lifecycle != nil {
				for i := uint64(0); i < n; i++ {
					c := b[i]
					if f.masked {
						c ^= f.mask[(f.position+i)%4]
					}
					f.metadata.feed(c)
				}
			}
			start := len(f.message)
			capture(&f.message, &f.large, b[:int(n)])
			if f.masked && !f.large {
				for i := uint64(0); i < n; i++ {
					f.message[start+int(i)] ^= f.mask[(f.position+i)%4]
				}
			}
		}
		f.position += n
		b = b[int(n):]
		f.remaining -= n
		if f.remaining == 0 {
			if f.opcode < 8 && f.fin {
				if !f.skip && f.lifecycle != nil {
					f.lifecycle(f.metadata.value)
				}
				if !f.skip && !f.large {
					f.emit(f.message)
				}
				f.message = nil
			}
			f.active = false
		}
	}
}
