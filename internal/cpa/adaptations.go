package cpa

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// Keep upstream error codes for retry classification without logging a token
// endpoint's arbitrary response body (which may contain credentials).
func tokenError(operation string, status int, body []byte) error {
	var v struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	_ = json.Unmarshal(body, &v)
	code := v.Code
	if code == "" {
		_ = json.Unmarshal(v.Error, &code)
	}
	if code == "" {
		var nested struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(v.Error, &nested)
		code = nested.Code
	}
	if !regexp.MustCompile(`^[a-z_]{1,64}$`).MatchString(code) {
		code = "upstream_error"
	}
	return fmt.Errorf("%s failed with status %d: %s", operation, status, code)
}
