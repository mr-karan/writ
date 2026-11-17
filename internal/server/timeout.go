package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// timeoutError is why a plan's or an apply's context ended: a limit of the
// config ran out.
type timeoutError struct {
	what  string
	limit string
	after time.Duration
}

func (e *timeoutError) Error() string {
	return fmt.Sprintf("%s ran past %s, %s", e.what, e.limit, shortDuration(e.after))
}

// withLimit is ctx with a deadline after d, whose cause names limit.
func (s *Server) withLimit(ctx context.Context, what, limit string, d time.Duration) (context.Context, context.CancelFunc) {
	timeout := s.withTimeout
	if timeout == nil {
		timeout = context.WithTimeoutCause
	}
	return timeout(ctx, d, &timeoutError{what: what, limit: limit, after: d})
}

// timedOut is the limit that ended ctx, nil when none did.
func timedOut(ctx context.Context) *timeoutError {
	te, _ := errors.AsType[*timeoutError](context.Cause(ctx))
	return te
}

// shortDuration writes d as 30m or 4h, not 30m0s or 4h0m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
