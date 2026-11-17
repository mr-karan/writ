// Package runas acts as an OS user with no name: it starts a process as
// that user, does file work as that user, and checks that a started
// process holds nothing of writ's. It imports only the standard library,
// so internal/provider and internal/tofu share it and reach nothing else.
package runas

import (
	"errors"
	"fmt"
)

// User is an OS user and group with no name, as writ serve gives each run.
type User struct {
	UID uint32
	GID uint32
}

func (u User) String() string { return fmt.Sprintf("uid %d gid %d", u.UID, u.GID) }

// ErrUnsupported is returned where a process cannot act as another user.
var ErrUnsupported = errors.New("acting as another user needs Linux")
