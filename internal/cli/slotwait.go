package cli

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"
)

// Bounds of waiting for a plan slot.
const (
	maxSlotWait    = time.Hour
	slotBackoff    = 5 * time.Second
	slotBackoffMax = time.Minute
)

// postPlan sends a plan request. With a slot wait, writ serve's 429 or
// 503, as when every plan slot of the principal or the server is busy, is
// asked again with backoff and jitter until the wait ends; a job of
// overlapping workflow runs then waits for a slot rather than failing. A
// refused plan spends no token. A token from GitHub Actions is asked for
// again before each try, so it does not expire while the job waits.
func (a *App) postPlan(ctx context.Context, r *remote, body []byte, wait time.Duration) (int, []byte, error) {
	if wait == 0 {
		return r.call(ctx, http.MethodPost, "/v1/plans", body, remoteTimeout)
	}
	deadline := time.Now().Add(wait)
	backoff := min(slotBackoff, max(wait/16, 10*time.Millisecond))
	for {
		status, resp, err := r.once(ctx, http.MethodPost, "/v1/plans", body, remoteTimeout, nil)
		if err != nil || status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
			return status, resp, err
		}
		pause := backoff*3/4 + rand.N(backoff/2+1)
		if time.Now().Add(pause).After(deadline) {
			fmt.Fprintf(a.opts.Stderr, "writ: writ serve still refused the plan with %d after waiting %s for a plan slot\n", status, wait)
			return status, resp, nil
		}
		fmt.Fprintf(a.opts.Stderr, "writ: writ serve's plan slots are busy (%d); asking again in %s\n", status, pause.Round(time.Millisecond))
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
		backoff = min(backoff*2, slotBackoffMax, max(wait/2, backoff))
		if r.refresh != nil {
			tok, err := r.refresh(ctx)
			if err != nil {
				return 0, nil, err
			}
			r.token = tok
		}
	}
}
