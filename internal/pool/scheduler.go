package pool

import (
	"errors"
	"sort"
	"sync"
	"time"
)

type Policy string

const FillFirst Policy = "fill-first"
const RoundRobin Policy = "round-robin"

var ErrNoAccount = errors.New("no account has observed quota available for this route")
var ErrPinnedUnavailable = errors.New("the resource's account is unavailable for this route; refusing to move it")

type AccountStatus struct {
	ID        string               `json:"auth_index"`
	Email     string               `json:"email"`
	Disabled  bool                 `json:"disabled"`
	Quota     Quota                `json:"quota"`
	Error     string               `json:"error,omitempty"`
	Cooldowns map[string]time.Time `json:"cooldowns,omitempty"`
}

type Scheduler struct {
	mu       sync.Mutex
	accounts map[string]AccountStatus
	failedAt map[string]time.Time
	last     map[string]string
	reserve  float64
	maxAge   time.Duration
	now      func() time.Time
}

func NewScheduler(cfg Config) *Scheduler {
	return &Scheduler{accounts: make(map[string]AccountStatus), failedAt: make(map[string]time.Time), last: make(map[string]string), reserve: cfg.ReservePercent, maxAge: cfg.maxAge(), now: time.Now}
}
func (s *Scheduler) Configure(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserve = cfg.ReservePercent
	s.maxAge = cfg.maxAge()
}
func (s *Scheduler) Sync(accounts []Credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, c := range accounts {
		id := c.ID()
		a := s.accounts[id]
		a.ID = id
		a.Email = c.Email
		a.Disabled = c.Disabled
		s.accounts[id] = a
		seen[id] = true
	}
	for id := range s.accounts {
		if !seen[id] {
			delete(s.accounts, id)
			delete(s.failedAt, id)
		}
	}
}
func (s *Scheduler) Observe(id string, q Quota) {
	s.observe(id, q, false)
}

// Only a successful dedicated quota probe can clear an account failure.
// Late events on an older streaming connection do not prove credentials work.
func (s *Scheduler) ObserveVerified(id string, q Quota) {
	s.observe(id, q, true)
}

func (s *Scheduler) observe(id string, q Quota, verified bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return
	}
	if !q.Observed.Before(a.Quota.Observed) {
		a.Quota = q
	}
	if verified && !q.Observed.Before(s.failedAt[id]) {
		a.Error = ""
		delete(s.failedAt, id)
	}
	s.accounts[id] = a
}
func (s *Scheduler) Failure(id, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return
	}
	a.Error = message
	s.failedAt[id] = s.now()
	s.accounts[id] = a
}
func (s *Scheduler) Cooldown(id, path string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return
	}
	if a.Cooldowns == nil {
		a.Cooldowns = map[string]time.Time{}
	}
	if until.IsZero() || until.After(a.Cooldowns[path]) {
		a.Cooldowns[path] = until
	}
	s.accounts[id] = a
}

func (s *Scheduler) Select(policy Policy, route, pinned string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	reserve := s.reserve
	if policy == RoundRobin {
		reserve = 0
	}
	usable := func(a AccountStatus) bool {
		return !a.Disabled && a.Error == "" && !a.Cooldowns[route].After(now) && a.Quota.usable(now, s.maxAge, reserve)
	}
	if pinned != "" {
		a, ok := s.accounts[pinned]
		if !ok || !usable(a) {
			return "", ErrPinnedUnavailable
		}
		return pinned, nil
	}
	var candidates []AccountStatus
	for _, a := range s.accounts {
		if usable(a) {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		return "", ErrNoAccount
	}
	if policy == RoundRobin {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		last := s.last[route]
		chosen := candidates[0].ID
		for _, a := range candidates {
			if a.ID > last {
				chosen = a.ID
				break
			}
		}
		s.last[route] = chosen
		return chosen, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Quota.Weekly.Reset.Equal(b.Quota.Weekly.Reset) {
			return a.ID < b.ID
		}
		return a.Quota.Weekly.Reset.Before(b.Quota.Weekly.Reset)
	})
	return candidates[0].ID, nil
}

func (s *Scheduler) Status() []AccountStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AccountStatus, 0, len(s.accounts))
	for _, a := range s.accounts {
		copy := a
		copy.Quota.Other = append([]Window(nil), a.Quota.Other...)
		copy.Cooldowns = map[string]time.Time{}
		for k, v := range a.Cooldowns {
			copy.Cooldowns[k] = v
		}
		out = append(out, copy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
