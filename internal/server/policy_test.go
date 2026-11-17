package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/policy"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// policyFixture is tofuApplyFixture with one policy set, base, whose one
// file is rego.
func policyFixture(t *testing.T, runner *fakeTofu, rego string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	return tofuApplyFixtureEdit(t, runner, policyConfig(t, rego), setup...)
}

// policyConfig adds to a config one policy set, base, whose one file is
// rego.
func policyConfig(t *testing.T, rego string) func(string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "base.rego"), []byte("package main\n\nimport rego.v1\n\n"+rego), 0o644); err != nil {
		t.Fatal(err)
	}
	return func(text string) string {
		return text + fmt.Sprintf("policy {\n  timeout = \"1m\"\n  set \"base\" {\n    dir = %q\n  }\n}\n", dir)
	}
}

func decisionOf(t *testing.T, raw json.RawMessage) policy.Decision {
	t.Helper()
	var d policy.Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("the decision %s: %v", raw, err)
	}
	return d
}

// A plan that a policy denies at the plan point is refused, with the
// decision and its masked messages kept with it and in its text, and
// keeps no saved plan, so no approval or apply can use it.
func TestTofuPolicyRefusesPlan(t *testing.T) {
	runner := &fakeTofu{}
	f := policyFixture(t, runner, `deny_replace contains msg if {
	some rc in input.resource_changes
	"delete" in rc.change.actions
	"create" in rc.change.actions
	msg := sprintf("%s is replaced in %s, planned by %s", [rc.address, input.writ.environment, input.writ.actor.display])
}

warn contains sprintf("keepers %v", [rc.change.after.keepers]) if {
	some rc in input.resource_changes
	rc.type == "random_pet"
}

warn contains "the provider env holds `+tofuEnvSecret+`" if input.writ.decision == "plan"
`)
	p, author := f.planCommit(tofuFiles(), "replace")
	if p.OK || p.Plan.Status != "refused" || p.Plan.ID == "" {
		t.Fatalf("plan %+v\n%s", p.Plan, p.Text)
	}
	d := decisionOf(t, p.Policy)
	// writ mcp cuts these lines from the text and lists the decision first.
	if !strings.HasSuffix(p.Text, server.PolicyText(d)) {
		t.Errorf("the plan's text does not end with its decision's lines:\n%s", p.Text)
	}
	want := []policy.Result{
		{Set: "base", Level: policy.Mandatory, Rule: "deny_replace", Outcome: policy.Deny, Message: "module.cert.random_id.i is replaced in prod, planned by " + p.Plan.Author.Display},
		{Set: "base", Level: policy.Mandatory, Rule: "warn", Outcome: policy.Warn, Message: `keepers {"c": "(sensitive)"}`},
		{Set: "base", Level: policy.Mandatory, Rule: "warn", Outcome: policy.Warn, Message: "the provider env holds (sensitive)"},
	}
	slices.SortFunc(d.Results, func(a, b policy.Result) int { return strings.Compare(a.Rule+a.Message, b.Rule+b.Message) })
	if d.Point != policy.PlanPoint || d.Outcome != policy.Deny || !slices.Equal(d.Results, want) {
		t.Fatalf("the decision %+v\nwant results %+v", d, want)
	}
	for _, line := range []string{"\nPolicy: denied, so this plan cannot be approved or applied.\n", "  deny: [base] module.cert.random_id.i is replaced in prod, planned by ", "  warn: [base] the provider env holds (sensitive)\n"} {
		if !strings.Contains(p.Text, line) {
			t.Errorf("the plan's text lacks %q:\n%s", line, p.Text)
		}
	}
	if saved, _, err := f.store.SavedPlan(context.Background(), p.Plan.ID); err != nil || saved != nil {
		t.Fatalf("the refused plan keeps a saved plan: %q, %v", saved, err)
	}
	if got := f.do("GET", "/v1/plans/"+p.Plan.ID, author, nil); got.status != http.StatusOK || !strings.Contains(string(got.body), `"outcome":"deny"`) {
		t.Fatalf("GET of the refused plan: %d %s", got.status, got.body)
	}
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status == http.StatusOK {
		t.Fatalf("an approval of the refused plan: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": p.Plan.ID})); r.status != http.StatusConflict {
		t.Fatalf("an apply of the refused plan: %d %s", r.status, r.body)
	}
	if len(runner.applied) != 0 {
		t.Fatal("OpenTofu applied a plan a policy refused")
	}
	scanForCanaries(t, f, tofuCanary, tofuEnvSecret)
}

// A policy that fails denies: the plan is refused with the error, at a
// mandatory set's level.
func TestTofuPolicyErrorRefusesPlan(t *testing.T) {
	f := policyFixture(t, &fakeTofu{}, "deny contains \"never\" if to_number(\"seven\") > 1\n")
	p, _ := f.planCommit(tofuFiles(), "error")
	d := decisionOf(t, p.Policy)
	if p.Plan.Status != "refused" || d.Outcome != policy.Deny || len(d.Results) != 1 || d.Results[0].Rule != "error" || !strings.Contains(d.Results[0].Message, "to_number") {
		t.Fatalf("plan %s, decision %+v", p.Plan.Status, d)
	}
}

// The apply point judges the plan again, for the person who asks for the
// apply and with the approvals that count. A deny there refuses the
// request with each masked message, starts no run, and leaves the plan
// approved; a warning lets the run begin and is kept with it.
func TestTofuPolicyJudgesApply(t *testing.T) {
	rule := func(name string) string {
		return name + ` contains msg if {
	input.writ.decision == "apply"
	msg := sprintf("%s applied by %s with %d approvals, near ` + tofuEnvSecret + `", [input.writ.stack, input.writ.actor.display, count(input.writ.approvals)])
}
`
	}
	t.Run("a deny refuses the apply", func(t *testing.T) {
		runner := &fakeTofu{}
		f := policyFixture(t, runner, rule("deny"))
		id := approvedTofuPlan(t, f, "deny-at-apply")
		p := f.do("GET", "/v1/plans/"+id, bob(f), nil)
		var plan struct {
			Plan   struct{ Status string } `json:"plan"`
			Policy policy.Decision         `json:"policy"`
		}
		if err := json.Unmarshal(p.body, &plan); err != nil || plan.Plan.Status != "approved" || plan.Policy.Outcome != policy.Pass {
			t.Fatalf("the plan point passes the rule, which reads only the apply point: %s", p.body)
		}
		r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
		if r.status != http.StatusForbidden || r.class() != "policy" {
			t.Fatalf("the apply: %d %s", r.status, r.body)
		}
		if !strings.Contains(string(r.body), "policy refuses the apply of plan "+id) || !strings.Contains(string(r.body), "deny: [base] app applied by ") ||
			!strings.Contains(string(r.body), "with 1 approvals, near (sensitive)") || strings.Contains(string(r.body), tofuEnvSecret) {
			t.Fatalf("the refusal: %s", r.body)
		}
		if len(runner.applied) != 0 {
			t.Fatal("OpenTofu applied a plan the apply point refused")
		}
		if after := f.do("GET", "/v1/plans/"+id, bob(f), nil); !strings.Contains(string(after.body), `"status":"approved"`) {
			t.Fatalf("the plan after the refusal: %s", after.body)
		}
		scanForCanaries(t, f, tofuCanary, tofuEnvSecret)
	})
	t.Run("a warning is kept with the run", func(t *testing.T) {
		runner := &fakeTofu{}
		f := policyFixture(t, runner, rule("warn"))
		id := approvedTofuPlan(t, f, "warn-at-apply")
		run := f.tofuRun(bob(f), map[string]any{"plan": id})
		if run.Run.Status != "applied" {
			t.Fatalf("run %+v\n%s", run.Run, run.Text)
		}
		served := f.do("GET", "/v1/runs/"+run.Run.ID, bob(f), nil)
		var got struct {
			Policy policy.Decision `json:"policy"`
		}
		if err := json.Unmarshal(served.body, &got); err != nil {
			t.Fatal(err)
		}
		d := got.Policy
		if d.Point != policy.ApplyPoint || d.Outcome != policy.Warn || len(d.Results) != 1 ||
			!strings.HasPrefix(d.Results[0].Message, "app applied by ") || !strings.HasSuffix(d.Results[0].Message, " with 1 approvals, near (sensitive)") {
			t.Fatalf("the run's decision %+v", d)
		}
		scanForCanaries(t, f, tofuCanary, tofuEnvSecret)
	})
}

// A state keeps a past secret where nothing marks it, and the apply
// point's mask knows it as the plan's mask did, from the secret set
// sealed with the state version the plan read, or on S3 with the serial
// its saved plan holds. app's first apply sealed tofuCanary with the
// state it wrote, and the plan after its variable changed deletes the
// resource keyed by it. A policy that writes tofuCanary into
// a message at the apply point has it swept from the refusal or the
// run's decision, the control database, the data directory, and the log.
func TestTofuPolicyApplyMasksPastSecrets(t *testing.T) {
	// The rule builds tofuCanary from two halves, as a policy can build
	// any value, so the policy's own text does not hold it.
	half := len(tofuCanary) / 2
	rule := func(name string) string {
		return fmt.Sprintf(`%s contains msg if {
	input.writ.decision == "apply"
	some rc in input.resource_changes
	rc.type == "terraform_data"
	"delete" in rc.change.actions
	msg := sprintf("%%s deletes the key %%s", [rc.address, concat("", [%q, %q])])
}
`, name, tofuCanary[:half], tofuCanary[half:])
	}
	rotated := func(t *testing.T, name string) (*fakeTofu, *fixture, *syncBuffer, string) {
		runner := &fakeTofu{}
		var log syncBuffer
		f := policyFixture(t, runner, rule(name), func(o *server.Options) {
			o.Log = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
		})
		appliedApp(t, f, "add-cert")
		runner.mu.Lock()
		runner.plan = func() (tofu.Plan, error) {
			return tofu.Plan{File: []byte("saved rotated plan"), JSON: []byte(rotatedPlanJSON)}, nil
		}
		runner.mu.Unlock()
		return runner, f, &log, approvedTofuPlan(t, f, "rotate")
	}
	leaks := func(t *testing.T, f *fixture, log *syncBuffer, answers ...[]byte) {
		t.Helper()
		for i, a := range answers {
			if strings.Contains(string(a), tofuCanary) {
				t.Errorf("answer %d holds the past secret: %s", i, a)
			}
		}
		if strings.Contains(log.String(), tofuCanary) {
			t.Errorf("the log holds the past secret:\n%s", log.String())
		}
		scanForCanaries(t, f, tofuCanary, tofuRotated, tofuEnvSecret)
	}
	const want = `deletes the key (sensitive)`
	t.Run("a deny", func(t *testing.T) {
		runner, f, log, id := rotated(t, "deny")
		n := len(runner.applied)
		r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
		leaks(t, f, log, r.body)
		if r.status != http.StatusForbidden || r.class() != "policy" || !strings.Contains(string(r.body), want) {
			t.Errorf("the apply: %d %s", r.status, r.body)
		}
		if len(runner.applied) != n {
			t.Error("OpenTofu applied a plan the apply point refused")
		}
	})
	t.Run("a warning", func(t *testing.T) {
		_, f, log, id := rotated(t, "warn")
		run := f.tofuRun(bob(f), map[string]any{"plan": id})
		served := f.do("GET", "/v1/runs/"+run.Run.ID, bob(f), nil)
		var got struct {
			Policy policy.Decision `json:"policy"`
		}
		leaks(t, f, log, served.body)
		if err := json.Unmarshal(served.body, &got); err != nil {
			t.Fatal(err)
		}
		if d := got.Policy; d.Point != policy.ApplyPoint || d.Outcome != policy.Warn || len(d.Results) != 1 || !strings.HasSuffix(d.Results[0].Message, want) {
			t.Errorf("the run's decision %+v", d)
		}
	})
	t.Run("a deny on S3", func(t *testing.T) {
		var serial atomic.Int64
		runner := &fakeTofu{
			plan: func() (tofu.Plan, error) {
				return tofu.Plan{File: savedPlan(t, "L", serial.Load()), JSON: []byte(tofuPlanJSON)}, nil
			},
			applyS3: func() (tofu.Applied, error) {
				return tofu.Applied{Plan: []byte(tofuPlanJSON), State: tofu.State{Serial: tofu.Serial{Lineage: "L", Serial: serial.Add(1)}, Unmasked: []byte(tofuStateJSON)}}, nil
			},
		}
		f := tofuS3Fixture(t, runner, policyConfig(t, rule("deny")))
		approve := func(branch string) string {
			p, _ := f.planCommit(tofuS3Files(), branch)
			if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
				t.Fatalf("bob's approval of the plan at %s: %d %s", branch, r.status, r.body)
			}
			return p.Plan.ID
		}
		if run := f.tofuRun(bob(f), map[string]any{"plan": approve("first")}); run.Run.Status != "applied" {
			t.Fatalf("the first run %+v\n%s", run.Run, run.Text)
		}
		runner.mu.Lock()
		runner.plan = func() (tofu.Plan, error) {
			return tofu.Plan{File: savedPlan(t, "L", serial.Load()), JSON: []byte(rotatedPlanJSON)}, nil
		}
		runner.mu.Unlock()
		id := approve("rotate")
		n := len(runner.applied)
		r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
		if strings.Contains(string(r.body), tofuCanary) {
			t.Errorf("the refusal holds the past secret: %s", r.body)
		}
		scanForCanaries(t, f, tofuCanary, tofuRotated, tofuEnvSecret)
		if r.status != http.StatusForbidden || r.class() != "policy" || !strings.Contains(string(r.body), want) {
			t.Errorf("the apply: %d %s", r.status, r.body)
		}
		if len(runner.applied) != n {
			t.Error("OpenTofu applied a plan the apply point refused")
		}
	})
}

// A message that holds a character that is not graphic is quoted before
// it is kept, which escapes its quotes. A provider_env value written in
// that escaped form appears only after the quoting, so neither point
// keeps, answers, or logs it.
func TestTofuPolicyMasksQuotedMessages(t *testing.T) {
	const quoted = `canary-quoted \"8c4d\"`
	file := filepath.Join(t.TempDir(), "quoted")
	if err := os.WriteFile(file, []byte(quoted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withPolicy := policyConfig(t, "warn contains \"canary-quoted \\\"8c4d\\\"\\t\"\n")
	var log syncBuffer
	f := tofuApplyFixtureEdit(t, &fakeTofu{}, func(text string) string {
		return withPolicy(strings.Replace(text, `TF_VAR_extra = "`, fmt.Sprintf("TF_VAR_quoted = %q, TF_VAR_extra = \"", file), 1))
	}, func(o *server.Options) {
		o.Log = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	p, _ := f.planCommit(tofuFiles(), "quoted")
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}
	run := f.tofuRun(bob(f), map[string]any{"plan": p.Plan.ID})
	served := f.do("GET", "/v1/runs/"+run.Run.ID, bob(f), nil)
	plan, _ := json.Marshal(p)
	for name, text := range map[string]string{"the plan": string(plan), "the plan's text": p.Text, "the run": string(served.body), "the log": log.String()} {
		if strings.Contains(text, quoted) {
			t.Errorf("%s holds the provider_env value:\n%s", name, text)
		}
	}
	scanForCanaries(t, f, quoted, tofuEnvSecret)
	var got struct{ Policy json.RawMessage }
	if err := json.Unmarshal(served.body, &got); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]json.RawMessage{"plan": p.Policy, "apply": got.Policy} {
		if d := decisionOf(t, raw); d.Outcome != policy.Warn || len(d.Results) != 1 || strings.Contains(d.Results[0].Message, quoted) || !strings.Contains(d.Results[0].Message, "(sensitive)") {
			t.Errorf("the %s point's decision %+v", name, d)
		}
	}
}

// A drift check's plan is never approved or applied, so no policy judges
// it, and a policy that would deny that plan leaves its drift reported.
func TestDriftCheckSkipsPolicy(t *testing.T) {
	runner := &fakeTofu{}
	f := policyFixture(t, runner, `deny contains msg if {
	some rc in input.resource_changes
	rc.address == "random_id.gone"
	"delete" in rc.change.actions
	msg := sprintf("%s is deleted", [rc.address])
}
`)
	f.gitlab.Commit(4242, "main", driftFiles())
	appliedApp(t, f, "add-cert")
	runner.mu.Lock()
	runner.plan = driftPlan(true)
	runner.mu.Unlock()
	f.srv.LookForDrift(time.Hour)
	if c := checkOf(t, f.drift(f.token(nil), "?env=prod&stack=app"), "app"); c.Status != "drifted" || c.Error != "" || c.Code != 2 {
		t.Fatalf("the check of app, whose plan deletes random_id.gone: %+v, want drifted", c)
	}
}

// withEnvSecret adds to a config a provider_env value read from a file
// that holds secret, and one policy set, base, whose one file is rego.
func withEnvSecret(t *testing.T, secret, rego string) func(string) string {
	t.Helper()
	withValue, withPolicy := withEnvValue(t, secret), policyConfig(t, rego)
	return func(text string) string { return withPolicy(withValue(text)) }
}

// withEnvValue adds to a config a provider_env value read from a file that
// holds secret.
func withEnvValue(t *testing.T, secret string) func(string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "synthetic-secret")
	if err := os.WriteFile(file, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(text string) string {
		return strings.Replace(text, `TF_VAR_extra = "`, fmt.Sprintf("TF_VAR_synthetic = %q, TF_VAR_extra = \"", file), 1)
	}
}

// A provider_env value that spells part of the plan's structure, which the
// configuration names, leaves that part whole: a mandatory policy on a
// resource's type, name, mode, module, address, or provider denies at the
// plan point and at the apply point.
func TestTofuPolicyReadsStructureThatSpellsASecret(t *testing.T) {
	const match = `some rc in input.resource_changes
	rc.type == "random_id"
	rc.name == "i"
	rc.mode == "managed"
	rc.module_address == "module.cert"
	rc.address == "module.cert.random_id.i"
	rc.provider_name == "registry.opentofu.org/hashicorp/random"
	msg := "random_id is forbidden"`
	for _, secret := range []string{"random_id", "i", "managed", "module.cert", "registry.opentofu.org/hashicorp/random"} {
		t.Run(secret, func(t *testing.T) {
			t.Run("plan point", func(t *testing.T) {
				runner := &fakeTofu{}
				f := tofuApplyFixtureEdit(t, runner, withEnvSecret(t, secret, "deny contains msg if {\n\t"+match+"\n}\n"))
				p, _ := f.planCommit(tofuFiles(), "structure-plan")
				if d := decisionOf(t, p.Policy); p.Plan.Status != "refused" || d.Outcome != policy.Deny {
					t.Fatalf("plan %s, decision %+v; want refused by the deny", p.Plan.Status, d)
				}
			})
			t.Run("apply point", func(t *testing.T) {
				runner := &fakeTofu{}
				f := tofuApplyFixtureEdit(t, runner, withEnvSecret(t, secret, "deny contains msg if {\n\tinput.writ.decision == \"apply\"\n\t"+match+"\n}\n"))
				id := approvedTofuPlan(t, f, "structure-apply")
				r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
				if r.status != http.StatusForbidden || r.class() != "policy" || len(runner.applied) != 0 {
					t.Fatalf("the apply: %d %s", r.status, r.body)
				}
			})
		})
	}
}

// A provider_env value that spells the type of a resource whose name is not
// ASCII leaves its address whole, at the plan point and at the apply point.
// The plan is one OpenTofu wrote, of internal/mask/testdata/address.
func TestTofuPolicyReadsUnicodeAddressThatSpellsASecret(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "mask", "testdata", "address-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	const match = `some rc in input.resource_changes
	rc.address == "terraform_data.café"
	msg := "terraform_data.café is forbidden"`
	planned := func() (tofu.Plan, error) { return tofu.Plan{File: []byte("saved"), JSON: raw}, nil }
	t.Run("plan point", func(t *testing.T) {
		runner := &fakeTofu{plan: planned}
		f := tofuApplyFixtureEdit(t, runner, withEnvSecret(t, "terraform_data", "deny contains msg if {\n\t"+match+"\n}\n"))
		p, _ := f.planCommit(tofuFiles(), "unicode-plan")
		if d := decisionOf(t, p.Policy); p.Plan.Status != "refused" || d.Outcome != policy.Deny {
			t.Fatalf("plan %s, decision %+v; want refused by the deny\n%s", p.Plan.Status, d, p.Document)
		}
	})
	t.Run("apply point", func(t *testing.T) {
		runner := &fakeTofu{plan: planned}
		f := tofuApplyFixtureEdit(t, runner, withEnvSecret(t, "terraform_data", "deny contains msg if {\n\tinput.writ.decision == \"apply\"\n\t"+match+"\n}\n"))
		id := approvedTofuPlan(t, f, "unicode-apply")
		r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
		if r.status != http.StatusForbidden || r.class() != "policy" || len(runner.applied) != 0 {
			t.Fatalf("the apply: %d %s", r.status, r.body)
		}
	})
}

// A provider_env value that spells an action leaves the plan's actions,
// and with them its risk classes, the approvals it needs, and the teams
// that may apply it, as they are with a value that spells nothing.
func TestTofuRisksIgnoreTheMask(t *testing.T) {
	plan := func(t *testing.T, secret string) server.RemotePlan {
		t.Helper()
		f := tofuApplyFixtureEdit(t, &fakeTofu{}, withEnvValue(t, secret))
		p, _ := f.planCommit(tofuFiles(), "risks")
		return p
	}
	base := plan(t, "synthetic-unrelated-value")
	if base.Plan.Status != "pending" || !slices.Equal(base.Plan.Risks, []string{"create", "replace"}) || base.Plan.Approvals.Need != 1 {
		t.Fatalf("the plan with an unrelated value: %+v", base.Plan)
	}
	for _, secret := range []string{"create", "delete"} {
		t.Run(secret, func(t *testing.T) {
			p := plan(t, secret)
			var doc struct {
				ResourceChanges []struct {
					Change struct{ Actions []string } `json:"change"`
				} `json:"resource_changes"`
			}
			if err := json.Unmarshal(p.Document, &doc); err != nil || len(doc.ResourceChanges) != 2 {
				t.Fatalf("the document %s: %v", p.Document, err)
			}
			if a := doc.ResourceChanges[1].Change.Actions; !slices.Equal(a, []string{"delete", "create"}) {
				t.Errorf("the actions of module.cert.random_id.i are %q", a)
			}
			if p.Plan.Status != base.Plan.Status || !slices.Equal(p.Plan.Risks, base.Plan.Risks) || p.Plan.Approvals.Need != base.Plan.Approvals.Need ||
				!slices.Equal(p.Plan.Approvals.Rules, base.Plan.Approvals.Rules) || !slices.Equal(p.Plan.TeamsThatMayApply, base.Plan.TeamsThatMayApply) {
				t.Errorf("plan %+v\nwant it as %+v\n%s", p.Plan, base.Plan, p.Text)
			}
		})
	}
}

// A provider_env value that spells an outcome leaves the verdict alone:
// the mask sweeps only messages, so a deny still refuses, and a warning
// and a pass are kept as they were decided.
func TestTofuPolicyVerdictIgnoresTheMask(t *testing.T) {
	cases := []struct {
		secret, rego string
		outcome      policy.Outcome
		status       string
	}{
		{"deny", `deny contains "this must refuse" if true`, policy.Deny, "refused"},
		{"warn", `warn contains "a warning" if true`, policy.Warn, "pending"},
		{"pass", `deny contains "never" if false`, policy.Pass, "pending"},
	}
	for _, c := range cases {
		t.Run(c.secret, func(t *testing.T) {
			runner := &fakeTofu{}
			f := tofuApplyFixtureEdit(t, runner, withEnvSecret(t, c.secret, c.rego+"\n"))
			p, _ := f.planCommit(tofuFiles(), "verdict-"+c.secret)
			if d := decisionOf(t, p.Policy); d.Outcome != c.outcome || p.Plan.Status != c.status {
				t.Fatalf("plan %s, decision %+v; want %s and %s", p.Plan.Status, d, c.status, c.outcome)
			}
			if c.outcome != policy.Deny {
				return
			}
			if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status == http.StatusOK {
				t.Fatalf("an approval of the refused plan: %d %s", r.status, r.body)
			}
			if r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": p.Plan.ID})); r.status == http.StatusAccepted || len(runner.applied) != 0 {
				t.Fatalf("an apply of the refused plan: %d %s", r.status, r.body)
			}
		})
	}
	t.Run("deny at the apply point", func(t *testing.T) {
		runner := &fakeTofu{}
		f := tofuApplyFixtureEdit(t, runner, withEnvSecret(t, "deny", `deny contains "this must refuse" if input.writ.decision == "apply"`+"\n"))
		id := approvedTofuPlan(t, f, "verdict-apply")
		r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
		if r.status != http.StatusForbidden || r.class() != "policy" || len(runner.applied) != 0 {
			t.Fatalf("the apply: %d %s", r.status, r.body)
		}
	})
}

// A message longer than the cut is masked whole before it is cut, so no
// prefix of a long secret survives.
func TestTofuPolicyMasksLongMessagesBeforeTheCut(t *testing.T) {
	secret := "synthetic-long-secret-" + strings.Repeat("1234567890", 120)
	f := tofuApplyFixtureEdit(t, &fakeTofu{}, withEnvSecret(t, secret,
		fmt.Sprintf("warn contains concat(\"\", [%q, %q]) if true\n", secret[:600], secret[600:])))
	p, _ := f.planCommit(tofuFiles(), "long-secret")
	d := decisionOf(t, p.Policy)
	if len(d.Results) != 1 || d.Results[0].Message != "(sensitive)" {
		t.Fatalf("the decision %+v", d)
	}
	if strings.Contains(p.Text, secret[:100]) {
		t.Fatalf("the plan's text holds a prefix of the secret:\n%s", p.Text)
	}
	scanForCanaries(t, f, secret[:100])
}

// A masked message is cut at 1,000 bytes after the mask, and marked.
func TestTofuPolicyCutsMaskedMessages(t *testing.T) {
	long := strings.Repeat("x", 1500)
	f := policyFixture(t, &fakeTofu{}, fmt.Sprintf("warn contains concat(\"\", [%q, %q, %q]) if true\n", tofuEnvSecret, " ", long))
	p, _ := f.planCommit(tofuFiles(), "cut")
	d := decisionOf(t, p.Policy)
	want := ("(sensitive) " + long)[:1000] + " (cut)"
	if len(d.Results) != 1 || d.Results[0].Message != want {
		t.Fatalf("the decision %+v", d)
	}
}

// A cut that would split the marker of a masked value cuts before it.
func TestTofuPolicyCutKeepsTheMarkerWhole(t *testing.T) {
	prefix := strings.Repeat("x", 995)
	f := policyFixture(t, &fakeTofu{}, fmt.Sprintf("warn contains concat(\"\", [%q, %q, %q]) if true\n", prefix, tofuEnvSecret, strings.Repeat("y", 100)))
	p, _ := f.planCommit(tofuFiles(), "cut-marker")
	d := decisionOf(t, p.Policy)
	if len(d.Results) != 1 || d.Results[0].Message != prefix+" (cut)" {
		t.Fatalf("the decision %+v", d)
	}
}

// Approvals that change between the apply point's decision and the start
// of the run refuse the apply, so no run begins with approvals the policy
// did not see. Here Bob withdraws and Carol approves in that gap: the rule
// needs Bob, and Carol alone meets the approval rules.
func TestTofuPolicyApprovalsChangedRefusesApply(t *testing.T) {
	var armed atomic.Bool
	var f *fixture
	var id string
	runner := &fakeTofu{}
	f = policyFixture(t, runner, `deny contains "Bob must approve" if {
	input.writ.decision == "apply"
	not bob_approved
}

bob_approved if {
	some a in input.writ.approvals
	a.person == "gitlab/user/2002"
}
`, func(o *server.Options) {
		o.Now = func() time.Time {
			// createRun reads the clock once, after the apply point's
			// decision and before the run begins.
			pc, _, _, _ := runtime.Caller(1)
			if strings.HasSuffix(runtime.FuncForPC(pc).Name(), ".(*Server).createRun") && armed.CompareAndSwap(true, false) {
				if r := f.do("DELETE", "/v1/plans/"+id+"/approvals/mine", bob(f), nil); r.status != http.StatusOK {
					t.Errorf("bob's withdrawal: %d %s", r.status, r.body)
				}
				if r := f.do("POST", "/v1/plans/"+id+"/approvals", bob(f, map[string]any{"user_id": "3003", "user_login": "carol"}), nil); r.status != http.StatusOK {
					t.Errorf("carol's approval: %d %s", r.status, r.body)
				}
			}
			return time.Now()
		}
	})
	id = approvedTofuPlan(t, f, "approvals-changed")
	armed.Store(true)
	r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
	if armed.Load() {
		t.Fatal("the approvals did not change between the decision and the run")
	}
	if r.status != http.StatusConflict || !strings.Contains(string(r.body), "approvals changed since policy was evaluated; apply again") || len(runner.applied) != 0 {
		t.Fatalf("the apply: %d %s", r.status, r.body)
	}
	if after := f.do("GET", "/v1/plans/"+id, bob(f), nil); !strings.Contains(string(after.body), `"status":"approved"`) {
		t.Fatalf("the plan after the refusal: %s", after.body)
	}
	// Applied again, the policy sees Carol's approval only and denies.
	if again := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id})); again.status != http.StatusForbidden || again.class() != "policy" {
		t.Fatalf("the second apply: %d %s", again.status, again.body)
	}
}

// GET /v1/policy serves the config's policy sets with their .rego text and
// no path, to a principal that may read any stack, and refuses one that
// may read none. A server with no policy serves no set.
func TestPolicyEndpoint(t *testing.T) {
	f := newFixture(t, policyConfig(t, "deny_rsa contains \"no\" if false\n"))
	r := f.do("GET", "/v1/policy", f.token(nil), nil)
	var doc server.PolicyDoc
	if err := json.Unmarshal(r.body, &doc); err != nil || r.status != http.StatusOK || !doc.OK || doc.Format != "writ.policy/v0" {
		t.Fatalf("policy %d %s", r.status, r.body)
	}
	want := []server.PolicySetInfo{{Name: "base", Level: "mandatory", Files: []server.PolicyFileInfo{{Name: "base.rego", Text: "package main\n\nimport rego.v1\n\ndeny_rsa contains \"no\" if false\n"}}}}
	if !slices.EqualFunc(doc.Sets, want, func(a, b server.PolicySetInfo) bool {
		return a.Name == b.Name && a.Level == b.Level && slices.Equal(a.Files, b.Files)
	}) {
		t.Fatalf("sets %+v, want %+v", doc.Sets, want)
	}
	if strings.Contains(string(r.body), os.TempDir()) {
		t.Errorf("the policy document holds a path: %s", r.body)
	}
	if r := f.do("GET", "/v1/policy", f.token(map[string]any{"project_id": "1"}), nil); r.status != http.StatusForbidden || r.class() != "denied" || r.doc["format"] != "writ.policy/v0" {
		t.Fatalf("policy without read: %d %s", r.status, r.body)
	}
	none := newFixture(t, nil)
	if r := none.do("GET", "/v1/policy", none.token(nil), nil); r.status != http.StatusOK || !strings.Contains(string(r.body), `"sets":[]`) {
		t.Fatalf("policy of a server with none: %d %s", r.status, r.body)
	}
}
