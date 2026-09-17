package cpa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRefreshReturnsIDTokenAndAccountClaims(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"refreshed@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"account-a"}}`))
	idToken := "header." + claims + ".signature"
	body, err := json.Marshal(map[string]any{
		"access_token": "new-access", "refresh_token": "new-refresh",
		"id_token": idToken, "token_type": "Bearer", "expires_in": 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	auth := NewCodexAuth(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.String() != TokenURL || r.ParseForm() != nil {
			return nil, errors.New("unexpected refresh request")
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("scope") != "openid profile email" {
			return nil, errors.New("refresh did not request the OpenID identity scope")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})})
	got, err := auth.RefreshTokensWithRetry(context.Background(), "old-refresh", 3)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.IDToken != idToken || got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.AccountID != "account-a" || got.Email != "refreshed@example.invalid" {
		t.Fatal("refresh lost tokens or the ID token's account claims")
	}
	if expiry, err := time.Parse(time.RFC3339, got.Expire); err != nil || !expiry.After(time.Now()) {
		t.Fatal("refresh lost the access token expiry")
	}
}
