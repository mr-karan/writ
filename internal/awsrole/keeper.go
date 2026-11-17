package awsrole

import (
	"context"
	"sync"
	"time"
)

// Keeper holds the credentials of one session, such as the one the change
// watcher reads its queue with or one run's. It assumes the role again
// when the credentials have less than RefreshBefore left. When a refresh
// fails it keeps serving the old credentials while they have a minute
// left.
type Keeper struct {
	assume        func(context.Context) (Credentials, error)
	refreshBefore time.Duration
	now           func() time.Time

	mu    sync.Mutex
	creds Credentials
}

// Get returns credentials with more than RefreshBefore left.
func (k *Keeper) Get(ctx context.Context) (Credentials, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if k.creds.Expires.Sub(now) > k.refreshBefore {
		return k.creds, nil
	}
	c, err := k.assume(ctx)
	if err == nil {
		k.creds = c
		return c, nil
	}
	if k.creds.Expires.Sub(now) > minLeft {
		return k.creds, nil
	}
	return Credentials{}, err
}
