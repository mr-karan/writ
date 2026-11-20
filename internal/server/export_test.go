package server

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/repo"
	"github.com/mr-karan/writ/internal/tofu"
)

// MaxLogins is maxLogins, for tests of the bound.
const MaxLogins = maxLogins

// MaxLoginsPerSource is maxLoginsPerSource, for tests of the bound.
const MaxLoginsPerSource = maxLoginsPerSource

// FillLogins takes n places under maxLogins with sign-ins that wait an
// hour and that no handle names.
func (s *Server) FillLogins(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range n {
		var key [32]byte
		binary.BigEndian.PutUint64(key[:], uint64(i))
		_, cancel := context.WithCancel(s.ctx)
		s.logins[key] = &login{expires: s.now().Add(time.Hour), cancel: cancel}
	}
}

// Logins counts the sign-ins of both flows that wait, those still
// starting included.
func (s *Server) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.logins) + len(s.browserLogins)
}

// HoldCommit wraps the commit of each run's start with step.
func (s *Server) HoldCommit(step func(commit func() error) error) { s.applyCommit = step }

// CloseKeyWith settles keys with f in place of the store, and paces the
// settle after a failed commit.
func (s *Server) CloseKeyWith(f func(ctx context.Context, key string, issued, now time.Time) (control.KeyRow, error), every, total time.Duration) {
	s.closeKey, s.settleEvery, s.settleFor = f, every, total
}

// Sweep fails the runs left applying with no process to do their work,
// and ends the state access of runs that no process runs, as the sweep
// each minute does.
func (s *Server) Sweep() { s.sweepOnce(context.Background()) }

// Applies counts the apply slots held.
func (s *Server) Applies() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applies
}

// Running counts the running plans and applies.
func (s *Server) Running() (plans, applies int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total, s.applies
}

// Poll reads the head of each repository's default branch, as the poll
// does.
func (s *Server) Poll() { s.pollOnce(context.Background()) }

// Shed counts the requests refused over the address limit.
func (s *Server) Shed() int64 { return s.shed.count() }

// RefreshIndexes refreshes the state index of each stack whose state is
// newer than its index, as the look every few minutes does.
func (s *Server) RefreshIndexes() { s.refreshStale(context.Background()) }

// FillIndex indexes state as version version of stack@env's state, as an
// apply's fill does, through a mask that knows no secret.
func (s *Server) FillIndex(env, stack string, version int64, state []byte) error {
	_, err := s.fillIndex(context.Background(), env, stack, control.BackendHTTP, tofu.Serial{Serial: version}, 0, mask.New(nil), tofu.State{Unmasked: state})
	return err
}

// ObserveS3 records read as a read of stack@env's state on S3 by a run
// that started at since, as a plan or a refresh records it, with now as
// the clock the record takes its time from. It returns the read's place.
func (s *Server) ObserveS3(env, stack string, read tofu.Serial, since time.Time, now func() time.Time) (int64, error) {
	o := &Server{cfg: s.cfg, store: s.store, log: s.log, now: now, notifier: s.notifier}
	_, seq, err := o.observeSerial(context.Background(), env, stack, &repo.Backend{}, read, since)
	return seq, err
}

// FillS3Index indexes state as read's serial of stack@env's state on S3,
// for the read numbered seq, as a refresh's fill does, through a mask
// that knows no secret. It reports whether the index took it.
func (s *Server) FillS3Index(env, stack string, read tofu.Serial, seq int64, state []byte) (bool, error) {
	return s.fillIndex(context.Background(), env, stack, control.BackendS3, read, seq, mask.New(nil), tofu.State{Unmasked: state})
}

// StartDriftLook runs one look of the drift checks with every as
// drift.every, which starts the checks that are due and returns.
func (s *Server) StartDriftLook(every time.Duration) {
	s.cfg.DriftEvery = every
	s.driftLook(s.ctx)
}

// WaitDrift waits until no drift check runs.
func (s *Server) WaitDrift() {
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		n := s.running[driftPrincipal.key()]
		s.mu.Unlock()
		if n == 0 {
			return
		}
	}
	panic("a drift check ran for a minute")
}

// LookForDrift runs one look of the drift checks with every as
// drift.every, and waits for the checks it started.
func (s *Server) LookForDrift(every time.Duration) {
	s.StartDriftLook(every)
	s.WaitDrift()
}

// Routes is each API route's pattern with the methods it takes.
func (s *Server) Routes() map[string][]string {
	out := map[string][]string{}
	for _, rt := range s.routeTable() {
		out[rt.pattern] = sortedKeys(rt.methods)
	}
	return out
}
