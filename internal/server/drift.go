package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/mask"
)

const (
	// FormatDrift is GET /v1/drift.
	FormatDrift = "writ.drift/v0"
	// FormatStateIndex is GET /v1/index.
	FormatStateIndex = "writ.state-index/v0"
)

// maxDriftChanges bounds the changes a drift check keeps; its counts are
// whole.
const maxDriftChanges = 200

// maxDriftTick bounds how long a drift check that an apply made due waits
// for the next look.
const maxDriftTick = 30 * time.Second

// driftPrincipal is who a drift check plans as: its role session and its
// share of the plan slots.
var driftPrincipal = &principal{info: Principal{Display: "drift", Person: "writ-serve-drift"}}

// checkDrift looks for due drift checks at start and then every tick
// until ctx ends. Close waits for it and for the checks it starts.
func (s *Server) checkDrift(ctx context.Context) {
	t := time.NewTicker(min(s.cfg.DriftEvery/4, maxDriftTick))
	defer t.Stop()
	for {
		s.driftLook(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// driftLook starts a drift check of each due stack@env, those checked
// longest ago first, while the plan slots allow: the checks share
// limits.plans with every other plan, take at most
// limits.plans_per_principal of them, and never the last free one, which
// stays for people's plans. A stack@env that finds no slot waits for the
// next look.
func (s *Server) driftLook(ctx context.Context) {
	var envs []string
	if !s.cfg.Dev {
		envs = s.envNames()
		if envs == nil {
			envs = []string{}
		}
	}
	due, err := s.store.DriftDue(ctx, s.now().Add(-s.cfg.DriftEvery), envs, s.cfg.Limits.PlansPerPrincipal)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("find the stacks whose drift check is due", "err", err)
		}
		return
	}
	key := driftPrincipal.key()
	for _, st := range due {
		if ctx.Err() != nil || !s.claimSpare(key, 1) {
			return
		}
		ref, started := newID(), s.now()
		if err := s.store.BeginDrift(ctx, ref, st.Env, st.Stack, started); err != nil {
			s.release(key)
			if ctx.Err() == nil {
				s.log.Error("store a drift check", "stack", st.Stack, "env", st.Env, "err", err)
			}
			return
		}
		s.wg.Go(func() {
			defer s.release(key)
			s.runDrift(control.DriftCheck{Ref: ref, Env: st.Env, Stack: st.Stack, StartedAt: started})
		})
	}
}

// runDrift runs one drift check and records how it ended.
func (s *Server) runDrift(c control.DriftCheck) {
	log := s.log.With("check", c.Ref, "stack", c.Stack, "env", c.Env)
	ctx, cancel := s.withLimit(s.ctx, "the drift check", "limits.plan_timeout", s.cfg.Limits.PlanTimeout)
	defer cancel()
	err := s.driftPlan(ctx, &c)
	if te := timedOut(ctx); te != nil && !errors.Is(err, te) {
		err = te
	}
	if ns, ok := errors.AsType[*noSecretSet](err); ok {
		// The plan's output may hold a past secret that no set names, so
		// the check keeps none of it.
		c.Status, c.Error = control.DriftFailed, fmt.Sprintf("drift unknown: no secret set for state version %d of %s@%s", ns.version, ns.stack, ns.env)
	} else if err != nil {
		c.Status, c.Error = control.DriftFailed, err.Error()
	}
	c.MadeAt = s.now()
	rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer rcancel()
	stored, ferr := s.store.FinishDrift(rctx, c)
	switch {
	case ferr != nil:
		log.Error("record the end of a drift check", "err", ferr)
	case !stored:
		log.Info("an apply ran while the drift check planned, so it keeps nothing; the next look checks again")
	case c.Status == control.DriftDrifted:
		log.Warn("drift found", "plan", c.PlanID, "commit", c.Commit, "outside", c.Outside, "code", c.Code)
	case c.Status == control.DriftFailed:
		log.Warn("drift check failed", "err", c.Error)
	default:
		log.Info("drift check found no drift", "plan", c.PlanID, "commit", c.Commit)
	}
}

// driftPlan plans c's stack@env at the head of the default branch of the
// stack's repository, by the path of a plan of a commit with the plan
// role, and fills c with what the masked plan changes. The saved plan is
// dropped: no API names it, and nothing applies it.
func (s *Server) driftPlan(ctx context.Context, c *control.DriftCheck) error {
	rp, dir, err := s.driftRepo(ctx, c.Env, c.Stack)
	if err != nil {
		return err
	}
	c.Repo, c.Commit = rp.Name, rp.Head
	client, err := s.clientOf(rp)
	if err != nil {
		return err
	}
	snap, err := s.commitSnapshot(ctx, rp, client, rp.Head)
	if err != nil {
		return err
	}
	in := planInput{repo: rp, client: client, dir: dir,
		source: control.PlanSource{Repo: rp.Name, Ref: "refs/heads/" + rp.DefaultBranch, Commit: rp.Head, RefCheck: control.OnRef}}
	res, err := s.planTofu(ctx, c.Ref, c.Env, c.Stack, in, snap, driftPrincipal)
	if err != nil {
		return err
	}
	if res.Status == control.RefusedPlan {
		return errors.New(strings.TrimSpace(strings.TrimPrefix(res.Text, "writ: ")))
	}
	var doc mask.Plan
	if err := json.Unmarshal(res.Document, &doc); err != nil {
		return fmt.Errorf("read the masked plan: %w", err)
	}
	c.PlanID = res.ID
	c.Changes, c.Outside, c.Code, err = driftChanges(&doc)
	if err != nil {
		return err
	}
	c.Status = control.DriftClean
	if c.Outside+c.Code > 0 {
		c.Status = control.DriftDrifted
	}
	return nil
}

// driftRepo finds the repository whose default branch holds stack as an
// OpenTofu stack at its head, and the stack's directory in it. When
// several do, it is the one whose code writ serve applied last to
// stack@env.
func (s *Server) driftRepo(ctx context.Context, env, stack string) (control.Repo, string, error) {
	repos, err := s.store.Repos(ctx)
	if err != nil {
		return control.Repo{}, "", fmt.Errorf("read the repositories: %w", err)
	}
	var found []control.Repo
	for _, rp := range repos {
		dir, ok := stackDir(rp, stack)
		if !ok || rp.Head == "" {
			continue
		}
		head, err := s.store.Commit(ctx, rp.Name, rp.Head)
		if err != nil {
			return control.Repo{}, "", fmt.Errorf("commit %s of repository %s: %w", forge.Short(rp.Head), rp.Name, err)
		}
		if slices.ContainsFunc(stacksOf(head), func(d discovered) bool { return d.Dir == dir && d.Tofu }) {
			found = append(found, rp)
		}
	}
	switch len(found) {
	case 0:
		return control.Repo{}, "", fmt.Errorf("no repository of this control plane holds %s as an OpenTofu stack at the head of its default branch", stack)
	case 1:
		dir, _ := stackDir(found[0], stack)
		return found[0], dir, nil
	}
	last, err := s.store.LastApplied(ctx, env, stack, s.now())
	if err != nil && !errors.Is(err, control.ErrNotFound) {
		return control.Repo{}, "", err
	}
	names := make([]string, len(found))
	for i, rp := range found {
		if rp.Name == last.Source.Repo {
			dir, _ := stackDir(rp, stack)
			return rp, dir, nil
		}
		names[i] = rp.Name
	}
	return control.Repo{}, "", fmt.Errorf("repositories %s each hold an OpenTofu stack %s, and no apply of writ serve to %s@%s names one of them", strings.Join(names, ", "), stack, stack, env)
}

// driftChanges lists what a masked plan changes: each resource OpenTofu
// read changed outside OpenTofu, from resource_drift, and each change an
// apply would make, from resource_changes, at most maxDriftChanges of
// them, and counts each kind.
func driftChanges(p *mask.Plan) (changes []control.DriftChange, outside, code int, err error) {
	for _, kind := range []string{control.DriftOutside, control.DriftCode} {
		rcs := p.ResourceChanges
		if kind == control.DriftOutside {
			rcs = p.ResourceDrift
		}
		for _, rc := range rcs {
			action, _, err := TofuChange(rc)
			if err != nil {
				return nil, 0, 0, err
			}
			if action == "" {
				continue
			}
			if kind == control.DriftOutside {
				outside++
			} else {
				code++
			}
			if len(changes) < maxDriftChanges {
				changes = append(changes, control.DriftChange{Address: rc.Address, Kind: kind, Action: action, Attributes: changedAttributes(rc.Change)})
			}
		}
	}
	return changes, outside, code, nil
}

// changedAttributes names the top-level attributes of a change with a
// before and an after, an update or a replace, whose masked values
// differ, whose sensitive values differ, or whose values are known only
// after apply.
func changedAttributes(c mask.Change) []string {
	before, _ := c.Before.(map[string]any)
	after, _ := c.After.(map[string]any)
	if before == nil || after == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, k := range slices.Concat(slices.Collect(maps.Keys(before)), slices.Collect(maps.Keys(after))) {
		if !reflect.DeepEqual(before[k], after[k]) {
			seen[k] = true
		}
	}
	for _, path := range c.ChangedSensitivePaths {
		if k, ok := firstStep(path); ok {
			seen[k] = true
		}
	}
	if unknown, ok := c.AfterUnknown.(map[string]any); ok {
		for k, v := range unknown {
			if v == true {
				seen[k] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func firstStep(path []any) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	k, ok := path[0].(string)
	return k, ok
}

// driftRefusal refuses a change of a plan id that no plan has, when a
// drift check of a stack@env the principal may read made it: a drift plan
// is never approved or applied. It reports false, and answers nothing,
// when no such check made it.
func (s *Server) driftRefusal(w http.ResponseWriter, r *http.Request, format, id string) bool {
	c, err := s.store.DriftPlan(r.Context(), id)
	switch {
	case errors.Is(err, control.ErrNotFound):
		return false
	case err != nil:
		s.dbFailed(w, r, format, err)
		return true
	case !s.cfg.Policy.Decide(reqOf(r).principal.teams, authz.Read, []string{c.Stack}, c.Env, nil).Allowed:
		return false
	}
	reqOf(r).entry.Plan = id
	s.refuse(w, r, format, diag.State, http.StatusConflict, "plan %s is a drift check of %s@%s, which writ serve never approves or applies: plan the stack to change it", id, c.Stack, c.Env)
	return true
}

// DriftDoc is GET /v1/drift: each OpenTofu stack@env writ serve keeps
// state for that the principal may read, with its drift.
type DriftDoc struct {
	Format      string       `json:"format"`
	OK          bool         `json:"ok"`
	Stacks      []DriftStack `json:"stacks"`
	More        bool         `json:"more"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// DriftStack is one stack@env of writ.drift/v0.
type DriftStack struct {
	Stack   string `json:"stack"`
	Env     string `json:"env"`
	Backend string `json:"backend"`
	// Resources counts the rows of its state index.
	Resources int `json:"resources"`
	// IndexedVersion is the state version, or the serial on S3, the index
	// was read from, 0 when nothing indexed it.
	IndexedVersion int64  `json:"indexed_version"`
	IndexedAt      string `json:"indexed_at"`
	// OutsideWrite is the newest serial of a state on S3 that no apply of
	// writ serve wrote, null when writ serve read none.
	OutsideWrite *OutsideWrite `json:"outside_write"`
	// Checking reports a drift check that plans now.
	Checking bool `json:"checking"`
	// Check is the newest drift check that ended, null when none did.
	Check *DriftCheckInfo `json:"check"`
}

// OutsideWrite is a serial of a state on S3 that writ serve read and no
// apply of it wrote, and when it read it.
type OutsideWrite struct {
	Serial int64  `json:"serial"`
	SeenAt string `json:"seen_at"`
}

// DriftCheckInfo is a drift check as writ.drift/v0 names it.
type DriftCheckInfo struct {
	// Plan is the id of the drift plan, "" when the check made none.
	Plan         string                `json:"plan"`
	Status       string                `json:"status"`
	StartedAt    string                `json:"started_at"`
	CheckedAt    string                `json:"checked_at"`
	Repo         string                `json:"repo"`
	Commit       string                `json:"commit"`
	Outside      int                   `json:"outside"`
	Code         int                   `json:"code"`
	DriftedSince string                `json:"drifted_since"`
	Changes      []control.DriftChange `json:"changes"`
	Error        string                `json:"error"`
}

// listDrift is GET /v1/drift?env=&stack=&limit=. It lists only the
// stack@envs the principal may read, and leaves them out before it
// counts the limit.
func (s *Server) listDrift(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	env, stack, limit := q.Get("env"), q.Get("stack"), 50
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 200 {
			s.refuse(w, r, FormatDrift, diag.Usage, http.StatusBadRequest, "limit must be 1 to 200")
			return
		}
		limit = n
	}
	if env != "" {
		e, ok := s.target(w, r, FormatDrift, env, cmpOr(stack, "x"))
		if !ok {
			return
		}
		env = e.Name
	} else if err := CheckStackName(cmpOr(stack, "x")); err != nil {
		s.refuse(w, r, FormatDrift, diag.Usage, http.StatusBadRequest, "%v", err)
		return
	}
	req := reqOf(r)
	stacks, more, err := s.store.DriftStacks(r.Context(), env, stack, limit, func(env, stack string) bool {
		_, served := s.envOf(env)
		return served && s.cfg.Policy.Decide(req.principal.teams, authz.Read, []string{stack}, env, nil).Allowed
	})
	if err != nil {
		s.dbFailed(w, r, FormatDrift, err)
		return
	}
	req.entry.Op, req.entry.Env, req.entry.Decision = string(authz.Read), env, control.Allowed
	doc := DriftDoc{Format: FormatDrift, OK: true, Stacks: []DriftStack{}, More: more, Diagnostics: []Diagnostic{}}
	for _, st := range stacks {
		doc.Stacks = append(doc.Stacks, *driftRow(st))
		req.entry.Stacks = append(req.entry.Stacks, st.Stack)
	}
	slices.Sort(req.entry.Stacks)
	req.entry.Stacks = slices.Compact(req.entry.Stacks)
	s.reply(w, http.StatusOK, doc)
}

// driftRow is a stack's row of writ.drift/v0.
func driftRow(st control.DriftStack) *DriftStack {
	row := &DriftStack{Stack: st.Stack, Env: st.Env, Backend: st.Backend, Resources: st.Resources, IndexedVersion: st.Version, IndexedAt: stamp(st.IndexedAt), Checking: st.Checking}
	if st.OutsideSerial > 0 {
		row.OutsideWrite = &OutsideWrite{Serial: st.OutsideSerial, SeenAt: stamp(st.OutsideAt)}
	}
	if c := st.Last; c != nil {
		row.Check = &DriftCheckInfo{Plan: c.PlanID, Status: string(c.Status), StartedAt: stamp(c.StartedAt), CheckedAt: stamp(c.MadeAt), Repo: c.Repo,
			Commit: c.Commit, Outside: c.Outside, Code: c.Code, DriftedSince: stamp(c.DriftedSince), Changes: c.Changes, Error: c.Error}
		if row.Check.Changes == nil {
			row.Check.Changes = []control.DriftChange{}
		}
	}
	return row
}

// StateIndexDoc is GET /v1/index: what one OpenTofu stack@env's state
// manages, as the state index holds it.
type StateIndexDoc struct {
	Format         string          `json:"format"`
	OK             bool            `json:"ok"`
	Stack          string          `json:"stack"`
	Env            string          `json:"env"`
	Backend        string          `json:"backend"`
	IndexedVersion int64           `json:"indexed_version"`
	Lineage        string          `json:"lineage"`
	IndexedAt      string          `json:"indexed_at"`
	OutsideWrite   *OutsideWrite   `json:"outside_write"`
	Resources      []IndexResource `json:"resources"`
	Diagnostics    []Diagnostic    `json:"diagnostics"`
}

// IndexResource is one row of the state index: its address, type, and
// provider, and the attributes of the index's allow-list, "" for one the
// state does not hold as a string, marks sensitive, or the mask swept.
type IndexResource struct {
	Address     string `json:"address"`
	Type        string `json:"type"`
	Provider    string `json:"provider"`
	ID          string `json:"id"`
	ARN         string `json:"arn"`
	Identifier  string `json:"identifier"`
	Region      string `json:"region"`
	Namespace   string `json:"namespace"`
	ModifyIndex string `json:"modify_index"`
}

// stateIndex is GET /v1/index?env=&stack=.
func (s *Server) stateIndex(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stack := q.Get("stack")
	e, ok := s.target(w, r, FormatStateIndex, q.Get("env"), stack)
	if !ok || !s.decide(w, r, FormatStateIndex, authz.Read, stack, e.Name) {
		return
	}
	ix, err := s.store.Index(r.Context(), e.Name, stack)
	if errors.Is(err, control.ErrNotFound) {
		s.refuse(w, r, FormatStateIndex, diag.Usage, http.StatusNotFound, "the state index holds nothing of %s@%s: it is not an OpenTofu stack whose state writ serve read", stack, e.Name)
		return
	}
	if err != nil {
		s.dbFailed(w, r, FormatStateIndex, err)
		return
	}
	doc := StateIndexDoc{Format: FormatStateIndex, OK: true, Stack: stack, Env: e.Name, Backend: ix.Backend, IndexedVersion: ix.Version, Lineage: ix.Lineage,
		IndexedAt: stamp(ix.At), Resources: []IndexResource{}, Diagnostics: []Diagnostic{}}
	if ix.Version == 0 {
		doc.IndexedAt = ""
	}
	if ix.Drift > 0 {
		doc.OutsideWrite = &OutsideWrite{Serial: ix.Drift, SeenAt: stamp(ix.DriftAt)}
	}
	for _, row := range ix.Rows {
		doc.Resources = append(doc.Resources, IndexResource{Address: row.Address, Type: row.Type, Provider: row.Provider, ID: row.ID, ARN: row.ARN,
			Identifier: row.Identifier, Region: row.Region, Namespace: row.Namespace, ModifyIndex: row.ModifyIndex})
	}
	s.reply(w, http.StatusOK, doc)
}
