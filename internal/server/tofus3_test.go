package server_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/awsrole/ststest"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// s3Endpoint is the custom endpoint of the S3 stand-in that app's
// backend names and the env lists. No test sends it a request.
const s3Endpoint = "http://127.0.0.1:9000"

// tofuS3Files is tofuFiles with app's state in S3, at s3Endpoint.
func tofuS3Files() map[string]forgetest.File {
	files := tofuFiles()
	files["infra/app/main.tf"] = forgetest.File{Content: `terraform {
  backend "s3" {
    bucket       = "states"
    key          = "app/terraform.tfstate"
    region       = "us-east-1"
    use_lockfile = true
    endpoints    = { s3 = "` + s3Endpoint + `" }
  }
}

variable "canary" {
  sensitive = true
}

module "cert" {
  source = "../modules/cert"
}
`}
	return files
}

// tofuS3Fixture is tofuApplyFixture with plan and apply roles in prod,
// which the STS stand-in answers, s3Endpoint in prod's s3_endpoints, and
// edits applied to the config.
func tofuS3Fixture(t *testing.T, runner *fakeTofu, edits ...func(string) string) *fixture {
	t.Helper()
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID": "AKIACONTROLPLANE0001", "AWS_SECRET_ACCESS_KEY": "control-plane-secret", "AWS_SESSION_TOKEN": "", "AWS_PROFILE": "",
		"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"), "AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(k, v)
	}
	st := ststest.New()
	t.Cleanup(st.Close)
	assumer, err := awsrole.NewSTS(context.Background(), awsrole.STSOptions{Region: "us-east-1", Endpoint: st.URL})
	if err != nil {
		t.Fatal(err)
	}
	return tofuApplyFixtureEdit(t, runner, func(text string) string {
		text = strings.Replace(text, "key = \"prod.key\"\n", fmt.Sprintf("key = \"prod.key\"\n  plan_role = %q\n  apply_role = %q\n  s3_endpoints = [%q]\n",
			"arn:aws:iam::222222222222:role/writ-plan", "arn:aws:iam::222222222222:role/writ-apply", s3Endpoint), 1)
		text += fmt.Sprintf("aws {\n  region = \"us-east-1\"\n  sts_endpoint = %q\n}\n", st.URL)
		for _, edit := range edits {
			text = edit(text)
		}
		return text
	}, func(o *server.Options) { o.Assumer = assumer })
}

// savedPlan is a saved plan whose state is serial serial of lineage, as
// tofu writes it: a zip with the state as its tfstate entry.
func savedPlan(t *testing.T, lineage string, serial int64) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create("tfstate")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, `{"version":4,"serial":%d,"lineage":%q,"resources":[]}`, serial, lineage)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// An apply of a stack on S3 claims the serial it pulled after tofu apply
// only when the apply succeeded, or began to change resources, and the
// serial is of its plan's lineage and above the serial its plan read. An
// apply that failed before it changed anything wrote nothing, and any
// other serial is not the apply's: it gets no sealed secrets and no
// index, and the next read judges it, as drift when no apply of writ
// serve wrote it.
func TestTofuS3ApplyClaims(t *testing.T) {
	stale := func(out []byte) error {
		return fmt.Errorf("%w: %w", tofu.ErrStale, &tofu.StepError{Step: "apply", Code: 1, Stdout: out})
	}
	for _, tc := range []struct {
		name   string
		pulled tofu.Serial
		err    func([]byte) error
		status string
		claim  bool
	}{
		// OpenTofu refused the saved plan: someone else wrote serial 9.
		{"a stale apply", tofu.Serial{Lineage: "L1", Serial: 9}, stale, "failed", false},
		{"a failed apply", tofu.Serial{Lineage: "L1", Serial: 5}, func(out []byte) error { return &tofu.StepError{Step: "apply", Code: 1, Stdout: out} }, "failed", false},
		{"an apply that left another lineage", tofu.Serial{Lineage: "L2", Serial: 5}, nil, "applied", false},
		{"an apply that wrote", tofu.Serial{Lineage: "L1", Serial: 5}, nil, "applied", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A first apply of writ serve writes serial 4 to the empty
			// state, and seals its secret set with it.
			var read atomic.Int64
			runner := &fakeTofu{
				plan: func() (tofu.Plan, error) {
					return tofu.Plan{File: savedPlan(t, "L1", read.Load()), JSON: []byte(tofuPlanJSON)}, nil
				},
				applyS3: func() (tofu.Applied, error) {
					read.Store(4)
					return tofu.Applied{Plan: []byte(tofuPlanJSON), State: tofu.State{Unmasked: []byte(tofuStateJSON), Serial: tofu.Serial{Lineage: "L1", Serial: 4}}}, nil
				},
			}
			f := tofuS3Fixture(t, runner)
			first, _ := f.planCommit(tofuS3Files(), "first")
			if r := f.do("POST", "/v1/plans/"+first.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
				t.Fatalf("bob's approval of the first plan: %d %s", r.status, r.body)
			}
			if run := f.tofuRun(bob(f), map[string]any{"plan": first.Plan.ID}); run.Run.Status != "applied" {
				t.Fatalf("the first run %+v\n%s", run.Run, run.Text)
			}
			runner.mu.Lock()
			runner.applyS3 = func() (tofu.Applied, error) {
				res := tofu.Applied{Plan: []byte(tofuPlanJSON), State: tofu.State{Unmasked: []byte(tofuStateJSON), Serial: tc.pulled}}
				if tc.err != nil {
					res.Log = []byte(`{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Saved plan is stale"}}` + "\n")
					return res, tc.err(res.Log)
				}
				return res, nil
			}
			runner.mu.Unlock()
			p, _ := f.planCommit(tofuS3Files(), "s3")
			if p.Plan.Status != "pending" {
				t.Fatalf("plan %s:\n%s", p.Plan.Status, p.Text)
			}
			if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
				t.Fatalf("bob's approval: %d %s", r.status, r.body)
			}
			run := f.tofuRun(bob(f), map[string]any{"plan": p.Plan.ID})
			if run.Run.Status != tc.status {
				t.Fatalf("run %+v\n%s", run.Run, run.Text)
			}
			ctx := context.Background()
			j, err := f.store.JudgeSerial(ctx, "prod", "app", tc.pulled.Lineage, tc.pulled.Serial)
			if err != nil {
				t.Fatal(err)
			}
			sealed, serr := f.store.SerialSecrets(ctx, "prod", "app", tc.pulled.Lineage, tc.pulled.Serial)
			ix, ierr := f.store.Index(ctx, "prod", "app")
			if !tc.claim {
				if j.Writer != control.SerialOutside || !errors.Is(serr, control.ErrNotFound) || ierr != nil || ix.Version != 4 {
					t.Fatalf("serial %d of %s after the apply: %+v, %d bytes of secrets %v, index of serial %d %v; want it unclaimed, with no secrets, and the index at serial 4",
						tc.pulled.Serial, tc.pulled.Lineage, j, len(sealed), serr, ix.Version, ierr)
				}
				return
			}
			if j != (control.Judged{Writer: control.SerialSeen, RunID: run.Run.ID}) || serr != nil || len(sealed) == 0 {
				t.Fatalf("serial %d after the apply: %+v, secrets %v; want the run's, with its secrets", tc.pulled.Serial, j, serr)
			}
			if mid, err := f.store.JudgeSerial(ctx, "prod", "app", "L1", 4); err != nil || mid.Writer != control.SerialSeen {
				t.Fatalf("the serial the plan read: %+v %v, want it seen", mid, err)
			}
			if ierr != nil || ix.Backend != control.BackendS3 || ix.Lineage != "L1" || ix.Version != 5 || len(ix.Rows) != len(tofuIndex) {
				t.Fatalf("the index after the apply: %+v %v", ix, ierr)
			}
		})
	}
}

// A refresh of a stack on S3 that fails still takes its turn, so a stack
// that always fails keeps none from the stacks after it.
func TestTofuS3RefreshTakesTurns(t *testing.T) {
	f := tofuApplyFixture(t, &fakeTofu{})
	ctx := context.Background()
	for _, stack := range []string{"app", "web"} {
		// No run of that id exists, so the refresh fails before it reads.
		v := control.StateSerial{Env: "prod", Stack: stack, Lineage: "L1", Serial: 2, From: 1, RunID: "NO-SUCH-RUN", SeenAt: time.Now()}
		if _, err := f.store.PutSerial(ctx, v, []byte("sealed")); err != nil {
			t.Fatal(err)
		}
	}
	before := time.Now().Add(-time.Second)
	f.srv.RefreshIndexes()
	rows, err := controltest.Raw(t, f.loc).QueryContext(ctx, "SELECT stack, checked_at FROM state_s3_stacks ORDER BY stack")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var stack string
		var at int64
		if err := rows.Scan(&stack, &at); err != nil {
			t.Fatal(err)
		}
		if at < before.UnixMilli() {
			t.Errorf("stack %s was checked at %d, before the refresh", stack, at)
		}
		got = append(got, stack)
	}
	if err := rows.Err(); err != nil || !slices.Equal(got, []string{"app", "web"}) {
		t.Fatalf("checked stacks %v %v", got, err)
	}
}

// tofuS3Applied is app@prod on S3 after applies applies: each plan reads
// the serial the state of lineage L is at, the first none, the first
// apply writes serial 5 and each later apply the next, and indexes it,
// and a pull reads the serial it is at. plan makes and approves a plan
// on branch, and apply applies one.
func tofuS3Applied(t *testing.T, applies int) (f *fixture, plan func(branch string) string, apply func(id string)) {
	t.Helper()
	serial := int64(0)
	state := func() tofu.State {
		return tofu.State{Serial: tofu.Serial{Lineage: "L", Serial: serial}, Unmasked: []byte(tofuStateJSON)}
	}
	f = tofuS3Fixture(t, &fakeTofu{
		plan: func() (tofu.Plan, error) {
			return tofu.Plan{File: savedPlan(t, "L", serial), JSON: []byte(tofuPlanJSON)}, nil
		},
		applyS3: func() (tofu.Applied, error) {
			serial = max(serial, 4) + 1
			return tofu.Applied{Plan: []byte(tofuPlanJSON), State: state()}, nil
		},
		pull: func() (tofu.State, error) { return state(), nil },
	})
	plan = func(branch string) string {
		t.Helper()
		p, _ := f.planCommit(tofuS3Files(), branch)
		if p.Plan.Status != "pending" {
			t.Fatalf("plan %s:\n%s", p.Plan.Status, p.Text)
		}
		if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
			t.Fatalf("bob's approval: %d %s", r.status, r.body)
		}
		return p.Plan.ID
	}
	apply = func(id string) {
		t.Helper()
		if r := f.tofuRun(bob(f), map[string]any{"plan": id}); r.Run.Status != "applied" {
			t.Fatalf("run %+v\n%s", r.Run, r.Text)
		}
	}
	for i := range applies {
		apply(plan(fmt.Sprintf("apply-%d", i)))
	}
	return f, plan, apply
}

// wantS3Index fails the test unless app@prod's index is of serial serial
// of lineage L.
func wantS3Index(t *testing.T, f *fixture, serial int64, after string) {
	t.Helper()
	ix, err := f.store.Index(context.Background(), "prod", "app")
	if err != nil || ix.Backend != control.BackendS3 || ix.Lineage != "L" || ix.Version != serial {
		t.Fatalf("the index %s: serial %d of %q %v, want serial %d of L", after, ix.Version, ix.Lineage, err, serial)
	}
}

// A refresh pulled serial 5, and an apply wrote and indexed serial 6
// while the refresh recorded its read. The refresh takes no place after
// the apply's, so it cannot replace serial 6's index with serial 5's.
func TestTofuS3RefreshOverlappedByApply(t *testing.T) {
	f, plan, apply := tofuS3Applied(t, 1)
	id := plan("second")
	since := time.Now()
	applied := false
	seq, err := f.srv.ObserveS3("prod", "app", tofu.Serial{Lineage: "L", Serial: 5}, since, func() time.Time {
		if !applied {
			applied = true
			apply(id)
			wantS3Index(t, f, 6, "after the second apply")
		}
		return time.Now()
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		if _, err := f.srv.FillS3Index("prod", "app", tofu.Serial{Lineage: "L", Serial: 5}, seq, []byte(tofuStateJSON)); err != nil {
			t.Fatal(err)
		}
		t.Errorf("the read the apply overlapped took place %d, want none", seq)
	}
	wantS3Index(t, f, 6, "after the overlapped refresh")
}

// A refresh read serial 5, then a plan read serial 6, and then the
// refresh stores its index. The plan's read is newer, so the refresh's
// index of serial 5 is dropped and the index keeps serial 6.
func TestTofuS3RefreshBeforeNewerPlanRead(t *testing.T) {
	f, plan, _ := tofuS3Applied(t, 2)
	wantS3Index(t, f, 6, "after two applies")
	// The read starts after the second apply ended, by the millisecond
	// the runs table counts in.
	time.Sleep(5 * time.Millisecond)
	slow := tofu.Serial{Lineage: "L", Serial: 5}
	seq, err := f.srv.ObserveS3("prod", "app", slow, time.Now(), time.Now)
	if err != nil || seq == 0 {
		t.Fatalf("the refresh's read: place %d %v", seq, err)
	}
	plan("concurrent")
	if stored, err := f.srv.FillS3Index("prod", "app", slow, seq, []byte(tofuStateJSON)); err != nil || stored {
		t.Errorf("the refresh's index after the plan's read: stored %v %v, want it dropped", stored, err)
	}
	wantS3Index(t, f, 6, "after the slow refresh")
}

// A refresh read serial 4, then a second refresh read serial 5 and found
// the index holds it, and then the first stores its index. The second
// read is newer, though it wrote nothing, so the first refresh's index is
// dropped.
func TestTofuS3RefreshBeforeSameIndexRefresh(t *testing.T) {
	f, _, _ := tofuS3Applied(t, 1)
	wantS3Index(t, f, 5, "after the apply")
	time.Sleep(5 * time.Millisecond)
	slow := tofu.Serial{Lineage: "L", Serial: 4}
	seq, err := f.srv.ObserveS3("prod", "app", slow, time.Now(), time.Now)
	if err != nil || seq == 0 {
		t.Fatalf("the first refresh's read: place %d %v", seq, err)
	}
	f.srv.RefreshIndexes()
	if stored, err := f.srv.FillS3Index("prod", "app", slow, seq, []byte(tofuStateJSON)); err != nil || stored {
		t.Errorf("the first refresh's index after the second's read: stored %v %v, want it dropped", stored, err)
	}
	wantS3Index(t, f, 5, "after the slow refresh")
}

// On S3 a plan's mask learns the secret set sealed with the serial the
// plan read, and an apply's mask the set of its plan's serial, which it
// seals on with the serial it writes. So after app's sensitive variable
// changed, the address that keeps the old value is masked in a plan, in
// the apply's events, and in a plan of the serial that apply wrote. A
// plan that tofu refuses, before writ serve knows the serial it read,
// masks with every set writ serve keeps for the stack. A serial with no
// set, as one written outside writ serve, keeps no detail of a plan.
func TestTofuS3PlanMasksPastSecrets(t *testing.T) {
	var serial atomic.Int64
	rotated := func() (tofu.Plan, error) {
		return tofu.Plan{File: savedPlan(t, "L", serial.Load()), JSON: []byte(rotatedPlanJSON)}, nil
	}
	runner := &fakeTofu{
		plan: func() (tofu.Plan, error) {
			return tofu.Plan{File: savedPlan(t, "L", serial.Load()), JSON: []byte(tofuPlanJSON)}, nil
		},
		applyS3: func() (tofu.Applied, error) {
			return tofu.Applied{Plan: []byte(tofuPlanJSON), State: tofu.State{Serial: tofu.Serial{Lineage: "L", Serial: serial.Add(1)}, Unmasked: []byte(tofuStateJSON)}}, nil
		},
	}
	f := tofuS3Fixture(t, runner)
	planned := func(branch string) server.RemotePlan {
		t.Helper()
		p, _ := f.planCommit(tofuS3Files(), branch)
		if body, _ := json.Marshal(p); strings.Contains(string(body), tofuCanary) {
			t.Errorf("the plan at %s holds the old value:\n%s", branch, body)
		}
		return p
	}
	applied := func(id string) server.RemoteRun {
		t.Helper()
		if r := f.do("POST", "/v1/plans/"+id+"/approvals", bob(f), nil); r.status != http.StatusOK {
			t.Fatalf("bob's approval: %d %s", r.status, r.body)
		}
		run := f.tofuRun(bob(f), map[string]any{"plan": id})
		if body, _ := json.Marshal(run); run.Run.Status != "applied" || strings.Contains(string(body), tofuCanary) {
			t.Fatalf("run %+v\n%s", run.Run, body)
		}
		return run
	}
	// The first apply writes serial 1 to the empty state, and seals the
	// old value with it.
	applied(planned("first").Plan.ID)

	want := []string{`terraform_data.keyed["(sensitive)"] create`, `terraform_data.keyed["(sensitive)"] delete`}
	runner.mu.Lock()
	runner.plan = rotated
	runner.applyS3 = func() (tofu.Applied, error) {
		return tofu.Applied{Plan: []byte(rotatedPlanJSON), State: tofu.State{Serial: tofu.Serial{Lineage: "L", Serial: serial.Add(1)}, Unmasked: []byte(tofuStateJSON)},
			Log: []byte(`{"type":"apply_complete","hook":{"resource":{"addr":"terraform_data.keyed[\"` + tofuCanary + `\"]"},"action":"delete","elapsed_seconds":0}}` + "\n")}, nil
	}
	runner.mu.Unlock()
	p := planned("rotate")
	if got := keyedChanges(t, p.Document); p.Plan.Status != "pending" || !slices.Equal(got, want) {
		t.Fatalf("the plan of serial 1 after the variable changed: %s, changes %q\n%s", p.Plan.Status, got, p.Text)
	}
	run := applied(p.Plan.ID)
	if ev := run.Document; !strings.Contains(string(ev), `terraform_data.keyed[\"(sensitive)\"]`) {
		t.Fatalf("the apply's events: %s", ev)
	}
	if p := planned("after"); p.Plan.Status != "pending" || !slices.Equal(keyedChanges(t, p.Document), want) {
		t.Fatalf("the plan of serial 2, which the apply wrote: %s\n%s", p.Plan.Status, p.Text)
	}

	runner.mu.Lock()
	runner.plan = func() (tofu.Plan, error) {
		out := []byte(`{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Cannot delete terraform_data.keyed[\"` + tofuCanary + `\"]"}}` + "\n")
		return tofu.Plan{}, &tofu.StepError{Step: "plan", Code: 1, Stdout: out}
	}
	runner.mu.Unlock()
	if p := planned("refused"); p.Plan.Status != "refused" || p.Text != "writ: tofu plan exited with code 1\nerror: Cannot delete terraform_data.keyed[\"(sensitive)\"]\n" {
		t.Fatalf("a plan tofu refused: %s\n%s", p.Plan.Status, p.Text)
	}

	// Someone outside writ serve writes serial 3.
	serial.Add(1)
	runner.mu.Lock()
	runner.plan = rotated
	runner.mu.Unlock()
	p = planned("outside")
	if p.Plan.Status != "failed" || !strings.Contains(p.Text, "no secret set for state version 3 of app@prod") || len(keyedChanges(t, p.Document)) != 0 {
		t.Fatalf("a plan of a serial written outside writ serve: %s\n%s\n%s", p.Plan.Status, p.Text, p.Document)
	}
	if j, err := f.store.JudgeSerial(context.Background(), "prod", "app", "L", 3); err != nil || j.Writer != control.SerialSeen {
		t.Fatalf("serial 3 after the plan read it: %+v %v, want it recorded", j, err)
	}
	scanForCanaries(t, f, tofuCanary, tofuRotated, tofuEnvSecret)
}

// A plan of a stack on S3 that tofu refused wrote no saved plan, so writ
// serve cannot tell which serial it read: the serial writ serve read last
// may be one written outside writ serve, with no set, or of an older
// lineage, whose set lacks the current lineage's secrets. Its mask knows
// every set writ serve keeps for the stack, of every lineage, and the
// refusal keeps each diagnostic's summary and place swept by it, with each
// quoted instance key swept whole. So a provider's AccessDenied shows,
// and no secret of any set does.
func TestTofuS3RefusedPlanMasksEverySet(t *testing.T) {
	for _, tc := range []struct {
		name string
		// last is the serial a plan reads after the second lineage's
		// first apply, none when no plan does.
		last tofu.Serial
	}{
		{"after an apply of writ serve", tofu.Serial{}},
		{"after a serial written outside writ serve", tofu.Serial{Lineage: "new", Serial: 2}},
		{"after a read of an older lineage", tofu.Serial{Lineage: "old", Serial: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			lineage, serial, secret := "old", int64(0), tofuCanary
			planJSON := func() []byte { return []byte(strings.ReplaceAll(tofuPlanJSON, tofuCanary, secret)) }
			runner := &fakeTofu{
				plan: func() (tofu.Plan, error) {
					mu.Lock()
					defer mu.Unlock()
					return tofu.Plan{File: savedPlan(t, lineage, serial), JSON: planJSON()}, nil
				},
				applyS3: func() (tofu.Applied, error) {
					mu.Lock()
					defer mu.Unlock()
					serial++
					return tofu.Applied{Plan: planJSON(), State: tofu.State{Serial: tofu.Serial{Lineage: lineage, Serial: serial}, Unmasked: []byte(tofuStateJSON)}}, nil
				},
			}
			f := tofuS3Fixture(t, runner)
			applied := func(branch string) {
				t.Helper()
				p, _ := f.planCommit(tofuS3Files(), branch)
				if p.Plan.Status != "pending" {
					t.Fatalf("the plan at %s: %s\n%s", branch, p.Plan.Status, p.Text)
				}
				if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
					t.Fatalf("bob's approval: %d %s", r.status, r.body)
				}
				if run := f.tofuRun(bob(f), map[string]any{"plan": p.Plan.ID}); run.Run.Status != "applied" {
					t.Fatalf("the run of %s: %+v\n%s", branch, run.Run, run.Text)
				}
			}
			// Serial 1 of lineage old is sealed with tofuCanary, and serial
			// 1 of lineage new, which replaced it, with tofuRotated.
			applied("old")
			mu.Lock()
			lineage, serial, secret = "new", 0, tofuRotated
			mu.Unlock()
			applied("new")
			if tc.last.Serial > 0 {
				mu.Lock()
				lineage, serial = tc.last.Lineage, tc.last.Serial
				mu.Unlock()
				f.planCommit(tofuS3Files(), "read")
				if j, err := f.store.JudgeSerial(context.Background(), "prod", "app", tc.last.Lineage, tc.last.Serial); err != nil || j.Writer != control.SerialSeen {
					t.Fatalf("serial %+v after a plan read it: %+v %v", tc.last, j, err)
				}
			}

			runner.mu.Lock()
			runner.plan = func() (tofu.Plan, error) {
				out := []byte(`{"type":"diagnostic","diagnostic":{"severity":"error","summary":"reading ACM Certificate (` + tofuCanary + `): AccessDeniedException: not authorized to perform: acm:GetCertificate","detail":"plan role","range":{"filename":"main.tf","start":{"line":12,"column":3}}}}
{"type":"diagnostic","diagnostic":{"severity":"error","summary":"tag ` + tofuRotated + ` is in use by keyed[\"` + tofuCanary + `\"]"}}
`)
				return tofu.Plan{}, &tofu.StepError{Step: "plan", Code: 1, Stdout: out}
			}
			runner.mu.Unlock()
			p, _ := f.planCommit(tofuS3Files(), "refused")
			wantRefused(t, p, `writ: tofu plan exited with code 1
error: main.tf:12:3: reading ACM Certificate ((sensitive)): AccessDeniedException: not authorized to perform: acm:GetCertificate
error: tag (sensitive) is in use by keyed["(sensitive)"]
`, "tofu plan exited with code 1",
				"main.tf:12:3: reading ACM Certificate ((sensitive)): AccessDeniedException: not authorized to perform: acm:GetCertificate",
				`tag (sensitive) is in use by keyed["(sensitive)"]`)
			scanForCanaries(t, f, tofuCanary, tofuRotated, tofuEnvSecret)
		})
	}
}

// A stack on S3 that writ serve never applied or adopted has no secret
// set, so a refused plan's mask knows only the run's own secrets. The
// state can still keep a past secret where nothing marks it, as a key of
// nonsensitive(), so each quoted instance key is swept whole, and the rest
// of each summary, such as the action an AccessDenied names, shows.
func TestTofuS3RefusedPlanWithNoSet(t *testing.T) {
	const pastKey = "canary-past-key-of-state-6f10"
	runner := &fakeTofu{plan: func() (tofu.Plan, error) {
		out := []byte(`{"type":"diagnostic","diagnostic":{"severity":"error","summary":"reading ACM Certificate: AccessDeniedException: not authorized to perform: acm:GetCertificate","detail":"` + tofuCanary + `","range":{"filename":"main.tf","start":{"line":12,"column":3}},"snippet":{"code":"` + tofuCanary + `"}}}
{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Cannot delete module.m[\"` + pastKey + `\"].terraform_data.keyed[0]"}}
{"type":"diagnostic","diagnostic":{"severity":"warning","summary":"Provider failed with ` + tofuEnvSecret + `"}}
`)
		return tofu.Plan{}, &tofu.StepError{Step: "plan", Code: 1, Stdout: out}
	}}
	f := tofuS3Fixture(t, runner)
	p, _ := f.planCommit(tofuS3Files(), "refused")
	wantRefused(t, p, `writ: tofu plan exited with code 1
error: main.tf:12:3: reading ACM Certificate: AccessDeniedException: not authorized to perform: acm:GetCertificate
error: Cannot delete module.m["(sensitive)"].terraform_data.keyed[0]
warning: Provider failed with (sensitive)
`, "tofu plan exited with code 1",
		"main.tf:12:3: reading ACM Certificate: AccessDeniedException: not authorized to perform: acm:GetCertificate",
		`Cannot delete module.m["(sensitive)"].terraform_data.keyed[0]`,
		"Provider failed with (sensitive)")
	scanForCanaries(t, f, pastKey, tofuCanary, tofuEnvSecret)
}

// wantRefused fails the test unless p is refused with exactly text, and
// its document with exactly the diagnostic messages want.
func wantRefused(t *testing.T, p server.RemotePlan, text string, want ...string) {
	t.Helper()
	var doc struct{ Diagnostics []server.Diagnostic }
	if err := json.Unmarshal(p.Document, &doc); err != nil || p.Plan.Status != "refused" || p.Text != text {
		t.Fatalf("a plan tofu refused: %s %v\n%s\n%s", p.Plan.Status, err, p.Text, p.Document)
	}
	var got []string
	for _, d := range doc.Diagnostics {
		got = append(got, d.Message)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the refused plan's diagnostics %q, want %q", got, want)
	}
}

// An apply of a stack on S3 that failed after it began to change
// resources wrote a partial state under the state's lock. It claims that
// serial and seals with it the secret set it carried, as a whole apply
// does, so the next plan masks the state's past secrets and may be
// approved, instead of failing for want of a set until someone rewrites
// the state.
func TestTofuS3PartialApplyCarriesSecretSet(t *testing.T) {
	var serial atomic.Int64
	plan := func(js string) func() (tofu.Plan, error) {
		return func() (tofu.Plan, error) {
			return tofu.Plan{File: savedPlan(t, "L", serial.Load()), JSON: []byte(js)}, nil
		}
	}
	runner := &fakeTofu{
		plan: plan(tofuPlanJSON),
		applyS3: func() (tofu.Applied, error) {
			return tofu.Applied{Plan: []byte(tofuPlanJSON), State: tofu.State{Serial: tofu.Serial{Lineage: "L", Serial: serial.Add(1)}, Unmasked: []byte(tofuStateJSON)}}, nil
		},
	}
	f := tofuS3Fixture(t, runner)
	planned := func(branch string) server.RemotePlan {
		t.Helper()
		p, _ := f.planCommit(tofuS3Files(), branch)
		if p.Plan.Status != "pending" {
			t.Fatalf("the plan at %s: %s\n%s", branch, p.Plan.Status, p.Text)
		}
		if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
			t.Fatalf("bob's approval: %d %s", r.status, r.body)
		}
		return p
	}
	// The first apply writes serial 1, and seals tofuCanary with it.
	if run := f.tofuRun(bob(f), map[string]any{"plan": planned("first").Plan.ID}); run.Run.Status != "applied" {
		t.Fatalf("the first run %+v\n%s", run.Run, run.Text)
	}

	runner.mu.Lock()
	runner.plan = plan(rotatedPlanJSON)
	runner.applyS3 = func() (tofu.Applied, error) {
		log := []byte(`{"type":"apply_start","hook":{"resource":{"addr":"terraform_data.keyed[\"` + tofuRotated + `\"]"},"action":"create"}}
{"type":"apply_complete","hook":{"resource":{"addr":"terraform_data.keyed[\"` + tofuRotated + `\"]"},"action":"create","elapsed_seconds":0}}
{"type":"apply_start","hook":{"resource":{"addr":"terraform_data.keyed[\"` + tofuCanary + `\"]"},"action":"delete"}}
{"type":"apply_errored","hook":{"resource":{"addr":"terraform_data.keyed[\"` + tofuCanary + `\"]"},"action":"delete","elapsed_seconds":0}}
{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Delete failed"}}
`)
		return tofu.Applied{Plan: []byte(rotatedPlanJSON), Log: log, State: tofu.State{Serial: tofu.Serial{Lineage: "L", Serial: serial.Add(1)}, Unmasked: []byte(tofuStateJSON)}},
			&tofu.StepError{Step: "apply", Code: 1, Stdout: log}
	}
	runner.mu.Unlock()
	run := f.tofuRun(bob(f), map[string]any{"plan": planned("rotate").Plan.ID})
	if run.Run.Status != "failed" {
		t.Fatalf("the partial run %+v\n%s", run.Run, run.Text)
	}
	ctx := context.Background()
	if j, err := f.store.JudgeSerial(ctx, "prod", "app", "L", 2); err != nil || j != (control.Judged{Writer: control.SerialSeen, RunID: run.Run.ID}) {
		t.Fatalf("serial 2 after the partial apply: %+v %v, want the run's", j, err)
	}
	if _, err := f.store.SerialSecrets(ctx, "prod", "app", "L", 2); err != nil {
		t.Fatalf("the secret set of serial 2: %v", err)
	}
	if ix, err := f.store.Index(ctx, "prod", "app"); err != nil || ix.Version != 2 {
		t.Fatalf("the index after the partial apply: %+v %v", ix, err)
	}

	// The delete that failed is still to do, and its address holds
	// tofuCanary, which only serial 2's set names now.
	want := []string{`terraform_data.keyed["(sensitive)"] create`, `terraform_data.keyed["(sensitive)"] delete`}
	p := planned("recover")
	if got := keyedChanges(t, p.Document); !slices.Equal(got, want) {
		t.Fatalf("the plan of serial 2: changes %q, want %q\n%s", got, want, p.Text)
	}
	scanForCanaries(t, f, tofuCanary, tofuRotated, tofuEnvSecret)
}

// servedCreds fetches the role credentials of stk from the credential
// endpoint, as a provider does.
func servedCreds(t *testing.T, stk tofu.Stack) (secret, token string) {
	t.Helper()
	req, err := http.NewRequest("GET", envValue(stk.Creds, "AWS_CONTAINER_CREDENTIALS_FULL_URI"), nil)
	if err != nil {
		t.Error(err)
		return "", ""
	}
	req.Header.Set("Authorization", envValue(stk.Creds, "AWS_CONTAINER_AUTHORIZATION_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return "", ""
	}
	defer resp.Body.Close()
	var doc struct{ SecretAccessKey, Token string }
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || doc.SecretAccessKey == "" || doc.Token == "" {
		t.Errorf("the credential endpoint answered %d %+v %v", resp.StatusCode, doc, err)
	}
	return doc.SecretAccessKey, doc.Token
}

// echoed is a -json stream that quotes secret and token as a provider's
// error and a resource address can.
func echoed(secret, token string) []byte {
	diag, _ := json.Marshal(map[string]any{"type": "diagnostic", "diagnostic": map[string]any{"severity": "error",
		"summary": "provider rejected " + secret, "detail": "session " + token}})
	warn, _ := json.Marshal(map[string]any{"type": "diagnostic", "diagnostic": map[string]any{"severity": "warning",
		"summary": "retrying with " + secret}})
	ev, _ := json.Marshal(map[string]any{"type": "apply_errored", "hook": map[string]any{"resource": map[string]any{"addr": `random_id.i["` + token + `"]`}, "action": "create"}})
	return slices.Concat(diag, []byte("\n"), warn, []byte("\n"), ev, []byte("\n"))
}

// The role credentials the broker serves a run are known to the run's
// mask, though the run's environment holds only the token that fetches
// them: a refused plan, a plan, or a failed apply whose provider quotes
// them keeps none of them.
func TestTofuMasksServedCredentials(t *testing.T) {
	var mu sync.Mutex
	var served []string
	serve := func(r *fakeTofu, applied bool) (string, string) {
		r.mu.Lock()
		stk := r.stacks[len(r.stacks)-1]
		if applied {
			stk = r.applied[len(r.applied)-1].stack
		}
		r.mu.Unlock()
		secret, token := servedCreds(t, stk)
		mu.Lock()
		served = append(served, secret, token)
		mu.Unlock()
		return secret, token
	}
	leaks := func(t *testing.T, what string, docs ...string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		for _, d := range docs {
			for _, s := range served {
				if strings.Contains(d, s) {
					t.Fatalf("%s keeps served credential %q:\n%s", what, s, d)
				}
			}
		}
	}

	t.Run("a refused plan", func(t *testing.T) {
		r := &fakeTofu{}
		r.plan = func() (tofu.Plan, error) {
			out := echoed(serve(r, false))
			return tofu.Plan{}, &tofu.StepError{Step: "plan", Code: 1, Stdout: out}
		}
		f := tofuS3Fixture(t, r)
		p, _ := f.planCommit(tofuS3Files(), "refused")
		if p.Plan.Status != string(control.RefusedPlan) || !strings.Contains(p.Text, "provider rejected") {
			t.Fatalf("plan %s:\n%s", p.Plan.Status, p.Text)
		}
		leaks(t, "the refused plan", p.Text, string(p.Document))
	})

	t.Run("a plan", func(t *testing.T) {
		r := &fakeTofu{}
		r.plan = func() (tofu.Plan, error) {
			secret, _ := serve(r, false)
			log, _ := json.Marshal(map[string]any{"type": "diagnostic", "diagnostic": map[string]any{"severity": "warning", "summary": "retrying with " + secret}})
			return tofu.Plan{File: savedPlan(t, "L1", 0), JSON: []byte(tofuPlanJSON), Log: append(log, '\n')}, nil
		}
		f := tofuS3Fixture(t, r)
		p, _ := f.planCommit(tofuS3Files(), "plan")
		if p.Plan.Status != "pending" || !strings.Contains(p.Text, "retrying with") {
			t.Fatalf("plan %s:\n%s", p.Plan.Status, p.Text)
		}
		leaks(t, "the plan", p.Text, string(p.Document))
	})

	t.Run("a failed apply", func(t *testing.T) {
		r := &fakeTofu{plan: func() (tofu.Plan, error) {
			return tofu.Plan{File: savedPlan(t, "L1", 0), JSON: []byte(tofuPlanJSON)}, nil
		}}
		r.applyS3 = func() (tofu.Applied, error) {
			out := echoed(serve(r, true))
			return tofu.Applied{Plan: []byte(tofuPlanJSON), Log: out}, &tofu.StepError{Step: "apply", Code: 1, Stdout: out}
		}
		f := tofuS3Fixture(t, r)
		p, _ := f.planCommit(tofuS3Files(), "apply")
		if res := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); res.status != http.StatusOK {
			t.Fatalf("bob's approval: %d %s", res.status, res.body)
		}
		run := f.tofuRun(bob(f), map[string]any{"plan": p.Plan.ID})
		if run.Run.Status != "failed" || !strings.Contains(string(run.Document), "provider rejected") || !strings.Contains(string(run.Document), "apply_errored") {
			t.Fatalf("run %+v\n%s\n%s", run.Run, run.Text, run.Document)
		}
		leaks(t, "the failed apply", run.Text, string(run.Document))
	})
}

// A secret of provider_env that spans lines is masked whole in a refused
// plan's diagnostics, which fold newlines.
func TestTofuMasksMultilineSecret(t *testing.T) {
	const secret = "canary-multiline-first\ncanary-multiline-second"
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &fakeTofu{plan: func() (tofu.Plan, error) {
		out, _ := json.Marshal(map[string]any{"type": "diagnostic", "diagnostic": map[string]any{"severity": "error", "summary": "provider rejected " + secret}})
		return tofu.Plan{}, &tofu.StepError{Step: "plan", Code: 1, Stdout: out}
	}}
	f := tofuS3Fixture(t, r, func(text string) string {
		return strings.Replace(text, "provider_env = {", "provider_env = { TF_VAR_multiline = \""+file+"\",", 1)
	})
	p, _ := f.planCommit(tofuS3Files(), "multiline")
	if !strings.Contains(p.Text, "provider rejected") {
		t.Fatalf("plan %s:\n%s", p.Plan.Status, p.Text)
	}
	for _, half := range strings.Split(secret, "\n") {
		if strings.Contains(p.Text, half) || strings.Contains(string(p.Document), half) {
			t.Fatalf("the plan keeps %q:\n%s\n%s", half, p.Text, p.Document)
		}
	}
}

func TestMergedS3RetryAfterOutsideWrite(t *testing.T) {
	var serial atomic.Int64
	state := func() tofu.State {
		return tofu.State{Unmasked: []byte(tofuStateJSON), Serial: tofu.Serial{Lineage: "L1", Serial: serial.Load()}}
	}
	runner := &fakeTofu{
		plan: func() (tofu.Plan, error) {
			return tofu.Plan{File: savedPlan(t, "L1", serial.Load()), JSON: []byte(tofuPlanJSON)}, nil
		},
		applyS3: func() (tofu.Applied, error) {
			serial.Store(4)
			return tofu.Applied{Plan: []byte(tofuPlanJSON), State: state()}, nil
		},
		pull: func() (tofu.State, error) { return state(), nil },
	}
	f := tofuS3Fixture(t, runner, func(config string) string {
		return strings.Replace(config, `can = ["read", "apply"]
  risks = ["create"]`, `can = ["read", "apply"]
  risks = ["create", "update", "replace", "delete", "data-loss"]`, 1)
	})
	files := tofuS3Files()
	p, _ := f.planCommit(files, "s3")
	if p.Plan.Status != "pending" {
		t.Fatalf("plan: %s\n%s", p.Plan.Status, p.Text)
	}
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
		t.Fatalf("approve: %d %s", r.status, r.body)
	}
	sha := f.gitlab.Commit(4242, "main", files)
	main := mainJob(f, sha)
	r := f.do("POST", "/v1/runs", main, mergedRequest("app"))
	if r.status != http.StatusAccepted {
		t.Fatalf("apply: %d %s", r.status, r.body)
	}
	if run := f.waitRun(main, r.doc["run"].(map[string]any)["id"].(string)); run.Run.Status != "applied" {
		t.Fatalf("run: %+v", run.Run)
	}

	serial.Store(5)
	r = f.do("POST", "/v1/runs", mainJob(f, sha), mergedRequest("app"))
	if r.status != http.StatusConflict || !strings.Contains(string(r.body), "plan again") {
		t.Fatalf("retry after outside write: %d %s", r.status, r.body)
	}
}
