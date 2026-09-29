package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Match the installed Codex 0.155.1 image tool contract. These are local
// application endpoints only; native image requests remain byte-preserving.
func prepareImageRR(w http.ResponseWriter, r *http.Request, upstream string) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return false
	}
	edit := strings.HasSuffix(r.URL.Path, "/edits")
	expected := "/backend-api/codex/images/generations"
	if edit {
		expected = "/backend-api/codex/images/edits"
	}
	if upstream != expected {
		http.Error(w, "image upstream changed; review Codex image API compatibility", 503)
		return false
	}
	if r.URL.RawQuery != "" || r.Header.Get("Content-Encoding") != "" {
		http.Error(w, "send uncompressed JSON without query parameters", 400)
		return false
	}
	var input struct {
		Prompt string `json:"prompt"`
		Images []struct {
			URL string `json:"image_url"`
		} `json:"images"`
	}
	if r.Body == nil {
		http.Error(w, "JSON body required", 400)
		return false
	}
	defer r.Body.Close()
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20))
	d.DisallowUnknownFields()
	err := d.Decode(&input)
	var extra any
	if err == nil && d.Decode(&extra) != io.EOF {
		err = errors.New("invalid trailing JSON")
	}
	if err != nil || strings.TrimSpace(input.Prompt) == "" ||
		(edit && (len(input.Images) == 0 || len(input.Images) > 5)) ||
		(!edit && len(input.Images) != 0) {
		http.Error(w, "only prompt and images are accepted; prompt is required; edits require 1-5 images", 400)
		return false
	}
	for _, image := range input.Images {
		if strings.TrimSpace(image.URL) == "" {
			http.Error(w, "each image requires image_url", 400)
			return false
		}
	}
	body := map[string]any{"prompt": input.Prompt, "model": "gpt-image-2", "background": "auto", "quality": "auto", "size": "auto"}
	if edit {
		body["images"] = input.Images
	}
	encoded, _ := json.Marshal(body)
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.ContentLength = int64(len(encoded))
	r.TransferEncoding = nil
	r.Header.Set("Content-Type", "application/json")
	r.Header.Del("Content-Length")
	return true
}
