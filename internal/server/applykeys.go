package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
)

// ApplyKeyHeader carries the key of an apply request.
const ApplyKeyHeader = "Idempotency-Key"

// keySkew is how far past writ serve's clock the time of a key may be.
const keySkew = 5 * time.Minute

// The server's settle of a key after the commit of a run's start
// returned an error: each attempt has settleTimeout, attempts are
// settleEvery apart, and the last starts before settleFor ends.
const (
	settleTimeout = 5 * time.Second
	settleEvery   = 5 * time.Second
	settleFor     = 2 * time.Minute
)

// sweepEvery is how often writ serve fails the runs left applying with no
// process to do their work, and ends the state access of runs that no
// process runs.
const sweepEvery = time.Minute

var applyKeyRE = regexp.MustCompile(`^([0-9]{1,12})-[A-Z2-7]{26}$`)

// NewApplyKey makes the key of one writ apply --remote: the Unix time in
// seconds, a dash, and 130 random bits in base32.
func NewApplyKey(now time.Time) string {
	return strconv.FormatInt(now.Unix(), 10) + "-" + rand.Text()
}

// ParseApplyKey returns the time a key carries.
func ParseApplyKey(key string) (time.Time, error) {
	m := applyKeyRE.FindStringSubmatch(key)
	if m == nil {
		return time.Time{}, fmt.Errorf("%q is not an apply request's key: it is the Unix time, a dash, and 26 letters A to Z and digits 2 to 7", trunc(key))
	}
	sec, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}

// requestKey reads the key of POST /v1/runs from ApplyKeyHeader, or makes
// one for a client that sends none, so the server can settle every
// request. It refuses a malformed key, one past control.KeyLife, and one
// whose time is past the server's clock by more than keySkew.
func (s *Server) requestKey(w http.ResponseWriter, r *http.Request, body RunRequest) (control.RequestKey, bool) {
	b, _ := json.Marshal(body)
	sum := sha256.Sum256(b)
	k := control.RequestKey{Key: r.Header.Get(ApplyKeyHeader), Request: hex.EncodeToString(sum[:])}
	now := s.now()
	if k.Key == "" {
		k.Key = NewApplyKey(now)
	}
	issued, err := ParseApplyKey(k.Key)
	switch {
	case err != nil:
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "%s: %v", ApplyKeyHeader, err)
		return k, false
	case issued.After(now.Add(keySkew)):
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "the key %s was made at %s, after writ serve's clock, %s: check the client's clock",
			k.Key, issued.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		return k, false
	case !now.Before(issued.Add(control.KeyLife)):
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "the key %s was made at %s, and a key starts a run for %s: run the command again",
			k.Key, issued.UTC().Format(time.RFC3339), control.KeyLife)
		return k, false
	}
	k.IssuedAt = issued
	return k, true
}

// answerKey answers a request whose key has a row: the same principal's
// repeat of the same request gets the run with 202, as the first answer
// did. It starts no second run and spends no second token.
func (s *Server) answerKey(w http.ResponseWriter, r *http.Request, row control.KeyRow, k control.RequestKey) {
	req := reqOf(r)
	pr := req.principal
	switch {
	case row.Outcome == control.KeyNone:
		req.keyClosed = true
		s.refuse(w, r, FormatRemoteRun, diag.State, http.StatusConflict, "the key %s is closed with no run: no run started; run the command again", k.Key)
		return
	case row.Issuer != pr.info.Issuer || row.Subject != pr.info.Subject:
		s.refuse(w, r, FormatRemoteRun, diag.Denied, http.StatusForbidden, "the key %s started a run of another principal", k.Key)
		return
	case row.Request != k.Request:
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "the key %s started a run of another request: each command sends a key of its own", k.Key)
		return
	}
	run, err := s.store.GetRun(r.Context(), row.RunID)
	if err != nil {
		s.dbFailed(w, r, FormatRemoteRun, err)
		return
	}
	e := &req.entry
	e.Run, e.Plan, e.Env, e.Stacks, e.Decision, e.Reason = run.ID, run.PlanID, run.Env, []string{run.Stack}, control.Allowed, "a repeat of the key "+k.Key
	w.Header().Set("Location", "/v1/runs/"+run.ID)
	s.reply(w, http.StatusAccepted, s.remoteRun(run))
}

// applyOutcome is POST /v1/apply-requests/{key}/outcome. It settles the
// key: a key with no row gets a none row, which no later request can
// change. It answers started with the run, only to the run's principal and
// to an admin, or none to any principal.
func (s *Server) applyOutcome(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	pr := req.principal
	key := r.PathValue("key")
	issued, err := ParseApplyKey(key)
	if err != nil {
		s.refuse(w, r, FormatOutcome, diag.Usage, http.StatusBadRequest, "%v", err)
		return
	}
	if issued.After(s.now().Add(keySkew)) {
		s.refuse(w, r, FormatOutcome, diag.Usage, http.StatusBadRequest, "the key %s was made after writ serve's clock: check the client's clock", key)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), settleTimeout)
	row, err := s.closeKey(ctx, key, issued, s.now())
	cancel()
	if err != nil {
		s.dbFailed(w, r, FormatOutcome, err)
		return
	}
	doc := ApplyOutcome{Format: FormatOutcome, OK: true, Key: key, Outcome: string(row.Outcome), Diagnostics: []Diagnostic{}}
	e := &req.entry
	e.Reason = "outcome " + string(row.Outcome)
	if row.Outcome == control.KeyStarted {
		e.Run = row.RunID
		if (row.Issuer != pr.info.Issuer || row.Subject != pr.info.Subject) && len(s.adminGrants(pr)) == 0 {
			s.refuse(w, r, FormatOutcome, diag.Denied, http.StatusForbidden, "the key %s started a run of another principal; only that principal or an admin reads it", key)
			return
		}
		run, err := s.store.GetRun(r.Context(), row.RunID)
		if err != nil {
			s.dbFailed(w, r, FormatOutcome, err)
			return
		}
		e.Plan, e.Env, e.Stacks = run.PlanID, run.Env, []string{run.Stack}
		doc.Run = s.remoteRun(run).Run
	}
	e.Decision = control.Allowed
	s.reply(w, http.StatusOK, doc)
}

// settleStart handles a run's start whose commit returned an error. It
// settles the key: started goes on as the run, and returns true. none
// answers 503 with outcome none and key_closed. When the settle fails, it answers 503
// with outcome unknown, keeps the apply slot, and settles again in the
// background.
func (s *Server) settleStart(w http.ResponseWriter, r *http.Request, k control.RequestKey, run control.Run, work func()) bool {
	req := reqOf(r)
	row, err := s.settleOnce(k)
	switch {
	case err == nil && row.Outcome == control.KeyStarted && row.RunID == run.ID:
		s.log.Warn("the commit of a run's start returned an error, and the run began", "request", req.id, "run", run.ID, "key", k.Key)
		return true
	case err == nil:
		s.releaseApply()
		s.unpend(run.ID)
		if row.Outcome == control.KeyStarted {
			s.answerKey(w, r, row, k)
			return false
		}
		s.log.Error("the commit of a run's start failed, and no run began", "request", req.id, "run", run.ID, "key", k.Key)
		req.keyClosed = true
		s.refuse(w, r, FormatRemoteRun, diag.StateIO, http.StatusServiceUnavailable, "the control database failed as run %s began, and no run started: the key %s is closed; run the command again; the request id is %s",
			run.ID, k.Key, req.id)
		return false
	}
	s.log.Error("the commit of a run's start returned an error, and its key could not be settled", "request", req.id, "run", run.ID, "key", k.Key, "err", err)
	req.outcome = OutcomeUnknown
	s.wg.Go(func() { s.settleLater(k, run, work) })
	s.refuse(w, r, FormatRemoteRun, diag.StateIO, http.StatusServiceUnavailable, "the control database failed as run %s began, and writ serve cannot tell yet whether it started; ask with: writ show --remote --request %s",
		run.ID, k.Key)
	return false
}

// settleOnce settles a key under the server's context, for settleTimeout.
func (s *Server) settleOnce(k control.RequestKey) (control.KeyRow, error) {
	ctx, cancel := context.WithTimeout(s.ctx, settleTimeout)
	defer cancel()
	return s.closeKey(ctx, k.Key, k.IssuedAt, s.now())
}

// settleLater settles a key every settleEvery until it answers, settleFor
// passes, or the server stops. started does the run's work. Any other
// end gives the apply slot back. A run that began and was given up is
// failed by the next sweep of this process, or by the next process.
func (s *Server) settleLater(k control.RequestKey, run control.Run, work func()) {
	end := time.NewTimer(s.settleFor)
	defer end.Stop()
	tick := time.NewTicker(s.settleEvery)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			s.abandon(run.ID, k.Key, "writ serve is stopping")
			return
		case <-end.C:
			s.abandon(run.ID, k.Key, fmt.Sprintf("the key was not settled in %s", s.settleFor))
			return
		case <-tick.C:
		}
		row, err := s.settleOnce(k)
		switch {
		case err != nil:
			s.log.Warn("settle the key of a run's start", "run", run.ID, "key", k.Key, "err", err)
		case row.Outcome == control.KeyStarted && row.RunID == run.ID:
			s.log.Warn("settled the key of a run's start: the run began", "run", run.ID, "key", k.Key)
			work()
			return
		default:
			s.log.Warn("settled the key of a run's start: no run began", "run", run.ID, "key", k.Key, "outcome", row.Outcome)
			s.releaseApply()
			s.unpend(run.ID)
			return
		}
	}
}

// abandon gives back the apply slot of a run whose start was never
// settled.
func (s *Server) abandon(run, key, why string) {
	s.log.Error("gave up settling the key of a run's start; a run that began is failed by the next sweep", "run", run, "key", key, "why", why)
	s.releaseApply()
	s.unpend(run)
}

// pend marks a run whose start is in progress, so a sweep leaves it.
func (s *Server) pend(run string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[run] = struct{}{}
}

func (s *Server) unpend(run string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, run)
}

// handled reports whether this process has work for a run: its start is
// in progress, or it applies.
func (s *Server) handled(run string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, starting := s.pending[run]
	_, applying := s.runDone[run]
	return starting || applying
}

// sweepRuns fails the runs left applying with no process to do their
// work, every sweepEvery: a run that an earlier process's transaction
// committed after this one started, and a run of this process whose start
// or end it could not record. Then it ends the state tokens, and
// releases the locks, of runs that no process runs, which no expiry
// would release.
func (s *Server) sweepRuns(ctx context.Context) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.sweepOnce(ctx)
	}
}

func (s *Server) sweepOnce(ctx context.Context) {
	runs, err := s.store.FailApplying(ctx, s.now(), s.boot, s.handled)
	switch {
	case err != nil && !errors.Is(ctx.Err(), context.Canceled):
		s.log.Error("fail the runs left applying", "err", err)
	case len(runs) > 0:
		s.log.Warn("runs left applying with no process to do their work are failed; the next plan reads what they left in doubt", "runs", runs)
	}
	unlocked, err := s.store.EndOrphanState(ctx, s.now())
	switch {
	case err != nil && !errors.Is(ctx.Err(), context.Canceled):
		s.log.Error("end the state access of runs that no process runs", "err", err)
	case len(unlocked) > 0:
		s.log.Warn("state locks of runs that no process runs are released", "stacks", unlocked)
	}
}
