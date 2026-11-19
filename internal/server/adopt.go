package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/repo"
	"github.com/mr-karan/writ/internal/tfstate"
	"github.com/mr-karan/writ/internal/tofu"
)

const (
	// FormatStateAdoptRequest is the body of POST /v1/state/adopt.
	FormatStateAdoptRequest = "writ.state-adopt-request/v0"
	// FormatStateAdopt is the answer of POST /v1/state/adopt.
	FormatStateAdopt = "writ.state-adopt/v0"
)

// AdoptLimit is what an adoption cannot learn, which its answer and
// writ state adopt state.
const AdoptLimit = "the secret set holds each value the state marks sensitive and the environment's provider_env values; " +
	"a secret the state holds where nothing marks it, as an address keyed by nonsensitive() of a variable before writ serve planned the stack, is not in it, " +
	"and later plans and drift checks may show it"

// StateAdoptRequest is writ.state-adopt-request/v0: the state of one
// stack@env to adopt, named by its state version on the http backend or
// its serial on S3, and why.
type StateAdoptRequest struct {
	Format  string `json:"format"`
	Env     string `json:"env"`
	Stack   string `json:"stack"`
	Version int64  `json:"version"`
	Serial  int64  `json:"serial"`
	Reason  string `json:"reason"`
}

// StateAdoptDoc is writ.state-adopt/v0: the state an adoption sealed a
// secret set with, the code whose providers read it, and how many rows
// of the state index it read.
type StateAdoptDoc struct {
	Format  string `json:"format"`
	OK      bool   `json:"ok"`
	Stack   string `json:"stack"`
	Env     string `json:"env"`
	Backend string `json:"backend"`
	// Version is the state version on the http backend, 0 on S3.
	Version int64  `json:"version"`
	Serial  int64  `json:"serial"`
	Lineage string `json:"lineage"`
	Repo    string `json:"repo"`
	Commit  string `json:"commit"`
	// Resources counts the rows of the state index the adoption read,
	// and Indexed reports that the index took them.
	Resources   int          `json:"resources"`
	Indexed     bool         `json:"indexed"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// adoptState is POST /v1/state/adopt: a person with adopt on a stack@env
// seals a secret set with its newest state, which no apply of writ serve
// sealed one with. A run reads the state with tofu show -json, and the
// mask learns every value the state marks sensitive. The set, the audit
// row, and the rows of the state index are all that is kept.
func (s *Server) adoptState(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	pr := req.principal
	var body StateAdoptRequest
	if err := s.readBody(r, &body); err != nil {
		status := http.StatusBadRequest
		if _, ok := err.(errTooLarge); ok {
			status = http.StatusRequestEntityTooLarge
		}
		s.refuse(w, r, FormatStateAdopt, diag.Usage, status, "%v", err)
		return
	}
	if body.Format != FormatStateAdoptRequest {
		s.refuse(w, r, FormatStateAdopt, diag.Usage, http.StatusBadRequest, "the body's format is %q, not %s", trunc(body.Format), FormatStateAdoptRequest)
		return
	}
	e, ok := s.target(w, r, FormatStateAdopt, body.Env, body.Stack)
	if !ok {
		return
	}
	env, stack := e.Name, body.Stack
	backend, named := control.BackendHTTP, body.Version
	if body.Serial > 0 {
		backend, named = control.BackendS3, body.Serial
	}
	switch {
	case body.Version < 0 || body.Serial < 0 || (body.Version > 0) == (body.Serial > 0):
		s.refuse(w, r, FormatStateAdopt, diag.Usage, http.StatusBadRequest, "an adoption names one state: version, a state version of a stack on the http backend, or serial, a serial of a stack's state on S3")
		return
	case len(body.Reason) < control.MinOverride || len(body.Reason) > 500:
		s.refuse(w, r, FormatStateAdopt, diag.Usage, http.StatusBadRequest, "an adoption's reason needs %d to 500 characters, such as the ticket and where the state came from", control.MinOverride)
		return
	case pr.tok != nil:
		s.refuse(w, r, FormatStateAdopt, diag.Denied, http.StatusForbidden, "an adoption is a person's: a CI job cannot adopt a state")
		return
	}
	if !s.decide(w, r, FormatStateAdopt, authz.Adopt, stack, env) {
		return
	}
	if s.tofuRunner == nil {
		s.refuse(w, r, FormatStateAdopt, diag.Usage, http.StatusConflict, "this writ serve runs no OpenTofu, so it cannot read a state to adopt: its config needs a tofu { } block")
		return
	}
	want, err := s.store.NewestStateOf(r.Context(), env, stack, backend)
	switch {
	case errors.Is(err, control.ErrNotFound) && backend == control.BackendHTTP:
		s.refuse(w, r, FormatStateAdopt, diag.State, http.StatusNotFound, "writ serve keeps no state version of %s@%s on the http backend; a stack on S3 names its state with serial", stack, env)
		return
	case errors.Is(err, control.ErrNotFound):
		s.refuse(w, r, FormatStateAdopt, diag.State, http.StatusNotFound, "writ serve has read no serial of the state of %s@%s on S3: plan the stack first; a stack on the http backend names its state with version", stack, env)
		return
	case err != nil:
		s.dbFailed(w, r, FormatStateAdopt, err)
		return
	case backend == control.BackendHTTP && want.Version != named:
		s.refuse(w, r, FormatStateAdopt, diag.State, http.StatusConflict, "state version %d of %s@%s is not its newest, version %d: adopt the newest", named, stack, env, want.Version)
		return
	case backend == control.BackendS3 && want.Serial != named:
		s.refuse(w, r, FormatStateAdopt, diag.State, http.StatusConflict, "serial %d of the state of %s@%s is not the newest that writ serve read, serial %d: adopt the newest", named, stack, env, want.Serial)
		return
	case want.HasSet:
		s.refuse(w, r, FormatStateAdopt, diag.State, http.StatusConflict, "state version %d of %s@%s has a secret set already, so there is nothing to adopt", named, stack, env)
		return
	case want.Applying != "":
		s.refuse(w, r, FormatStateAdopt, diag.State, http.StatusConflict, "an apply of %s@%s runs, run %s, which may write a newer state: adopt after it ends", stack, env, want.Applying)
		return
	}
	if !s.claim(pr.key()) {
		s.refuse(w, r, FormatStateAdopt, diag.Usage, http.StatusTooManyRequests,
			"writ serve is running %d plans for %s or %d in all; try again when one ends", s.cfg.Limits.PlansPerPrincipal, pr.info.Display, s.cfg.Limits.Plans)
		return
	}
	defer s.release(pr.key())
	// The answer must fit the server's write timeout.
	ctx, cancel := context.WithTimeout(r.Context(), MaxWait)
	defer cancel()
	got, err := s.adoptRun(ctx, adoption{env: env, stack: stack, backend: backend, want: want, p: pr})
	if err != nil {
		class := diag.Usage
		if ce, ok := errors.AsType[*classedError](err); ok {
			class = ce.class
		}
		if ctx.Err() != nil && r.Context().Err() == nil {
			err = fmt.Errorf("the adoption ran past %s, so writ serve kept nothing of it: run it again: %w", shortDuration(MaxWait), err)
		}
		s.refuse(w, r, FormatStateAdopt, class, http.StatusConflict, "%v", err)
		return
	}
	en := &req.entry
	en.Kind, en.Status, en.At = "state-adopt", http.StatusOK, req.start
	if backend == control.BackendHTTP {
		en.Reason = fmt.Sprintf("adopted state version %d of %s@%s, serial %d of lineage %s: %s", want.Version, stack, env, want.Serial, want.Lineage, body.Reason)
	} else {
		en.Reason = fmt.Sprintf("adopted serial %d of lineage %s of the state of %s@%s on S3: %s", want.Serial, want.Lineage, stack, env, body.Reason)
	}
	en.Elapsed = s.now().Sub(req.start)
	sctx, scancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer scancel()
	indexed, err := s.store.AdoptState(sctx, control.StateAdopt{Env: env, Stack: stack, Backend: backend, Version: want.Version, Lineage: want.Lineage, Serial: want.Serial,
		Sealed: got.sealed, Rows: got.rows, Seq: got.seq, At: s.now(), Audit: *en})
	switch {
	case errors.Is(err, control.ErrNotNewest):
		s.refuse(w, r, FormatStateAdopt, diag.State, http.StatusConflict, "the state of %s@%s moved while the adoption read it, so writ serve kept nothing: adopt the newest", stack, env)
		return
	case errors.Is(err, control.ErrHasSet):
		s.refuse(w, r, FormatStateAdopt, diag.State, http.StatusConflict, "a secret set was sealed with state version %d of %s@%s while the adoption read it, so writ serve kept nothing", named, stack, env)
		return
	case errors.Is(err, control.ErrApplyRuns):
		s.refuse(w, r, FormatStateAdopt, diag.State, http.StatusConflict, "an apply of %s@%s runs, which may write a newer state: adopt after it ends", stack, env)
		return
	case err != nil:
		s.dbFailed(w, r, FormatStateAdopt, err)
		return
	}
	req.audited = true
	doc := StateAdoptDoc{Format: FormatStateAdopt, OK: true, Stack: stack, Env: env, Backend: backend, Version: want.Version, Serial: want.Serial, Lineage: want.Lineage,
		Repo: got.repo, Commit: got.commit, Resources: len(got.rows), Indexed: indexed, Diagnostics: []Diagnostic{{"warning", string(diag.State), AdoptLimit}}}
	s.log.Warn("state adopted", "request", req.id, "stack", stack, "env", env, "backend", backend, "version", want.Version, "serial", want.Serial,
		"lineage", want.Lineage, "person", pr.info.Person, "reason", body.Reason)
	s.reply(w, http.StatusOK, doc)
}

// adoption is the state of a stack@env that a person adopts.
type adoption struct {
	env, stack, backend string
	want                control.NewestState
	p                   *principal
}

// adopted is what an adoption's run read: the sealed secret set, the
// rows of the state index, and on S3 the place of the read.
type adopted struct {
	sealed       []byte
	rows         []control.IndexRow
	seq          int64
	repo, commit string
}

// adoptRun reads the state a names in a run, with the files of the stack
// at the head of its repository's default branch, as a drift check
// plans them: on the http backend, show -json of the stored version as a
// file, with no credential and no state token; on S3, state pull with
// the plan role and show -json of what it pulled with none. Both run as
// a provider uid in the filesystem view. The mask, seeded with the
// environment's secrets, reads the state into rows of the index and
// learns every value it marks sensitive, and that set is sealed with
// the state. Nothing else of the run is kept.
func (s *Server) adoptRun(ctx context.Context, a adoption) (adopted, error) {
	rp, dir, err := s.driftRepo(ctx, a.env, a.stack)
	if err != nil {
		return adopted{}, err
	}
	client, err := s.clientOf(rp)
	if err != nil {
		return adopted{}, err
	}
	snap, err := s.commitSnapshot(ctx, rp, client, rp.Head)
	if err != nil {
		return adopted{}, err
	}
	ts, err := repo.TofuSources(ctx, client, rp.ProjectID, snap, dir, s.store, repo.DefaultLimits)
	if err != nil {
		return adopted{}, &classedError{diag.Usage, err}
	}
	s.keepBlobs(rp.Name, snap)
	kind, err := s.stateBackend(a.env, a.stack, ts)
	if err != nil {
		return adopted{}, err
	}
	if kind != a.backend {
		return adopted{}, tofuRefused("stack %s keeps its state on backend %s at the head of %s, not on %s", a.stack, kind, rp.Name, a.backend)
	}
	key, err := s.stateKey(a.env)
	if err != nil {
		return adopted{}, fmt.Errorf("the state key of env %s: %w", a.env, err)
	}
	id := "adopt-" + newID()
	work := filepath.Join(s.cfg.Data, "work", id)
	defer os.RemoveAll(work)
	if err := writeTofuFiles(work, ts.Files, rp.Head); err != nil {
		return adopted{}, err
	}
	out := adopted{repo: rp.Name, commit: rp.Head}
	var state tofu.State
	var m *mask.Mask
	if a.backend == control.BackendS3 {
		if state, m, out.seq, err = s.adoptPull(ctx, a, id, ts, dir, work); err != nil {
			return adopted{}, err
		}
	} else if state, m, err = s.adoptShow(ctx, a, key, ts, dir, work); err != nil {
		return adopted{}, err
	}
	if out.rows, err = maskedRows(m, state); err != nil {
		return adopted{}, fmt.Errorf("the mask refused the state of %s@%s, so writ serve kept nothing: %w", a.stack, a.env, err)
	}
	version := a.want.Version
	if a.backend == control.BackendS3 {
		version = a.want.Serial
	}
	out.sealed = key.SealIndexSecrets(m.Secrets(), a.env, a.stack, version)
	return out, nil
}

// adoptShow runs show -json of the newest stored version of a's stack on
// the http backend, which must still be the version a names, and returns
// it with a mask that knows the environment's secrets.
func (s *Server) adoptShow(ctx context.Context, a adoption, key *tfstate.Key, ts repo.TofuStack, dir, work string) (tofu.State, *mask.Mask, error) {
	v, err := s.store.LatestState(ctx, a.env, a.stack)
	if err != nil {
		return tofu.State{}, nil, fmt.Errorf("read the state version of %s@%s: %w", a.stack, a.env, err)
	}
	if v.Version != a.want.Version {
		return tofu.State{}, nil, &classedError{diag.State, fmt.Errorf("the state of %s@%s moved to version %d, so writ serve kept nothing: adopt the newest", a.stack, a.env, v.Version)}
	}
	raw, err := key.OpenState(v.Sealed, a.env, a.stack, v.Version)
	if err != nil {
		return tofu.State{}, nil, err
	}
	user, done, err := s.takeUID(ctx)
	if err != nil {
		return tofu.State{}, nil, err
	}
	defer done()
	if user == nil && !s.cfg.Dev {
		return tofu.State{}, nil, tofuRefused("this writ serve has no provider_uids to run OpenTofu as, so it would run as writ's own user: set provider_uids")
	}
	// tofu gets none of the environment's secrets, and the mask learns
	// them, as a refresh of the index does.
	var pe []string
	if !s.cfg.Dev {
		if pe, err = readProviderEnv(s.cfg.Envs[a.env]); err != nil {
			return tofu.State{}, nil, err
		}
	}
	_, _, secrets := s.tofuEnv(pe)
	m := mask.New(secrets)
	lock, err := s.tofuRunner.Fill(ctx, ts.Lock)
	if err != nil {
		return tofu.State{}, nil, fmt.Errorf("install the providers of %s into the plugin cache: %w%s", tofu.LockFile, err, stepLines(m, err))
	}
	stk := tofu.Stack{Files: os.DirFS(work), Dir: dir, Lock: lock, Env: baseProviderEnv}
	if user != nil {
		stk.User = &tofu.User{UID: user.UID, GID: user.GID}
	}
	// The output of a failed show or pull can hold a value of the state
	// that the mask has not learned yet, so only the step and its exit
	// code are kept, as for a refused plan on S3.
	state, err := s.tofuRunner.Show(ctx, stk, raw)
	if err != nil {
		return tofu.State{}, nil, err
	}
	return state, m, nil
}

// adoptPull pulls the state of a's stack on S3 with the plan role, as the
// timer's refresh does, records the read, and returns the state, a mask
// of the run, and the read's place. The pulled serial must be the one a
// names.
func (s *Server) adoptPull(ctx context.Context, a adoption, id string, ts repo.TofuStack, dir, work string) (tofu.State, *mask.Mask, int64, error) {
	if err := s.s3Role(a.env, a.stack, planRole); err != nil {
		return tofu.State{}, nil, 0, err
	}
	started := s.now()
	launch, done, err := s.launch(ctx, id, a.env, planRole, a.p)
	if err != nil {
		return tofu.State{}, nil, 0, err
	}
	defer done()
	if launch.User == nil {
		return tofu.State{}, nil, 0, tofuRefused("this writ serve has no provider_uids to run OpenTofu as, so it would run as writ's own user: set provider_uids")
	}
	runEnv, creds, secrets := s.tofuEnv(launch.Env)
	m := launch.mask(secrets)
	lock, err := s.tofuRunner.Fill(ctx, ts.Lock)
	if err != nil {
		return tofu.State{}, nil, 0, fmt.Errorf("install the providers of %s into the plugin cache: %w%s", tofu.LockFile, err, stepLines(m, err))
	}
	stk := tofu.Stack{Files: os.DirFS(work), Dir: dir, Lock: lock, Env: runEnv, Creds: creds, StateCreds: true, User: &tofu.User{UID: launch.User.UID, GID: launch.User.GID}}
	state, err := s.tofuRunner.Pull(ctx, stk)
	if err != nil {
		return tofu.State{}, nil, 0, err
	}
	got := state.Serial
	if got.Serial != a.want.Serial || got.Lineage != a.want.Lineage {
		return tofu.State{}, nil, 0, &classedError{diag.State, fmt.Errorf("the bucket holds serial %d of lineage %s of the state of %s@%s, not serial %d of lineage %s, so writ serve kept nothing: adopt the newest",
			got.Serial, got.Lineage, a.stack, a.env, a.want.Serial, a.want.Lineage)}
	}
	_, seq, err := s.observeSerial(ctx, a.env, a.stack, ts.Backend, got, started)
	if err != nil {
		return tofu.State{}, nil, 0, fmt.Errorf("record the serial of the state of %s@%s that the adoption read: %w", a.stack, a.env, err)
	}
	if seq == 0 {
		return tofu.State{}, nil, 0, &classedError{diag.State, fmt.Errorf("an apply of %s@%s ran while the adoption read the state, so writ serve kept nothing: adopt after it ends", a.stack, a.env)}
	}
	return state, m, seq, nil
}

// noSecretSetRE finds the state version, or the S3 serial, that a plan's
// diagnostic or a drift check's error says has no secret set.
var noSecretSetRE = regexp.MustCompile(`(?:^|: )no secret set for state version (\d+) of `)

// AdoptCommand is the command that adopts the state that msg, a plan's
// diagnostic or a drift check's error, says has no secret set, for
// stack@env on backend. It reports false for any other message.
func AdoptCommand(msg, stack, env, backend string) (string, bool) {
	m := noSecretSetRE.FindStringSubmatch(msg)
	if m == nil {
		return "", false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return "", false
	}
	flag := "--version"
	if backend == control.BackendS3 {
		flag = "--serial"
	}
	return fmt.Sprintf("writ state adopt %s --env %s %s %d --reason REASON", stack, env, flag, n), true
}
