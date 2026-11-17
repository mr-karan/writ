//go:build failpoint

package sqldb

import (
	"os"
	"time"
)

// DefaultTimeout returns d unless the e2e binary has a fixture budget.
func DefaultTimeout(d time.Duration) time.Duration {
	raw := os.Getenv("WRIT_TEST_DATABASE_TIMEOUT")
	if raw == "" {
		return d
	}
	budget, err := time.ParseDuration(raw)
	if err != nil || budget <= 0 {
		panic("invalid WRIT_TEST_DATABASE_TIMEOUT")
	}
	return budget
}
