package pool

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"codex-account-pool/internal/cpa"
)

func TestQuotaRecognizesWeeklyWindowInEitherPosition(t *testing.T) {
	for _, name := range []string{"primary_window", "secondary_window"} {
		b := []byte(fmt.Sprintf(`{"rate_limit":{"allowed":true,%q:{"used_percent":64.5,"limit_window_seconds":604800,"reset_at":%d}},"future":{"unknown":true}}`, name, testNow.Add(time.Hour).Unix()))
		q, err := ParseQuota(b, testNow)
		if err != nil || q.Weekly.Used != 64.5 || q.Weekly.Seconds != 604800 {
			t.Fatalf("weekly parse %+v %v", q, err)
		}
	}
}

func TestQuotaCPAEventReuse(t *testing.T) {
	b := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":25,"window_minutes":10080,"reset_after_seconds":3600},"secondary":{"used_percent":10,"window_minutes":300,"reset_after_seconds":60}}}`)
	q, ok := QuotaFromHeaders(cpa.ParseCodexQuotaEventHeaders(b), testNow)
	if !ok || q.Weekly.Used != 25 || !q.Weekly.Reset.Equal(testNow.Add(time.Hour)) || len(q.Other) != 1 {
		t.Fatalf("CPA normalized quota %+v %v", q, ok)
	}
}

func TestQuotaMissingWeeklyInvalidAndExhaustedNeverInvented(t *testing.T) {
	for _, body := range []string{`{}`, `not json`, `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":1234}}}`, `{"rate_limit":{"primary_window":{"used_percent":101,"limit_window_seconds":604800,"reset_at":1234}}}`} {
		if _, err := ParseQuota([]byte(body), testNow); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if _, ok := QuotaFromHeaders(http.Header{"X-Codex-Primary-Used-Percent": {"NaN"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"100"}}, testNow); ok {
		t.Fatal("NaN accepted")
	}
	q := quota(90, -time.Second)
	if q.usable(testNow, time.Minute, 10) {
		t.Fatal("assumed expired window automatically reset")
	}
}

func TestModelSpecificQuotaCannotReplaceMainWeeklyBudget(t *testing.T) {
	for _, field := range []string{"limit_id", "metered_limit_name", "limit_name", "meteredLimitName", "limitName"} {
		for _, bucket := range []string{"codex", "GPT-5.3-Codex-Spark"} {
			b := []byte(fmt.Sprintf(`{"type":"codex.rate_limits",%q:%q,"rate_limits":{"primary":{"used_percent":100,"window_minutes":10080,"reset_after_seconds":3600}}}`, field, bucket))
			if _, ok := QuotaFromHeaders(quotaEventHeaders(b), testNow); ok != (bucket == "codex") {
				t.Fatalf("wrong quota family accepted: %s", b)
			}
		}
	}
	h := http.Header{"X-Codex-Primary-Used-Percent": {"25"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}, "X-Codex-Active-Limit": {"primary"}, "X-Codex-Spark-Primary-Used-Percent": {"100"}}
	if q, ok := QuotaFromHeaders(h, testNow); !ok || q.Weekly.Used != 25 {
		t.Fatal("main header family confused with additional limit metadata")
	}
}
