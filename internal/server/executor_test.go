package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/runner"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// recorder is an executor that keeps each job that crosses it to the
// executor it wraps.
type recorder struct {
	runner.Executor
	mu   sync.Mutex
	jobs []runner.Job
}

func (r *recorder) Reserve(ctx context.Context) (runner.Slot, error) {
	slot, err := r.Executor.Reserve(ctx)
	if err != nil {
		return nil, err
	}
	return recordedSlot{Slot: slot, r: r}, nil
}

func (r *recorder) seen() []runner.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.jobs)
}

type recordedSlot struct {
	runner.Slot
	r *recorder
}

func (s recordedSlot) Run(start, ctx context.Context, job runner.Job) (runner.Result, error) {
	s.r.mu.Lock()
	s.r.jobs = append(s.r.jobs, job)
	s.r.mu.Unlock()
	return s.Slot.Run(start, ctx, job)
}

// recording is a setup that wraps the server's executor in rec.
func recording(rec *recorder) func(*server.Options) {
	return func(o *server.Options) {
		o.WrapExecutor = func(e runner.Executor) runner.Executor {
			rec.Executor = e
			return rec
		}
	}
}

// blobs are the bytes a job carries.
func blobs(j runner.Job) [][]byte {
	out := [][]byte{[]byte(j.ID), []byte(j.Dir), j.Lock, j.SavedPlan, j.State}
	for _, b := range j.Files {
		out = append(out, b)
	}
	for _, kv := range slices.Concat(j.Env, j.Creds, j.Egress.Registries) {
		out = append(out, []byte(kv))
	}
	return out
}

// Every OpenTofu run of writ serve crosses to its executor as a job: a
// plan, its apply, a refresh of the index, and a drift check, and tofu
// runs nothing that did not. A job carries what its run needs: the
// commit's files, the env's provider_env values and the run's own state
// token, the opened saved plan of an apply, and the opened state of a
// refresh, which gets no credentials and no token. It carries none of
// writ serve's own: not the env's state key in any encoding, and not the
// forge's token.
func TestJobsCrossTheExecutor(t *testing.T) {
	rec := &recorder{}
	fake := &fakeTofu{showErr: errors.New("tofu show exited with code 1")}
	f := tofuApplyFixtureEdit(t, fake, func(s string) string { return s }, recording(rec))
	appliedApp(t, f, "add-cert")
	fake.mu.Lock()
	fake.showErr = nil
	fake.mu.Unlock()
	f.srv.RefreshIndexes()
	f.srv.LookForDrift(time.Hour)

	jobs := rec.seen()
	var kinds []runner.Kind
	for _, j := range jobs {
		kinds = append(kinds, j.Kind)
	}
	if want := []runner.Kind{runner.Plan, runner.Apply, runner.Show, runner.Plan}; !slices.Equal(kinds, want) {
		t.Fatalf("jobs %q, want %q", kinds, want)
	}
	fake.mu.Lock()
	ran := len(fake.stacks) + len(fake.applied) + len(fake.shown) + len(fake.pulled)
	fake.mu.Unlock()
	if ran != len(jobs) {
		t.Fatalf("tofu ran %d times for %d jobs", ran, len(jobs))
	}
	plan, apply, show, drift := jobs[0], jobs[1], jobs[2], jobs[3]
	for _, j := range []runner.Job{plan, apply, show, drift} {
		var names []string
		for name := range j.Files {
			names = append(names, name)
		}
		slices.Sort(names)
		if j.Dir != "app" || !slices.Equal(names, []string{"app/.terraform.lock.hcl", "app/main.tf", "modules/cert/main.tf"}) || string(j.Lock) != tofuLockFile {
			t.Errorf("the %s job carries dir %q, files %q, and lock file %q", j.Kind, j.Dir, names, j.Lock)
		}
	}
	for _, j := range []runner.Job{plan, apply, drift} {
		if envValue(j.Env, "TF_HTTP_PASSWORD") == "" || envValue(j.Creds, "TF_VAR_extra") != tofuEnvSecret || j.State != nil {
			t.Errorf("the %s job carries env %q, creds %q, and a state of %d bytes; want its state token and the env's provider_env, and no state", j.Kind, j.Env, j.Creds, len(j.State))
		}
	}
	if plan.SavedPlan != nil || drift.SavedPlan != nil || string(apply.SavedPlan) != "saved" {
		t.Errorf("saved plans: plan %q, apply %q, drift %q; want only the apply's, opened", plan.SavedPlan, apply.SavedPlan, drift.SavedPlan)
	}
	if want := `{"version":4,"serial":1,"lineage":"lineage-1","resources":[]}`; string(show.State) != want || len(show.Creds) != 0 || envValue(show.Env, "TF_HTTP_PASSWORD") != "" {
		t.Errorf("the refresh's job carries state %q, creds %q, and env %q; want the stored version, opened, and neither credentials nor a token", show.State, show.Creds, show.Env)
	}

	key, err := os.ReadFile(f.cfg.Envs["prod"].Key)
	if err != nil || len(key) == 0 {
		t.Fatalf("the state key of prod: %v", err)
	}
	held := map[string][]byte{
		"the state key": key, "the state key in hex": []byte(hex.EncodeToString(key)),
		"the state key in base64": []byte(base64.StdEncoding.EncodeToString(key)), "the forge's token": []byte(gitlabToken),
	}
	for _, j := range jobs {
		for _, b := range blobs(j) {
			for what, h := range held {
				if bytes.Contains(b, h) {
					t.Errorf("the %s job carries %s", j.Kind, what)
				}
			}
		}
	}
}

// fakeExecutor runs every job as a plan whose raw result is plan, and
// tofu not at all.
type fakeExecutor struct{ plan tofu.Plan }

func (e fakeExecutor) Reserve(context.Context) (runner.Slot, error) { return e, nil }
func (e fakeExecutor) Release()                                     {}
func (e fakeExecutor) Run(_, _ context.Context, job runner.Job) (runner.Result, error) {
	if job.Kind != runner.Plan {
		return runner.Result{}, errors.New("not a plan")
	}
	return runner.Result{Plan: e.plan}, nil
}

// A plan's document is what writ serve makes of the raw result its
// executor returns, through the mask: the sensitive value and the
// provider_env value in the raw plan are gone from what writ serve keeps,
// and OpenTofu itself ran nothing.
func TestPlanOfTheExecutorsResult(t *testing.T) {
	fake := &fakeTofu{}
	raw := tofu.Plan{File: []byte("saved"), JSON: []byte(tofuPlanJSON), Log: []byte(`{"type":"diagnostic","diagnostic":{"severity":"warning","summary":"Deprecated","detail":"uses ` + tofuEnvSecret + `"}}` + "\n")}
	f := tofuApplyFixtureEdit(t, fake, func(s string) string { return s }, func(o *server.Options) {
		o.WrapExecutor = func(runner.Executor) runner.Executor { return fakeExecutor{plan: raw} }
	})
	p, _ := f.planCommit(tofuFiles(), "add-cert")
	if p.Plan.Status != "pending" || len(p.Document) == 0 {
		t.Fatalf("plan %s: %s\n%s", p.Plan.Status, p.Document, p.Text)
	}
	for _, canary := range []string{tofuCanary, tofuEnvSecret} {
		if strings.Contains(string(p.Document), canary) || strings.Contains(p.Text, canary) {
			t.Errorf("the plan keeps %s:\n%s\n%s", canary, p.Document, p.Text)
		}
	}
	if !strings.Contains(p.Text, "Deprecated") {
		t.Errorf("the plan's text lacks the warning of the raw result:\n%s", p.Text)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.fills)+len(fake.stacks) != 0 {
		t.Errorf("OpenTofu ran %d fills and %d plans, want none", len(fake.fills), len(fake.stacks))
	}
}
