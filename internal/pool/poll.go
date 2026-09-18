package pool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

func (h *Handler) FetchQuota(ctx context.Context, id string) (Quota, error) {
	var empty Quota
	started := time.Now()
	c, err := h.Store.Token(ctx, id, false)
	if err != nil {
		return empty, err
	}
	// Only this read-only quota probe retries a 401 after refreshing the same
	// account. Generation requests are never replayed or silently failed over.
	for attempt := 0; attempt < 2; attempt++ {
		r, err := http.NewRequestWithContext(ctx, "GET", "https://chatgpt.com/backend-api/wham/usage", nil)
		if err != nil {
			return empty, err
		}
		r.Header.Set("Authorization", "Bearer "+c.AccessToken)
		r.Header.Set("Chatgpt-Account-Id", c.AccountID)
		r.Header.Set("Accept", "application/json")
		resp, err := h.Transport.RoundTrip(r)
		if err != nil {
			return empty, fmt.Errorf("quota connection failed")
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == 401 && attempt == 0 {
			c, err = h.Store.RefreshRejected(ctx, id, c.AccessToken)
			if err != nil {
				return empty, err
			}
			continue
		}
		if resp.StatusCode != 200 {
			return empty, fmt.Errorf("quota HTTP %d", resp.StatusCode)
		}
		if readErr != nil {
			return empty, fmt.Errorf("quota read failed")
		}
		q, err := ParseQuota(b, time.Now())
		// A slow probe must not replace newer live observations or clear an
		// authentication failure that happened after this probe began.
		q.Observed = started
		return q, err
	}
	return empty, fmt.Errorf("quota authentication failed")
}

func (h *Handler) Poll(ctx context.Context) error {
	accounts, err := h.Store.List()
	if err != nil {
		return err
	}
	h.Scheduler.Sync(accounts)
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, a := range accounts {
		if a.Disabled {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-limit }()
			probe, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			q, err := h.FetchQuota(probe, id)
			if err != nil {
				h.Scheduler.Failure(id, "quota unavailable; refresh or login may be required")
				return
			}
			h.Scheduler.ObserveVerified(id, q)
		}(a.ID())
	}
	wg.Wait()
	return ctx.Err()
}

func (h *Handler) PollLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(h.Config.QuotaPollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = h.Poll(ctx)
		}
	}
}
