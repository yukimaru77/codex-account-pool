package pool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/cpa"
)

func TestQuota401RefreshesAndPersistsBeforeRetryingSameAccount(t *testing.T) {
	for _, retryStatus := range []int{200, 401} {
		t.Run(fmt.Sprint(retryStatus), func(t *testing.T) {
			h, accounts := relayFixture(t, 90)
			account := accounts[0]
			quotaCalls, refreshCalls := 0, 0
			h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body := 200, ""
				switch r.URL.String() {
				case cpa.TokenURL:
					refreshCalls++
					if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != account.RefreshToken || r.Form.Get("client_id") != cpa.ClientID {
						t.Fatal("incorrect refresh request")
					}
					body = `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`
				case "https://chatgpt.com/backend-api/wham/usage":
					quotaCalls++
					if r.Method != "GET" || r.Header.Get("Chatgpt-Account-Id") != account.AccountID {
						t.Fatal("quota retry changed account or method")
					}
					if quotaCalls == 1 {
						if r.Header.Get("Authorization") != "Bearer "+account.AccessToken {
							t.Fatal("initial quota probe used wrong token")
						}
						status, body = 401, `{"error":"expired"}`
					} else {
						if quotaCalls > 2 || r.Header.Get("Authorization") != "Bearer rotated-access" {
							t.Fatal("quota retried too often or used stale token")
						}
						stored, err := h.Store.read(account.ID())
						if err != nil || stored.RefreshToken != "rotated-refresh" {
							t.Fatal("rotated token was not persisted before retry")
						}
						status = retryStatus
						body = fmt.Sprintf(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":%d}}}`, time.Now().Add(time.Hour).Unix())
					}
				default:
					t.Fatal("unexpected upstream URL")
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			auth := cpa.NewCodexAuth(&http.Client{Transport: h.Transport})
			h.Store.Refresh = func(ctx context.Context, token string) (*cpa.CodexTokenData, error) {
				return auth.RefreshTokensWithRetry(ctx, token, 3)
			}
			q, err := h.FetchQuota(context.Background(), account.ID())
			if (err == nil) != (retryStatus == 200) || quotaCalls != 2 || refreshCalls != 1 {
				t.Fatalf("quota=%d refresh=%d err=%v", quotaCalls, refreshCalls, err)
			}
			if err == nil && (q.Weekly.Seconds != 604800 || q.Weekly.Used != 20) {
				t.Fatal("retried quota was not parsed")
			}
		})
	}
}

func TestPollRefreshFailurePreservesCredentialAndExcludesAccount(t *testing.T) {
	h, accounts := relayFixture(t, 90, 90)
	failed := accounts[0]
	failed.Expire = time.Now().Add(-time.Hour).Format(time.RFC3339)
	if _, err := h.Store.Login(failed); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(h.Store.path(failed.ID()))
	refreshCalls := 0
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := 200, ""
		if r.URL.String() == cpa.TokenURL {
			refreshCalls++
			status, body = 400, `{"error":{"code":"refresh_token_reused","message":"must never enter status"}}`
		} else {
			if r.URL.String() != "https://chatgpt.com/backend-api/wham/usage" || r.Header.Get("Chatgpt-Account-Id") != accounts[1].AccountID {
				t.Error("failed account used after refresh failure")
			}
			body = fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":%d}}}`, time.Now().Add(time.Hour).Unix())
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	auth := cpa.NewCodexAuth(&http.Client{Transport: h.Transport})
	h.Store.Refresh = func(ctx context.Context, token string) (*cpa.CodexTokenData, error) {
		return auth.RefreshTokensWithRetry(ctx, token, 3)
	}
	if err := h.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(h.Store.path(failed.ID()))
	if refreshCalls != 1 || string(before) != string(after) {
		t.Fatal("terminal refresh error retried or overwrote credential")
	}
	for _, policy := range []Policy{FillFirst, RoundRobin} {
		id, err := h.Scheduler.Select(policy, "/backend-api/codex/responses", "")
		if err != nil || id != accounts[1].ID() {
			t.Fatal("failed account remains selectable")
		}
	}
	for _, status := range h.Scheduler.Status() {
		if strings.Contains(status.Error, "must never enter status") {
			t.Fatal("raw OAuth error leaked into status")
		}
	}
}
