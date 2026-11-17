package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/repo"
	"github.com/mr-karan/writ/internal/tfstate"
	"github.com/mr-karan/writ/internal/tofu"
)

// s3Allowed are the attributes of an S3 backend block that writ serve
// runs, from the backend's schema in OpenTofu 1.13.9
// (internal/backend/remote-state/s3/backend.go): those that find and
// lock the state, choose its encryption, or tune how the client calls
// AWS's own endpoints. Every other attribute and block is refused: each
// names credentials, a role, a profile, a file, a proxy, a CA or a
// custom endpoint, sets the state object's ACL, or is not needed to find
// the state. writ serve gives tofu the credentials of the environment's
// role, and a stack must not choose others or send that role's signed
// requests elsewhere. endpoints is checked on its own, against the
// environment's s3_endpoints.
var s3Allowed = []string{
	"bucket", "key", "region", "workspace_key_prefix",
	"use_lockfile", "dynamodb_table",
	"encrypt", "kms_key_id",
	"use_path_style", "force_path_style", "use_dualstack_endpoint", "use_fips_endpoint",
	"skip_credentials_validation", "skip_region_validation", "skip_requesting_account_id", "skip_s3_checksum",
	"max_retries", "retry_mode",
}

// stateBackend is the kind of state backend a stack's code declares, the
// http backend writ serve keeps or the stack's own S3 bucket, or why
// writ serve does not run it in env.
func (s *Server) stateBackend(env, stack string, ts repo.TofuStack) (string, error) {
	be := ts.Backend
	switch {
	case ts.Cloud:
		return "", tofuRefused("stack %s declares a cloud block; writ serve runs stacks on the http backend it serves or on S3: write backend \"http\" {} in its terraform block", stack)
	case be == nil:
		return "", tofuRefused("stack %s declares no backend, so its state would be lost after each run: write backend \"http\" {} in its terraform block, and writ serve keeps its state", stack)
	case be.Type == "http":
		if len(be.Set) > 0 {
			return "", tofuRefused("stack %s sets %s in its http backend block; writ serve gives each run the backend's address and credentials, so write backend \"http\" {} with nothing in it", stack, strings.Join(be.Set, ", "))
		}
		return control.BackendHTTP, nil
	case be.Type == "s3":
		return control.BackendS3, s.s3Backend(env, stack, be)
	}
	return "", tofuRefused("stack %s uses backend %q, which writ serve does not run: write backend \"http\" {}, whose state writ serve keeps, or keep the state in S3 with backend \"s3\"", stack, be.Type)
}

// s3Backend refuses an S3 backend block that sets anything but s3Allowed
// and endpoints, sets one by an expression, names an endpoint env does
// not list, or takes no lock.
func (s *Server) s3Backend(env, stack string, be *repo.Backend) error {
	var refused, computed []string
	for _, name := range be.Set {
		_, lit := be.Literals[name]
		switch {
		case name == "endpoints":
			if _, ok := be.Objects[name]; !ok {
				computed = append(computed, name)
			}
		case !slices.Contains(s3Allowed, name):
			refused = append(refused, name)
		case !lit:
			computed = append(computed, name)
		}
	}
	if len(refused) > 0 {
		return tofuRefused("stack %s sets %s in its s3 backend block, which writ serve does not run: it gives each run the credentials of the environment's role, and the block may set only %s, and endpoints that the environment's s3_endpoints lists", stack, strings.Join(refused, ", "), strings.Join(s3Allowed, ", "))
	}
	if len(computed) > 0 {
		return tofuRefused("stack %s sets %s in its s3 backend block by an expression; writ serve checks what the block names, so write each as a literal", stack, strings.Join(computed, ", "))
	}
	listed := s.cfg.Envs[env].S3Endpoints
	endpoints := be.Objects["endpoints"]
	for _, api := range slices.Sorted(maps.Keys(endpoints)) {
		if u := endpoints[api]; !slices.Contains(listed, u) {
			return tofuRefused("stack %s sets endpoints.%s = %q in its s3 backend block, which env %s's s3_endpoints does not list: a custom endpoint gets the requests the role signs, so list it there or leave it out", stack, api, u, env)
		}
	}
	if be.Literals["use_lockfile"] != "true" && be.Literals["dynamodb_table"] == "" {
		return tofuRefused("stack %s's s3 backend takes no lock: set use_lockfile = true, or a dynamodb_table, so an apply of writ serve and a write from elsewhere cannot change the state at once", stack)
	}
	return nil
}

// s3Role refuses an S3 stack that a run of role cannot reach: the dev
// control plane has no credential broker, and an environment without the
// role gives tofu no credential.
func (s *Server) s3Role(env, stack string, role roleKind) error {
	if s.cfg.Dev {
		return tofuRefused("stack %s keeps its state in S3, and writ serve --dev runs no stack on S3: it has no roles to read the state with", stack)
	}
	e := s.cfg.Envs[env]
	if role == applyRole && e.ApplyRole == "" || role == planRole && e.PlanRole == "" {
		return tofuRefused("stack %s keeps its state in S3, and env %s has no %s_role to reach it with: set one", stack, env, role)
	}
	return nil
}

// s3Object names a stack's state in its bucket, for the log.
func s3Object(stack string, be *repo.Backend) string {
	bucket, key := be.Literals["bucket"], be.Literals["key"]
	if bucket == "" || key == "" {
		return "the S3 state of " + stack
	}
	return "s3://" + bucket + "/" + key
}

// observeSerial records a serial of a stack's state on S3 that a run of
// writ serve read, and returns who wrote it and the read's place among
// the reads of the stack@env. It records nothing, and returns 0, when an
// apply of the stack ran at or after since, which may have written the
// serial: the next read judges it. A serial it has not read and no apply
// of writ serve wrote is drift: the store marks the stack's index record
// with it, and it is logged. The first serial writ serve reads of a
// stack@env is where it starts.
func (s *Server) observeSerial(ctx context.Context, env, stack string, be *repo.Backend, read tofu.Serial, since time.Time) (control.Judged, int64, error) {
	if read.Serial == 0 {
		return control.Judged{}, 0, nil
	}
	j, seq, err := s.store.ObserveSerial(ctx, control.SerialRead{Env: env, Stack: stack, Lineage: read.Lineage, Serial: read.Serial, At: s.now(), Since: since})
	switch {
	case err != nil:
		return j, 0, err
	case seq == 0:
		s.log.Debug("an apply ran while a run read the state; the next read judges its serial", "stack", stack, "env", env, "serial", read.Serial)
	case j.Writer == control.SerialOutside:
		s.log.Warn("state written outside writ", "stack", stack, "env", env, "serial", read.Serial, "lineage", read.Lineage, "object", s3Object(stack, be))
	}
	return j, seq, nil
}

// appliedS3 records the serial an apply of a stack on S3 left as the
// apply's, when the apply wrote it: the serial is of its plan's lineage
// and above the serial its plan read, and the apply succeeded or began to
// change resources, which it does only while it holds the state's lock.
// Every serial above the one its plan read, up to it, is the apply's. The
// secrets the apply's mask knew, the environment's, the state's set it
// carried, and those it read from the plan, are sealed with it, so a plan
// after a partial apply masks the state as one after a whole apply does,
// and the index is filled from it. An apply that
// failed before it changed anything claims no serial: one above its
// plan's is someone else's, as the writer that held the lock, and the
// next read judges it. It returns the serial the apply left, and fails no
// apply.
//
// tofu apply lets go of the lock before writ serve pulls the state, and
// OpenTofu 1.13.9 reports no serial it wrote, so a write from elsewhere
// in that window would be claimed as the apply's.
func (s *Server) appliedS3(ctx context.Context, run control.Run, plan control.Plan, key *tfstate.Key, m *mask.Mask, masked bool, saved []byte, res tofu.Applied, ok bool) int64 {
	log := s.log.With("run", run.ID, "stack", plan.Stack, "env", plan.Env)
	if res.ShowErr != nil {
		log.Error("read the state the apply left; the next refresh of the index reads it", "err", res.ShowErr)
		return 0
	}
	got := res.State.Serial
	if got.Serial == 0 {
		return 0
	}
	if !ok && !changedResources(res.Log) {
		log.Info("the apply failed before it changed anything, so it claims no serial of the state; the next read judges it", "serial", got.Serial)
		return got.Serial
	}
	read, err := tofu.PlanSerial(saved)
	if err != nil {
		log.Error("read the serial the apply's plan read; the next refresh judges the state's serial", "err", err)
		return got.Serial
	}
	switch {
	case read.Serial > 0 && got.Lineage != read.Lineage:
		log.Warn("the state the apply left is of another lineage than the state its plan read, so the apply claims it not; the next read judges it", "serial", got.Serial, "lineage", got.Lineage, "plan_lineage", read.Lineage)
		return got.Serial
	case got.Serial <= read.Serial:
		// The apply wrote nothing, and its plan's run read this serial.
		return got.Serial
	}
	var sealed []byte
	if masked {
		sealed = key.SealIndexSecrets(m.Secrets(), plan.Env, plan.Stack, got.Serial)
	}
	seq, err := s.store.PutSerial(ctx, control.StateSerial{Env: plan.Env, Stack: plan.Stack, Lineage: got.Lineage, Serial: got.Serial, RunID: run.ID, From: read.Serial, SeenAt: s.now()}, sealed)
	if err != nil {
		log.Error("record the serial the apply wrote; a later read of it marks it as drift", "serial", got.Serial, "err", err)
		return got.Serial
	}
	if masked {
		if _, err := s.fillIndex(ctx, plan.Env, plan.Stack, control.BackendS3, got, seq, m, res.State); err != nil {
			log.Error("index the state the apply left; the next refresh reads it", "err", err)
		}
	}
	return got.Serial
}

// indexPrincipal is who a refresh of the index assumes the plan role as.
var indexPrincipal = &principal{info: Principal{Display: "index", Person: "writ-serve-index"}}

// refreshS3 reads the state of a stack@env on S3 and refills its index,
// with a run: the files of the commit the newest apply of writ serve that
// wrote the state applied, init with the plan role, state pull with it,
// and show -json of what it pulled with none, as a provider uid. A serial
// no apply of writ serve wrote is marked as drift and is
// not indexed: no apply's mask knew its plan's secrets.
func (s *Server) refreshS3(ctx context.Context, st control.S3Stack) error {
	started := s.now()
	run, err := s.store.GetRun(ctx, st.RunID)
	if err != nil {
		return fmt.Errorf("run %s, which wrote the state: %w", st.RunID, err)
	}
	plan, err := s.store.Plan(ctx, run.PlanID, s.now())
	if err != nil {
		return fmt.Errorf("plan %s, which run %s applied: %w", run.PlanID, run.ID, err)
	}
	ts, dir, err := s.tofuSourcesOf(ctx, plan)
	if err != nil {
		return err
	}
	if kind, err := s.stateBackend(st.Env, st.Stack, ts); err != nil {
		return err
	} else if kind != control.BackendS3 {
		return fmt.Errorf("the code run %s applied keeps its state on backend %s, not S3", run.ID, kind)
	}
	if err := s.s3Role(st.Env, st.Stack, planRole); err != nil {
		return err
	}
	id := "index-" + newID()
	work := filepath.Join(s.cfg.Data, "work", id)
	defer os.RemoveAll(work)
	if err := writeTofuFiles(work, ts.Files, plan.Source.Commit); err != nil {
		return err
	}
	launch, done, err := s.launch(ctx, id, st.Env, planRole, indexPrincipal)
	if err != nil {
		return err
	}
	defer done()
	if launch.User == nil {
		return tofuRefused("this writ serve has no provider_uids to run OpenTofu as, so it would run as writ's own user: set provider_uids")
	}
	runEnv, creds, secrets := s.tofuEnv(launch.Env)
	m := mask.New(secrets)
	lock, err := s.tofuRunner.Fill(ctx, ts.Lock)
	if err != nil {
		return fmt.Errorf("install the providers of %s into the plugin cache: %w%s", tofu.LockFile, err, stepLines(m, err))
	}
	stk := tofu.Stack{Files: os.DirFS(work), Dir: dir, Lock: lock, Env: runEnv, Creds: creds, StateCreds: true, User: &tofu.User{UID: launch.User.UID, GID: launch.User.GID}}
	state, err := s.tofuRunner.Pull(ctx, stk)
	if err != nil {
		return fmt.Errorf("%w%s", err, stepLines(m, err))
	}
	read := state.Serial
	j, seq, err := s.observeSerial(ctx, st.Env, st.Stack, ts.Backend, read, started)
	if err != nil || seq == 0 {
		return err
	}
	switch held, err := s.store.S3IndexHolds(ctx, st.Env, st.Stack, read.Lineage, read.Serial, seq); {
	case errors.Is(err, control.ErrStaleRead):
		s.log.Debug("a later read of the state follows the refresh's, so the refresh indexes nothing", "stack", st.Stack, "env", st.Env, "serial", read.Serial)
		return nil
	case err != nil:
		return err
	case held:
		return nil
	}
	sealed, err := s.store.SerialSecrets(ctx, st.Env, st.Stack, read.Lineage, read.Serial)
	if errors.Is(err, control.ErrNotFound) {
		why := "no apply of writ serve wrote it"
		if j.Writer == control.SerialWrit || j.RunID != "" {
			why = "it is not the serial an apply of writ serve ended at"
		}
		return fmt.Errorf("serial %d of the state is not indexed: %s, so no mask knew its plan's secrets; the index stays at its older serial until an apply of writ serve writes a newer one", read.Serial, why)
	}
	if err != nil {
		return err
	}
	key, err := s.stateKey(st.Env)
	if err != nil {
		return fmt.Errorf("the state key of env %s: %w", st.Env, err)
	}
	learned, err := key.OpenIndexSecrets(sealed, st.Env, st.Stack, read.Serial)
	if err != nil {
		return err
	}
	if stored, err := s.fillIndex(ctx, st.Env, st.Stack, control.BackendS3, read, seq, mask.New(append(secrets, learned...)), state); err != nil || !stored {
		return err
	}
	s.log.Info("state index refreshed", "stack", st.Stack, "env", st.Env, "serial", read.Serial)
	return nil
}

// changedResources reports whether an `apply -json` stream shows that
// tofu began to change a resource.
func changedResources(log []byte) bool {
	for line := range bytes.Lines(log) {
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &ev) == nil && ev.Type == "apply_start" {
			return true
		}
	}
	return false
}
