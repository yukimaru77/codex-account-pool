package cpa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDeviceLoginUsesCPAFlowAndReturnsRefreshCredential(t *testing.T) {
	for _, codeField := range []string{"user_code", "usercode"} {
		t.Run(codeField, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" || r.Context().Err() != nil {
					t.Fatal("unexpected device request")
				}
				var body string
				switch r.URL.String() {
				case codexDeviceUserCodeURL:
					var req codexDeviceUserCodeRequest
					if json.NewDecoder(r.Body).Decode(&req) != nil || req.ClientID != ClientID {
						t.Fatal("wrong OAuth client")
					}
					body = fmt.Sprintf(`{"device_auth_id":"device-id",%q:"TEST-CODE","interval":"1"}`, codeField)
				case codexDeviceTokenURL:
					var req codexDeviceTokenRequest
					if json.NewDecoder(r.Body).Decode(&req) != nil || req.DeviceAuthID != "device-id" || req.UserCode != "TEST-CODE" {
						t.Fatal("wrong device polling identity")
					}
					body = `{"authorization_code":"code","code_verifier":"verifier","code_challenge":"challenge"}`
				case TokenURL:
					if r.ParseForm() != nil || r.Form.Get("code_verifier") != "verifier" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != codexDeviceTokenExchangeRedirectURI {
						t.Fatal("incorrect device code exchange")
					}
					claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"test@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"account-a"}}`))
					body = fmt.Sprintf(`{"access_token":"access","refresh_token":"refresh","id_token":%q,"expires_in":3600}`, "header."+claims+".signature")
				default:
					t.Fatal("unexpected OAuth destination")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			shown := false
			bundle, err := DeviceLogin(context.Background(), client, func(uri, code string) error {
				shown = true
				if uri != codexDeviceVerificationURL || code != "TEST-CODE" {
					t.Fatal("incorrect verification prompt")
				}
				return nil
			})
			if err != nil || !shown || calls != 3 {
				t.Fatalf("device flow calls=%d err=%v", calls, err)
			}
			if bundle.TokenData.AccountID != "account-a" || bundle.TokenData.RefreshToken != "refresh" || bundle.TokenData.AccessToken != "access" {
				t.Fatal("device flow lost credential identity")
			}
		})
	}
}

func TestDevicePollingPendingIsCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("pending")), Header: make(http.Header)}, nil
	})}
	_, err := pollCodexDeviceToken(ctx, client, "device", "code", time.Hour)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("device polling ignored cancellation")
	}
}
