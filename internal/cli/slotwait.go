package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// Bounds of waiting for a plan slot.
const (
	maxSlotWait    = time.Hour
	slotBackoff    = 5 * time.Second
	slotBackoffMax = time.Minute
)

// postPlan sends a plan request. With a slot wait, which ends at
// deadline, a refusal writ serve marks as made before it took the plan,
// with X-Writ-Retry, as when every plan slot is busy or the forge limits
// the rate of calls, is asked again until the wait ends: after the
// answer's Retry-After when it says one, else with backoff and jitter. A
// job of overlapping workflow runs then waits rather than failing. A 429
// or 503 without the mark may come from a proxy after writ serve took
// the plan, so it is not asked again. A refused plan spends no token. A
// token from GitHub Actions is asked for again before each try, within
// the wait, so it does not expire meanwhile.
//
// No try starts after the wait ends. A plan request that started before
// it runs to its answer under the request's own timeout: cutting it could
// leave writ serve planning with an answer no one reads, and the token
// spent.
func (a *App) postPlan(ctx context.Context, r *remote, body []byte, wait time.Duration, deadline time.Time) (int, []byte, error) {
	if wait == 0 {
		return r.call(ctx, http.MethodPost, "/v1/plans", body, remoteTimeout)
	}
	gaveUp := func(status int) {
		fmt.Fprintf(a.opts.Stderr, "writ: writ serve still refused the plan with %d after waiting %s for a plan slot\n", status, wait)
	}
	backoff := min(slotBackoff, max(wait/16, 10*time.Millisecond))
	for {
		if !time.Now().Before(deadline) {
			return 0, nil, fmt.Errorf("the slot wait of %s ended before a plan request could be sent", wait)
		}
		status, hdr, resp, err := r.send(ctx, http.MethodPost, "/v1/plans", body, remoteTimeout, nil)
		if err != nil || !marked(status, hdr.Get(server.RetryHeader)) {
			return status, resp, err
		}
		now := time.Now()
		pause := backoff*3/4 + rand.N(backoff/2+1)
		ra, past := retryAfter(hdr.Get("Retry-After"), now, deadline.Sub(now))
		pause = max(pause, ra)
		if past || now.Add(pause).After(deadline) {
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

// marked reports whether a refusal carries the mark writ serve gives its
// own refusals made before it took the plan: plan-slots on a 429,
// forge-rate-limit on a 503. Any other pair, as a proxy could send, is not
// asked again.
func marked(status int, mark string) bool {
	return status == http.StatusTooManyRequests && mark == server.RetrySlots ||
		status == http.StatusServiceUnavailable && mark == server.RetryForge
}

// retryAfter reads a Retry-After value, seconds or an HTTP-date, as a
// wait from now, and reports past when the wait is longer than left. A
// count of seconds is compared with left before it becomes a duration, so
// a huge one cannot overflow; a value it cannot read is no wait.
func retryAfter(v string, now time.Time, left time.Duration) (wait time.Duration, past bool) {
	if v == "" {
		return 0, false
	}
	s, err := strconv.ParseUint(v, 10, 64)
	if err == nil {
		if s > uint64(left/time.Second) {
			return 0, true
		}
		return time.Duration(s) * time.Second, false
	}
	if errors.Is(err, strconv.ErrRange) && strings.Trim(v, "0123456789") == "" {
		return 0, true
	}
	at, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	wait = max(at.Sub(now), 0)
	return wait, wait > left
}
