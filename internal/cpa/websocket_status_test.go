package cpa

import "testing"

func TestCodexWebsocketErrorStatusUsesCPAStatusSpellings(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"type":"error","status":401}`, 401},
		{`{"type":"error","status_code":429}`, 429},
		{`{"type":"error","status":429,"status_code":500}`, 429},
		{`{"type":"error","unknown":true}`, 0},
		{`{"type":"future.event","status":429}`, 0},
		{`{}`, 0},
	} {
		if got := CodexWebsocketErrorStatus([]byte(tc.body)); got != tc.want {
			t.Fatalf("%s: got %d want %d", tc.body, got, tc.want)
		}
	}
}
