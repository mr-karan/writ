package server

import (
	"sync"
	"time"
)

// maxBuckets bounds the keys a limiter remembers.
const maxBuckets = 10000

// limiter is a token bucket for each key, a principal or a client address:
// rate requests a second, with bursts of rate. It holds at most max
// buckets.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	max     int
	now     func() time.Time
	buckets map[string]*bucket
	// swept is when full buckets were last dropped.
	swept time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(rate, max int, now func() time.Time) *limiter {
	return &limiter{rate: float64(rate), max: max, now: now, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.max {
			l.evict(now)
		}
		b = &bucket{tokens: l.rate, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.rate, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evict makes room for one bucket. A bucket idle for a second is full
// again, the same as no bucket, so those go first, in a sweep at most once
// a second. When every bucket was used in the last second, one bucket
// goes, whichever the map gives first: that key starts again with a full
// burst.
func (l *limiter) evict(now time.Time) {
	if now.Sub(l.swept) >= time.Second {
		l.swept = now
		for k, b := range l.buckets {
			if now.Sub(b.at) >= time.Second {
				delete(l.buckets, k)
			}
		}
	}
	for k := range l.buckets {
		if len(l.buckets) < l.max {
			return
		}
		delete(l.buckets, k)
	}
}

func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
