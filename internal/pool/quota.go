package pool

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
)

type Window struct {
	Used    float64   `json:"used_percent"`
	Seconds int64     `json:"window_seconds"`
	Reset   time.Time `json:"reset_at"`
}
type Quota struct {
	Weekly   Window    `json:"weekly"`
	Other    []Window  `json:"other_windows,omitempty"`
	Observed time.Time `json:"observed_at"`
	Allowed  bool      `json:"allowed"`
}

func parseWindow(v gjson.Result, now time.Time) (Window, bool) {
	used := v.Get("used_percent")
	seconds := v.Get("limit_window_seconds").Int()
	if seconds == 0 {
		seconds = v.Get("window_minutes").Int() * 60
	}
	w := Window{Used: used.Float(), Seconds: seconds}
	if !used.Exists() || used.Type != gjson.Number || math.IsNaN(w.Used) || w.Used < 0 || w.Used > 100 || seconds <= 0 {
		return w, false
	}
	if reset := v.Get("reset_at"); reset.Int() > 0 {
		w.Reset = time.Unix(reset.Int(), 0)
	} else if after := v.Get("reset_after_seconds"); after.Exists() && after.Int() >= 0 {
		w.Reset = now.Add(time.Duration(after.Int()) * time.Second)
	}
	return w, !w.Reset.IsZero()
}

// A weekly window is identified by its duration, not primary/secondary position.
// /wham/usage and CPA's normalized codex.rate_limits event have different names.
func ParseQuota(b []byte, now time.Time) (Quota, error) {
	q := Quota{Observed: now, Allowed: true}
	if !gjson.ValidBytes(b) {
		return q, fmt.Errorf("invalid quota JSON")
	}
	root := gjson.ParseBytes(b)
	limits := root.Get("rate_limit")
	if !limits.Exists() {
		limits = root.Get("rate_limits")
	}
	if !limits.IsObject() {
		return q, fmt.Errorf("missing quota windows")
	}
	if a := limits.Get("allowed"); a.Exists() {
		q.Allowed = a.Bool()
	}
	if limits.Get("limit_reached").Bool() {
		q.Allowed = false
	}
	for _, name := range []string{"primary_window", "secondary_window", "primary", "secondary"} {
		if w, ok := parseWindow(limits.Get(name), now); ok {
			if w.Seconds == 7*24*60*60 {
				q.Weekly = w
			} else {
				q.Other = append(q.Other, w)
			}
		}
	}
	if q.Weekly.Seconds == 0 {
		return q, fmt.Errorf("no observed weekly window")
	}
	return q, nil
}

// QuotaFromHeaders consumes CPA's standard X-Codex window representation.
func QuotaFromHeaders(h http.Header, now time.Time) (Quota, bool) {
	q := Quota{Observed: now, Allowed: true}
	for _, name := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + name + "-"
		used, errU := strconv.ParseFloat(h.Get(prefix+"Used-Percent"), 64)
		minutes, errM := strconv.ParseInt(h.Get(prefix+"Window-Minutes"), 10, 64)
		if errU != nil || errM != nil || math.IsNaN(used) || used < 0 || used > 100 || minutes <= 0 {
			continue
		}
		var reset time.Time
		if unix, err := strconv.ParseInt(h.Get(prefix+"Reset-At"), 10, 64); err == nil && unix > 0 {
			reset = time.Unix(unix, 0)
		} else if after, err := strconv.ParseInt(h.Get(prefix+"Reset-After-Seconds"), 10, 64); err == nil && after >= 0 {
			reset = now.Add(time.Duration(after) * time.Second)
		}
		if reset.IsZero() {
			continue
		}
		w := Window{Used: used, Seconds: minutes * 60, Reset: reset}
		if w.Seconds == 604800 {
			q.Weekly = w
		} else {
			q.Other = append(q.Other, w)
		}
	}
	if a := h.Get("X-Codex-Allowed"); a != "" {
		q.Allowed = a == "true"
	}
	if h.Get("X-Codex-Limit-Reached") == "true" {
		q.Allowed = false
	}
	return q, q.Weekly.Seconds != 0
}

func (q Quota) usable(now time.Time, maxAge time.Duration, reserve float64) bool {
	if q.Weekly.Seconds != 604800 || !q.Allowed || q.Observed.IsZero() || now.Sub(q.Observed) > maxAge || !q.Weekly.Reset.After(now) || 100-q.Weekly.Used <= reserve {
		return false
	}
	for _, w := range q.Other {
		if w.Used >= 100 {
			return false
		}
	}
	return true
}
