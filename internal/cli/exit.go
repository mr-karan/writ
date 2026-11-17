package cli

import (
	"errors"
	"fmt"
)

// UsageError means the command line was wrong: an unknown flag value, a
// malformed address, a missing argument. Exit 2.
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string { return e.Msg }

func usagef(format string, args ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

// Exit codes.
const (
	ExitOK     = 0
	ExitFailed = 1
	ExitUsage  = 2
)

// ExitCode maps a command's error to the process exit code: 0 for success,
// 2 for a usage error, and 1 for everything else, whether the command
// reported its own failure (ErrFailed) or could not run.
func ExitCode(err error) int {
	var u *UsageError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &u):
		return ExitUsage
	}
	return ExitFailed
}
