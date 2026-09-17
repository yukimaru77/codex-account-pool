package pool

// Remote KB is opt-in. Unregistered traffic never goes through the JSON editor.
import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/tidwall/gjson"
)

type kbSnapshot struct {
	ID    string          `json:"snapshot_id"`
	Items json.RawMessage `json:"items"`
}

type kbBinding struct {
	SessionID  string `json:"session_id"`
	SnapshotID string `json:"snapshot_id"`
}

type remoteKBStore struct {
	dir       string
	mu        sync.Mutex
	bindings  map[string]*kbSnapshot
	snapshots map[string]*kbSnapshot
}

func newRemoteKBStore(dir string) *remoteKBStore {
	return &remoteKBStore{dir: filepath.Join(dir, "remote-kb"), bindings: map[string]*kbSnapshot{}, snapshots: map[string]*kbSnapshot{}}
}

func kbHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func (s *remoteKBStore) bindingPath(id string) string {
	return filepath.Join(s.dir, "sessions", kbHash([]byte(id))+".json")
}

// Call with mu held. A missing registration is ordinary passthrough traffic.
func (s *remoteKBStore) lookup(id string) (*kbSnapshot, error) {
	if id == "" {
		return nil, nil
	}
	if snapshot, ok := s.bindings[id]; ok {
		return snapshot, nil
	}
	b, err := os.ReadFile(s.bindingPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var binding kbBinding
	if err = json.Unmarshal(b, &binding); err != nil {
		return nil, err
	}
	if binding.SessionID != id || len(binding.SnapshotID) != 64 {
		return nil, errors.New("invalid KB binding")
	}
	if _, err = hex.DecodeString(binding.SnapshotID); err != nil {
		return nil, err
	}
	snapshot := s.snapshots[binding.SnapshotID]
	if snapshot == nil {
		b, err = os.ReadFile(filepath.Join(s.dir, "snapshots", binding.SnapshotID+".json"))
		if err != nil {
			return nil, err
		}
		if kbHash(b) != binding.SnapshotID {
			return nil, errors.New("KB snapshot content mismatch")
		}
		snapshot = &kbSnapshot{ID: binding.SnapshotID, Items: b}
		s.snapshots[snapshot.ID] = snapshot
	}
	s.bindings[id] = snapshot
	return snapshot, nil
}

func (s *remoteKBStore) saveBinding(id string, snapshot *kbSnapshot) error {
	if err := os.MkdirAll(filepath.Join(s.dir, "sessions"), 0700); err != nil {
		return err
	}
	b, _ := json.Marshal(kbBinding{SessionID: id, SnapshotID: snapshot.ID})
	if err := AtomicWrite(s.bindingPath(id), b); err != nil {
		return err
	}
	s.bindings[id] = snapshot
	return nil
}

var errKBBound = errors.New("session already has a different KB snapshot")

func (s *remoteKBStore) bind(id string, items json.RawMessage) (*kbSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var compact bytes.Buffer
	if err := json.Compact(&compact, items); err != nil {
		return nil, err
	}
	snapshot := &kbSnapshot{ID: kbHash(compact.Bytes()), Items: append([]byte(nil), compact.Bytes()...)}
	existing, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.ID != snapshot.ID {
			return nil, errKBBound
		}
		return existing, nil
	}
	if err = os.MkdirAll(filepath.Join(s.dir, "snapshots"), 0700); err != nil {
		return nil, err
	}
	if err = AtomicWrite(filepath.Join(s.dir, "snapshots", snapshot.ID+".json"), snapshot.Items); err != nil {
		return nil, err
	}
	s.snapshots[snapshot.ID] = snapshot
	if err = s.saveBinding(id, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

type kbIdentity struct{ session, thread, parent, kind string }

func kbRequestIdentity(headers http.Header, body []byte) kbIdentity {
	m := kbIdentity{session: headers.Get("Session-Id"), thread: headers.Get("Thread-Id"), parent: headers.Get("X-Codex-Parent-Thread-Id")}
	apply := func(v gjson.Result) {
		if x := v.Get("session_id").String(); x != "" {
			m.session = x
		}
		if x := v.Get("thread_id").String(); x != "" {
			m.thread = x
		}
		if x := v.Get("parent_thread_id").String(); x != "" {
			m.parent = x
		}
		if x := v.Get("request_kind").String(); x != "" {
			m.kind = x
		}
	}
	apply(gjson.Parse(headers.Get("X-Codex-Turn-Metadata")))
	if len(body) > 0 {
		metadata := gjson.GetBytes(body, "client_metadata")
		apply(metadata)
		if x := metadata.Get("x-codex-parent-thread-id").String(); x != "" {
			m.parent = x
		}
		apply(gjson.Parse(metadata.Get("x-codex-turn-metadata").String()))
	}
	return m
}

func (s *remoteKBStore) resolve(m kbIdentity) (*kbSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A child can have its own explicit binding. Otherwise inherit and persist
	// the parent's fixed snapshot so grandchildren and resumes use it too.
	if b, err := s.lookup(m.thread); b != nil || err != nil {
		return b, err
	}
	if b, err := s.lookup(m.session); b != nil || err != nil {
		if err == nil && m.thread != "" {
			err = s.saveBinding(m.thread, b)
		}
		return b, err
	}
	b, err := s.lookup(m.parent)
	if err != nil || b == nil {
		return b, err
	}
	for _, id := range []string{m.thread, m.session} {
		if id != "" {
			if err = s.saveBinding(id, b); err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

func (h *Handler) remoteKBAPI(w http.ResponseWriter, r *http.Request) {
	var snapshot *kbSnapshot
	var err error
	id := r.URL.Query().Get("session_id")
	if r.Method == "POST" {
		var request struct {
			SessionID string          `json:"session_id"`
			Items     json.RawMessage `json:"items"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.SessionID == "" || !gjson.ParseBytes(request.Items).IsArray() || len(gjson.ParseBytes(request.Items).Array()) == 0 {
			http.Error(w, "session_id and a nonempty items array are required", 400)
			return
		}
		id = request.SessionID
		snapshot, err = h.RemoteKB.bind(id, request.Items)
	} else if r.Method == "GET" {
		snapshot, err = h.RemoteKB.resolve(kbIdentity{session: id})
	} else {
		http.Error(w, "method not allowed", 405)
		return
	}
	if errors.Is(err, errKBBound) {
		http.Error(w, err.Error(), 409)
		return
	}
	if err != nil {
		http.Error(w, "KB storage unavailable", 500)
		return
	}
	if snapshot == nil {
		http.Error(w, "KB session not registered", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		kbBinding
		Items int `json:"item_count"`
	}{kbBinding{id, snapshot.ID}, len(gjson.ParseBytes(snapshot.Items).Array())})
}

// Replace only the selected JSON value; all other bytes/unknown fields survive.
func kbReplace(body []byte, value gjson.Result, replacement []byte) []byte {
	out := make([]byte, 0, len(body)-len(value.Raw)+len(replacement))
	out = append(out, body[:value.Index]...)
	out = append(out, replacement...)
	return append(out, body[value.Index+len(value.Raw):]...)
}

func kbIsCompact(path string, m kbIdentity, body []byte) bool {
	if path == "/backend-api/codex/responses/compact" || m.kind == "compaction" {
		return true
	}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "compaction_trigger" {
			return true
		}
	}
	return false
}

func kbInject(body []byte, snapshot *kbSnapshot) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	items := input.Array()
	pos := 0
	for pos < len(items) {
		role := items[pos].Get("role").String()
		if role != "system" && role != "developer" {
			break
		}
		pos++
	}
	insertion := input.Index + 1
	prefix := append([]byte(nil), snapshot.Items[1:len(snapshot.Items)-1]...)
	if pos < len(items) {
		insertion = items[pos].Index
		prefix = append(prefix, ',')
	} else if len(items) > 0 {
		insertion = input.Index + len(input.Raw) - 1
		prefix = append([]byte{','}, prefix...)
	}
	out := make([]byte, 0, len(body)+len(prefix))
	out = append(out, body[:insertion]...)
	out = append(out, prefix...)
	return append(out, body[insertion:]...)
}
