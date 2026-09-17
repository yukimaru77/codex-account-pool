package pool

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/cpa"
)

var testNow = time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

func quota(remaining float64, reset time.Duration) Quota {
	return Quota{Weekly: Window{Used: 100 - remaining, Seconds: 604800, Reset: testNow.Add(reset)}, Allowed: true, Observed: testNow}
}
func testScheduler(t *testing.T, remaining ...float64) (*Scheduler, []string) {
	t.Helper()
	cfg := DefaultConfig()
	s := NewScheduler(cfg)
	s.now = func() time.Time { return testNow }
	var credentials []Credential
	var ids []string
	for i := range remaining {
		c := Credential{CodexTokenData: cpa.CodexTokenData{AccountID: fmt.Sprintf("account-%d", i)}}
		credentials = append(credentials, c)
		ids = append(ids, c.ID())
	}
	s.Sync(credentials)
	for i, r := range remaining {
		s.Observe(ids[i], quota(r, time.Duration(i+1)*time.Hour))
	}
	return s, ids
}

func TestFillFirstUsesWeeklyResetOrderUntilConfigurableReserve(t *testing.T) {
	s, ids := testScheduler(t, 60, 99, 80)
	cfg := DefaultConfig()
	cfg.ReservePercent = 37
	s.Configure(cfg)
	for range 5 {
		got, err := s.Select(FillFirst, "responses", "")
		if err != nil || got != ids[0] {
			t.Fatalf("got %s %v", got, err)
		}
	}
	s.Observe(ids[0], quota(37, time.Hour))
	got, err := s.Select(FillFirst, "responses", "")
	if err != nil || got != ids[1] {
		t.Fatalf("M boundary: %s %v", got, err)
	}
	// A weekly reset is eligible again only after observing the upstream's new quota.
	s.Observe(ids[0], quota(100, 7*24*time.Hour))
	got, _ = s.Select(FillFirst, "responses", "")
	if got != ids[1] {
		t.Fatal("reset account incorrectly outranked the earlier weekly reset")
	}
}

func TestRoundRobinIncludesReserveForNAccountsAndKeepsIndependentCursors(t *testing.T) {
	s, ids := testScheduler(t, 1, 2, 3, 4, 5, 6, 7)
	sort.Strings(ids)
	for range 3 {
		for _, want := range ids {
			got, err := s.Select(RoundRobin, "images", "")
			if err != nil || got != want {
				t.Fatalf("got %s want %s: %v", got, want, err)
			}
		}
	}
	got, _ := s.Select(RoundRobin, "compact", "")
	if got != ids[0] {
		t.Fatal("round-robin routes shared a cursor")
	}
	if _, err := s.Select(FillFirst, "images", ""); !errors.Is(err, ErrNoAccount) {
		t.Fatal("normal image endpoint consumed reserve")
	}
}

func TestRoundRobinConcurrentSelection(t *testing.T) {
	s, ids := testScheduler(t, 3, 4, 5, 6, 7)
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[string]int{}
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.Select(RoundRobin, "images", "")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			counts[id]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if counts[id] != 20 {
			t.Fatalf("unfair selection counts=%v", counts)
		}
	}
}

func TestSchedulerSkipsExhaustedStaleDisabledAndRPMWait(t *testing.T) {
	s, ids := testScheduler(t, 0, 50, 50, 50, 50)
	q := quota(50, 2*time.Hour)
	q.Observed = testNow.Add(-time.Hour)
	s.Observe(ids[1], q)
	a := s.accounts[ids[2]]
	a.Disabled = true
	s.accounts[ids[2]] = a
	s.Cooldown(ids[3], "images", testNow.Add(time.Minute))
	got, err := s.Select(RoundRobin, "images", "")
	if err != nil || got != ids[4] {
		t.Fatalf("got %s %v", got, err)
	}
	// Endpoint RPM cooldown does not suppress an unrelated request type.
	got, err = s.Select(FillFirst, "compact", "")
	if err != nil || got != ids[3] {
		t.Fatalf("endpoint RPM isolation: %s %v", got, err)
	}
}

func TestShortWindowAffectsAvailabilityButNotWeeklyOrder(t *testing.T) {
	s, ids := testScheduler(t, 60, 60)
	q := quota(60, time.Hour)
	q.Other = []Window{{Used: 10, Seconds: 18000, Reset: testNow.Add(4 * time.Hour)}}
	s.Observe(ids[0], q)
	q2 := quota(60, 2*time.Hour)
	q2.Other = []Window{{Used: 10, Seconds: 18000, Reset: testNow.Add(time.Minute)}}
	s.Observe(ids[1], q2)
	got, _ := s.Select(FillFirst, "responses", "")
	if got != ids[0] {
		t.Fatal("sorted by short window instead of weekly")
	}
	q.Other[0].Used = 100
	s.Observe(ids[0], q)
	got, _ = s.Select(FillFirst, "responses", "")
	if got != ids[1] {
		t.Fatal("ignored exhausted short window")
	}
}

func TestPinnedResourceNeverSilentlyMoves(t *testing.T) {
	s, ids := testScheduler(t, 5, 90)
	if _, err := s.Select(FillFirst, "responses", ids[0]); !errors.Is(err, ErrPinnedUnavailable) {
		t.Fatal("resource silently moved")
	}
	got, err := s.Select(RoundRobin, "images", ids[0])
	if err != nil || got != ids[0] {
		t.Fatalf("round-robin resource reserve unavailable: %s %v", got, err)
	}
}
