//go:build !failpoint

package sqldb

import "time"

// DefaultTimeout returns d. Failpoint builds allow an e2e fixture override.
func DefaultTimeout(d time.Duration) time.Duration { return d }
