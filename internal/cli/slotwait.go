package cli

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Bounds of waiting for a plan slot.
const (
	maxSlotWait    = time.Hour
	slotBackoff    = 5 * time.Second
	slotBackoffMax = time.Minute
)

// postPlan sends a plan request. With a slot wait, writ serve's 429 or
// 503, as when every plan slot is busy or the forge limits the rate of
// calls, is asked again until the wait ends: after the answer's
// Retry-After when it says one, else with backoff and jitter. A job of
// overlapping workflow runs then waits rather than failing. A refused plan
// spends no token. A token from GitHub Actions is asked for again before
// each try, within the wait, so it does not expire meanwhile.
//
// No try starts after the wait ends. A plan request that started before
// it runs to its answer under the request's own timeout: cutting it could
// leave writ serve planning with an answer no one reads, and the token
// spent.
func (a *App) postPlan(ctx context.Context, r *remote, body []byte, wait time.Duration) (int, []byte, error) {
	if wait == 0 {
		return r.call(ctx, http.MethodPost, "/v1/plans", body, remoteTimeout)
	}
	deadline := time.Now().Add(wait)
	gaveUp := func(status int) {
		fmt.Fprintf(a.opts.Stderr, "writ: writ serve still refused the plan with %d after waiting %s for a plan slot\n", status, wait)
	}
	backoff := min(slotBackoff, max(wait/16, 10*time.Millisecond))
	for {
		status, hdr, resp, err := r.send(ctx, http.MethodPost, "/v1/plans", body, remoteTimeout, nil)
		if err != nil || status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
			return status, resp, err
		}
		pause := backoff*3/4 + rand.N(backoff/2+1)
		if s, err := strconv.Atoi(hdr.Get("Retry-After")); err == nil && s > 0 {
			pause = max(pause, time.Duration(s)*time.Second)
		}
		if time.Now().Add(pause).After(deadline) {
			gaveUp(status)
			return status, resp, nil
		}
		fmt.Fprintf(a.opts.Stderr, "writ: writ serve refused the plan with %d, as its plan slots are busy or the forge limits calls; asking again in %s\n", status, pause.Round(time.Millisecond))
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
		backoff = min(backoff*2, slotBackoffMax, max(wait/2, backoff))
		if r.refresh != nil {
			fctx, cancel := context.WithDeadline(ctx, deadline)
			tok, err := r.refresh(fctx)
			cancel()
			if err != nil && !time.Now().Before(deadline) {
				gaveUp(status)
				return status, resp, nil
			}
			if err != nil {
				return 0, nil, err
			}
			r.token = tok
		}
		if !time.Now().Before(deadline) {
			gaveUp(status)
			return status, resp, nil
		}
	}
}
