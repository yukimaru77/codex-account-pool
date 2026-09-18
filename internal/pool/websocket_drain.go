package pool

import "sync"

// Track only Responses lifecycle metadata with bounded memory.
// No request is rewritten or replayed. Inspired by codex-lb's separation of
// pending/terminal requests and downstream terminal delivery (see third_party).
type websocketDrain struct {
	mu        sync.Mutex
	changed   *sync.Cond
	awaiting  int
	active    map[string]bool
	terminals []string
	closed    bool
}

func newWebsocketDrain() *websocketDrain {
	d := &websocketDrain{active: make(map[string]bool)}
	d.changed = sync.NewCond(&d.mu)
	return d
}

func (d *websocketDrain) request(event lifecycleEvent) {
	if event.typ != "response.create" {
		return
	}
	d.mu.Lock()
	d.awaiting++
	d.mu.Unlock()
}

func (d *websocketDrain) event(event lifecycleEvent) {
	id := event.id
	d.mu.Lock()
	defer d.mu.Unlock()
	switch event.typ {
	case "response.created":
		if id != "" && !d.active[id] {
			if d.awaiting > 0 {
				d.awaiting--
			}
			d.active[id] = true
		}
	case "response.completed", "response.failed", "response.incomplete":
		d.terminals = append(d.terminals, id)
	case "error":
		// An error with no response id can reject a pre-created request. It
		// does not prove a different active response finished.
		d.terminals = append(d.terminals, id)
	}
}

// Called by the next Read, after ReverseProxy has written the preceding chunk
// to the downstream socket. Parsing a terminal before that write is too early
// to let the other copy goroutine close both ends of the WebSocket.
func (d *websocketDrain) delivered() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range d.terminals {
		if id != "" && d.active[id] {
			delete(d.active, id)
		} else if id == "" && d.awaiting > 0 {
			d.awaiting--
		}
	}
	d.terminals = nil
	d.changed.Broadcast()
}

func (d *websocketDrain) wait() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for !d.closed && (d.awaiting > 0 || len(d.active) > 0) {
		d.changed.Wait()
	}
}

func (d *websocketDrain) close() {
	d.mu.Lock()
	d.closed = true
	d.changed.Broadcast()
	d.mu.Unlock()
}
