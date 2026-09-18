package cpa

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// TokenEndpointError keeps only the status and bounded code, never the OAuth
// response body. See third_party/codex-lb for refresh failure classification.
type TokenEndpointError struct {
	Operation string
	Status    int
	Code      string
}

func (e *TokenEndpointError) Error() string {
	return fmt.Sprintf("%s failed with status %d: %s", e.Operation, e.Status, e.Code)
}

func (e *TokenEndpointError) Permanent() bool {
	switch e.Code {
	case "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated",
		"invalid_grant", "token_invalidated", "token_expired", "app_session_terminated",
		"account_session_expired", "account_auth_invalidated", "invalid_refresh_token",
		"account_deactivated", "account_suspended", "account_deleted":
		return true
	}
	return false
}

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
	return &TokenEndpointError{Operation: operation, Status: status, Code: code}
}
