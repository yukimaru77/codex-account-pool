package pool

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

func kbDecodeBody(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(encoding) {
	case "", "identity":
		return body, nil
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	case "zstd":
		r, err := zstd.NewReader(nil)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return r.DecodeAll(body, nil)
	default:
		return nil, fmt.Errorf("unsupported remote KB request encoding")
	}
}

func kbHTTPBody(r *http.Request, path string, snapshot *kbSnapshot) error {
	original, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return err
	}
	body, err := kbDecodeBody(original, r.Header.Get("Content-Encoding"))
	if err != nil {
		return err
	}
	out := body
	if gjson.ValidBytes(body) && !kbIsCompact(path, kbRequestIdentity(r.Header, body), body) {
		if gjson.GetBytes(body, "previous_response_id").String() != "" {
			return fmt.Errorf("remote KB HTTP requires a complete input history")
		}
		out = kbInject(body, snapshot)
	}
	if bytes.Equal(out, body) {
		out = original
	} else {
		// Only edited requests change their content encoding. Responses are raw.
		r.Header.Del("Content-Encoding")
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Del("Content-Length")
	return nil
}
