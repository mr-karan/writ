package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/repo"
	"github.com/mr-karan/writ/internal/tfstate"
	"github.com/mr-karan/writ/internal/tofu"
)

const (
	// indexEvery is how often writ serve looks for states newer than
	// their index, and refreshes those.
	indexEvery = 5 * time.Minute
	// maxIndexRefresh bounds the stacks one look refreshes.
	maxIndexRefresh = 50
)

// indexAttrs are the attributes the state index keeps of a resource of
// type typ: the identifiers of a cloud object, and a nomad_job's
// namespace and modify index. state_index has a column for each.
func indexAttrs(typ string) []string {
	attrs := []string{"arn", "id", "identifier", "region"}
	if typ == "nomad_job" {
		attrs = append(attrs, "namespace", "modify_index")
	}
	return attrs
}

// fillIndex replaces the state index of stack@env with the rows the mask
// reads from state, version v of the stack's state on backend: a version
// of the http backend, whose lineage is "" and seq 0, or a serial on S3,
// whose read PutSerial or ObserveSerial numbered seq. It reports false,
// and logs it, when the index holds a newer read, and the rows are
// dropped.
func (s *Server) fillIndex(ctx context.Context, env, stack, backend string, v tofu.Serial, seq int64, m *mask.Mask, state tofu.State) (bool, error) {
	rows, err := maskedRows(m, state)
	if err != nil {
		return false, fmt.Errorf("the mask refused the state of %s@%s, so the index keeps what it had: %w", stack, env, err)
	}
	return s.storeIndex(ctx, env, stack, backend, v, seq, rows)
}

// maskedRows are the rows the mask reads from state, as state_index
// stores them. The mask learns the state's secrets as it reads.
func maskedRows(m *mask.Mask, state tofu.State) ([]control.IndexRow, error) {
	rows, err := m.Index(state.Unmasked, indexAttrs)
	if err != nil {
		return nil, err
	}
	out := make([]control.IndexRow, len(rows))
	for i, r := range rows {
		if out[i], err = indexRow(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// storeIndex replaces the state index of stack@env with rows, as
// fillIndex says.
func (s *Server) storeIndex(ctx context.Context, env, stack, backend string, v tofu.Serial, seq int64, rows []control.IndexRow) (bool, error) {
	w := control.IndexWrite{Env: env, Stack: stack, Backend: backend, Version: v.Serial, Lineage: v.Lineage, Seq: seq, At: s.now(), Rows: rows}
	stored, err := s.store.ReplaceIndex(ctx, w)
	if err != nil {
		return false, fmt.Errorf("store the index of %s@%s: %w", stack, env, err)
	}
	if !stored {
		s.log.Debug("the index holds a newer read of the state, so this one is dropped", "stack", stack, "env", env, "backend", backend, "version", v.Serial, "read", seq)
	}
	return stored, nil
}

// indexRow is a row of the mask as state_index stores it. A value the mask
// swept is stored as no value, as one the state marks sensitive is.
func indexRow(r mask.Row) (control.IndexRow, error) {
	row := control.IndexRow{Address: r.Address, Type: r.Type, Provider: r.Provider}
	for name, v := range r.Attrs {
		if v == mask.Sensitive {
			v = ""
		}
		switch name {
		case "id":
			row.ID = v
		case "arn":
			row.ARN = v
		case "identifier":
			row.Identifier = v
		case "region":
			row.Region = v
		case "namespace":
			row.Namespace = v
		case "modify_index":
			row.ModifyIndex = v
		default:
			return control.IndexRow{}, fmt.Errorf("attribute %s of %s has no column in the state index", name, r.Address)
		}
	}
	return row, nil
}

// indexApplied fills the index of an apply's stack@env from the state the
// apply left, when that state is the newest version: the apply wrote it,
// or nothing wrote since the version the plan read. Otherwise, or when the
// fill fails, the index stays behind the state, and the next refresh reads
// the newest version. It fails no apply.
func (s *Server) indexApplied(ctx context.Context, run control.Run, plan control.Plan, m *mask.Mask, res tofu.Applied, latest control.StateVersion) {
	log := s.log.With("run", run.ID, "stack", plan.Stack, "env", plan.Env)
	switch {
	case res.ShowErr != nil:
		log.Error("read the state the apply left; the next refresh of the index reads it", "err", res.ShowErr)
		return
	case latest.Version == 0:
		return
	case latest.RunID != run.ID && latest.Version != plan.Tofu.StateVersion:
		log.Info("the state moved after the apply; the next refresh of the index reads it", "version", latest.Version)
		return
	}
	if _, err := s.fillIndex(ctx, plan.Env, plan.Stack, control.BackendHTTP, tofu.Serial{Serial: latest.Version}, 0, m, res.State); err != nil {
		log.Error("index the state the apply left; the next refresh reads it", "err", err)
	}
}

// keepIndexSecrets seals what the apply's mask knows, the run's
// environment and the plan's secrets, with each state version the apply
// wrote, for a refresh that indexes one of them: a state can hold a
// value only the plan marks, and a refresh has no plan. It fails no
// apply. Without them a refresh does not index the version.
func (s *Server) keepIndexSecrets(ctx context.Context, run control.Run, plan control.Plan, key *tfstate.Key, m *mask.Mask) {
	secrets := m.Secrets()
	n, err := s.store.PutIndexSecrets(ctx, plan.Env, plan.Stack, run.ID, func(version int64) []byte {
		return key.SealIndexSecrets(secrets, plan.Env, plan.Stack, version)
	})
	if err != nil {
		s.log.Error("keep the secrets of the apply's mask; a refresh will not index the state versions it wrote", "run", run.ID, "stack", plan.Stack, "env", plan.Env, "err", err)
		return
	}
	s.log.Debug("kept the secrets of the apply's mask", "run", run.ID, "stack", plan.Stack, "env", plan.Env, "versions", n)
}

// refreshIndexes refreshes the index of each stack whose state is newer
// than its index, at start and then every indexEvery, until ctx ends: a
// stack on the http backend whose apply wrote a state and failed to fill
// the index, or whose state predates the index, and each stack on S3 that
// an apply of writ serve wrote, whose bucket anyone may write.
func (s *Server) refreshIndexes(ctx context.Context) {
	t := time.NewTicker(indexEvery)
	defer t.Stop()
	for {
		s.refreshStale(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refreshStale is one look of refreshIndexes.
func (s *Server) refreshStale(ctx context.Context) {
	stale, err := s.store.StaleIndexes(ctx, maxIndexRefresh)
	if err != nil && ctx.Err() == nil {
		s.log.Error("find the states newer than their index", "err", err)
	}
	for _, st := range stale {
		if err := s.refreshIndex(ctx, st); err != nil && ctx.Err() == nil {
			s.log.Error("refresh the state index; the next look tries again", "stack", st.Stack, "env", st.Env, "version", st.Version, "err", err)
		}
	}
	onS3, err := s.store.S3Stacks(ctx, maxIndexRefresh)
	if err != nil && ctx.Err() == nil {
		s.log.Error("find the stacks on S3", "err", err)
	}
	for _, st := range onS3 {
		if err := s.refreshS3(ctx, st); err != nil && ctx.Err() == nil {
			s.log.Error("refresh the state index of a stack on S3; the next look tries again", "stack", st.Stack, "env", st.Env, "err", err)
		}
		if ctx.Err() != nil {
			return
		}
		// A failed read takes its turn too, so a stack that always fails
		// keeps none from the stacks after it.
		if err := s.store.CheckedS3(ctx, st.Env, st.Stack, s.now()); err != nil {
			s.log.Error("record the check of a stack on S3", "stack", st.Stack, "env", st.Env, "err", err)
		}
	}
}

// refreshIndex indexes the newest state of a stack@env on the http
// backend. A run reads it, as an apply does: the files of the commit the
// apply that wrote it applied, init without the backend, and show -json
// of the stored state, as a run's uid, with no credentials and no state
// token. Its mask knows the secrets a plan's run gets, and the secrets
// the mask of the apply that wrote the version knew. A version without
// those is not indexed.
func (s *Server) refreshIndex(ctx context.Context, st control.StaleIndex) error {
	v, err := s.store.LatestState(ctx, st.Env, st.Stack)
	if err != nil {
		return err
	}
	sealedSecrets, err := s.store.IndexSecrets(ctx, st.Env, st.Stack, v.Version)
	if errors.Is(err, control.ErrNotFound) {
		return fmt.Errorf("version %d has no sealed secrets of the mask of the apply that wrote it, so a refresh cannot know its plan's secrets and does not index it: "+
			"it was written before writ serve kept them, that apply's mask could not read its plan, or that apply is still running; the index stays at its older version until an apply writes a newer one", v.Version)
	}
	if err != nil {
		return err
	}
	run, err := s.store.GetRun(ctx, v.RunID)
	if err != nil {
		return fmt.Errorf("run %s, which wrote version %d: %w", v.RunID, v.Version, err)
	}
	plan, err := s.store.Plan(ctx, run.PlanID, s.now())
	if err != nil {
		return fmt.Errorf("plan %s, which run %s applied: %w", run.PlanID, run.ID, err)
	}
	key, err := s.stateKey(st.Env)
	if err != nil {
		return fmt.Errorf("the state key of env %s: %w", st.Env, err)
	}
	learned, err := key.OpenIndexSecrets(sealedSecrets, st.Env, st.Stack, v.Version)
	if err != nil {
		return err
	}
	raw, err := key.OpenState(v.Sealed, st.Env, st.Stack, v.Version)
	if err != nil {
		return err
	}
	ts, dir, err := s.tofuSourcesOf(ctx, plan)
	if err != nil {
		return err
	}
	// The run has no role: tofu gets none of the environment's secrets,
	// and the mask still learns them, as a state holds one where nothing
	// marks it, as an address keyed by nonsensitive(var.NAME) does.
	tr, err := s.startRun(ctx, runSpec{id: "index-" + newID(), env: st.Env, commit: plan.Source.Commit, ts: ts, dir: dir})
	if err != nil {
		return err
	}
	defer tr.done()
	m := tr.mask(learned)
	stk, err := tr.stack(ctx, m)
	if err != nil {
		return err
	}
	defer tr.network(m)
	state, err := s.tofuRunner.Show(ctx, stk, raw)
	if err != nil {
		return fmt.Errorf("%w%s", err, stepLines(m, err))
	}
	if stored, err := s.fillIndex(ctx, st.Env, st.Stack, control.BackendHTTP, tofu.Serial{Serial: v.Version}, 0, m, state); err != nil || !stored {
		return err
	}
	s.log.Info("state index refreshed", "stack", st.Stack, "env", st.Env, "version", v.Version)
	return nil
}

// tofuSourcesOf gathers the files of an OpenTofu plan's stack at the plan's
// commit again, checks they are the ones the plan was made from, and
// returns them with the stack's directory in them.
func (s *Server) tofuSourcesOf(ctx context.Context, plan control.Plan) (repo.TofuStack, string, error) {
	rp, err := s.store.Repo(ctx, plan.Source.Repo)
	if err != nil {
		return repo.TofuStack{}, "", fmt.Errorf("repository %s: %w", plan.Source.Repo, err)
	}
	c, err := s.clientOf(rp)
	if err != nil {
		return repo.TofuStack{}, "", err
	}
	dir, ok := stackDir(rp, plan.Stack)
	if !ok {
		return repo.TofuStack{}, "", tofuRefused("stack %s is no longer a stack of repository %s", plan.Stack, rp.Name)
	}
	snap, err := s.commitSnapshot(ctx, rp, c, plan.Source.Commit)
	if err != nil {
		return repo.TofuStack{}, "", err
	}
	ts, err := repo.TofuSources(ctx, c, rp.ProjectID, snap, dir, s.store, repo.DefaultLimits)
	if err != nil {
		return repo.TofuStack{}, "", &classedError{diag.Usage, err}
	}
	s.keepBlobs(rp.Name, snap)
	if ts.Digest != plan.SourceDigest {
		return repo.TofuStack{}, "", &classedError{diag.SavedPlan, fmt.Errorf("the sources of %s at %s are not the ones plan %s was made from", plan.Stack, forge.Short(plan.Source.Commit), plan.ID)}
	}
	return ts, dir, nil
}
