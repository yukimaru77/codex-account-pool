package pool

import "encoding/json"

// Read only three lifecycle fields, regardless of their order or message size.
// The regular usage observer has a size limit; using it to decide when a socket
// may close would miss large response.completed messages. This scanner never
// buffers input/output strings or validates/rejects the forwarded message.
type websocketLifecycle struct {
	depth, responseDepth int
	inString, escaped    bool
	key, collect, large  bool
	topKey, responseKey  string
	topExpect, subExpect bool
	str                  []byte
	value                lifecycleEvent
}

type lifecycleEvent struct{ typ, id string }

func (s *websocketLifecycle) reset() { *s = websocketLifecycle{} }

func (s *websocketLifecycle) feed(c byte) {
	if s.inString {
		if s.collect && !s.large {
			if len(s.str) == 1024 {
				s.str, s.large = nil, true
			} else {
				s.str = append(s.str, c)
			}
		}
		if s.escaped {
			s.escaped = false
			return
		}
		if c == '\\' {
			s.escaped = true
			return
		}
		if c != '"' {
			return
		}
		s.inString = false
		var value string
		if s.collect && !s.large {
			_ = json.Unmarshal(s.str, &value)
		}
		if s.key {
			if s.depth == 1 {
				s.topKey, s.topExpect = value, false
			} else {
				s.responseKey, s.subExpect = value, false
			}
		} else if s.depth == 1 {
			if s.topKey == "type" {
				s.value.typ = value
			} else if s.topKey == "response_id" && s.value.id == "" {
				s.value.id = value
			}
		} else if s.responseDepth != 0 && s.depth == s.responseDepth && s.responseKey == "id" {
			s.value.id = value
		}
		return
	}
	switch c {
	case '"':
		s.inString, s.escaped, s.large = true, false, false
		s.key, s.collect = false, false
		if s.depth == 1 {
			s.key = s.topExpect
			s.collect = s.key || s.topKey == "type" || s.topKey == "response_id"
		} else if s.depth == s.responseDepth && s.responseDepth != 0 {
			s.key = s.subExpect
			s.collect = s.key || s.responseKey == "id"
		}
		s.str = nil
		if s.collect {
			s.str = append(s.str, '"')
		}
	case '{', '[':
		s.depth++
		if s.depth == 1 {
			s.topExpect = c == '{'
		} else if s.depth == 2 && s.topKey == "response" && c == '{' {
			s.responseDepth, s.subExpect, s.responseKey = 2, true, ""
		}
	case '}', ']':
		if s.depth == s.responseDepth {
			s.responseDepth = 0
		}
		s.depth--
	case ',':
		if s.depth == 1 {
			s.topExpect, s.topKey = true, ""
		} else if s.depth == s.responseDepth && s.responseDepth != 0 {
			s.subExpect, s.responseKey = true, ""
		}
	}
}
