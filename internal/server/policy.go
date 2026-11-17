package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/policy"
)

// Policies are the policy sets that judge every OpenTofu plan when it is
// made and again when an apply of it is asked for. They come only from
// the server's config, so no commit can change the policy that judges it.
type Policies struct {
	// Timeout bounds one decision, and Memory the address space its
	// evaluation process may add.
	Timeout time.Duration
	Memory  int64
	Sets    []PolicySet
}

// PolicySet is one policy set of the config.
type PolicySet struct {
	Name string
	// Dir holds the set's .rego files, which the config reads.
	Dir   string
	Level policy.Level
	Files map[string]string
}

// CompilePolicies compiles the config's policy sets, nil when it has
// none. A set that does not compile is an error that names its file and
// place.
func CompilePolicies(cfg *Config) (*policy.Engine, error) {
	p := cfg.Policies
	if p == nil {
		return nil, nil
	}
	sets := make([]policy.Set, len(p.Sets))
	for i, s := range p.Sets {
		sets[i] = policy.Set{Name: s.Name, Level: s.Level, Files: s.Files}
	}
	e, err := policy.Compile(sets, policy.Options{Timeout: p.Timeout, Memory: p.Memory})
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	return e, nil
}

// judge decides in at one point and sweeps the decision's messages with
// m, so no message a policy wrote holds a secret the mask knows. The mask
// sweeps only the messages: the verdict is decided before it, and a
// secret that spells an outcome must not change it. A message that is not
// printable is quoted, and the quoting escapes its quotes and
// backslashes, which can spell a secret written in that escaped form, so
// the mask sweeps the messages again after it. Only then is each message
// cut, as a cut before the mask keeps a prefix of a long secret. An input
// that cannot be built, and messages the mask cannot sweep, deny.
func (s *Server) judge(ctx context.Context, doc []byte, w policy.Writ, m *mask.Mask) policy.Decision {
	in, err := policy.NewInput(doc, w)
	var d policy.Decision
	if err != nil {
		d = policyError(w, fmt.Sprintf("build the policy input: %v", err))
	} else {
		d = s.policy.Decide(ctx, in)
	}
	if err := sweepMessages(&d, m); err != nil {
		return policyError(w, "the mask could not sweep the policy's messages, so writ serve keeps none")
	}
	for i := range d.Results {
		d.Results[i].Message = printable(d.Results[i].Message)
	}
	if err := sweepMessages(&d, m); err != nil {
		return policyError(w, "the mask could not sweep the policy's messages, so writ serve keeps none")
	}
	d.CutMessages(mask.Sensitive)
	return d
}

func sweepMessages(d *policy.Decision, m *mask.Mask) error {
	msgs := make([]string, len(d.Results))
	for i, r := range d.Results {
		msgs[i] = r.Message
	}
	if err := m.Sweep(&msgs); err != nil {
		return err
	}
	for i := range d.Results {
		d.Results[i].Message = msgs[i]
	}
	return nil
}

func policyError(w policy.Writ, msg string) policy.Decision {
	return policy.Decision{Format: policy.DecisionFormat, Point: w.Decision, Outcome: policy.Deny, At: w.Now.UTC(),
		Results: []policy.Result{{Rule: "error", Level: policy.Mandatory, Outcome: policy.Deny, Message: msg}}}
}

// judgeTofuPlan judges a pending plan of an OpenTofu stack at the plan
// point, with its masked document and the mask that made it. It keeps
// the decision with the plan, adds its lines to the plan's text, and
// refuses a plan that a policy denies, which then keeps no saved plan.
// A drift check's plan is never approved or applied, and a deny would
// report its drift as a failed check, so no policy judges it.
func (s *Server) judgeTofuPlan(ctx context.Context, res *control.PlanResult, doc []byte, m *mask.Mask, env, stack string, p *principal) {
	if s.policy == nil || res.Status != control.Pending || p == driftPrincipal {
		return
	}
	d := s.judge(ctx, doc, policy.Writ{Decision: policy.PlanPoint, Now: s.now(), Environment: env, Stack: stack, Actor: policyActor(p)}, m)
	res.Policy, _ = json.Marshal(d)
	res.Text += PolicyText(d)
	if d.Outcome.Denies() {
		res.Status, res.Saved = control.RefusedPlan, nil
	}
}

// judgeApply judges a live plan of an OpenTofu stack at the apply point,
// for p with the approvals that count now. It answers a deny itself and
// returns false; otherwise it returns the decision to keep with the run
// and the digest of the approvals it judged, which the run's start
// checks, both empty when no policy judges the apply.
func (s *Server) judgeApply(w http.ResponseWriter, r *http.Request, plan control.Plan, p *principal) (json.RawMessage, string, bool) {
	if s.policy == nil || plan.Tofu == nil || !plan.Status.Live() {
		return nil, "", true
	}
	approvals, err := s.store.Approvals(r.Context(), []string{plan.ID})
	if err != nil {
		s.dbFailed(w, r, FormatRemoteRun, err)
		return nil, "", false
	}
	have := s.rules().Tally(plan, approvals[plan.ID]).Approvals
	var counted []policy.Approval
	for _, a := range have {
		counted = append(counted, policy.Approval{Person: a.Person, Display: a.Display, Via: a.Via})
	}
	m, err := s.applyPointMask(r.Context(), plan)
	if err != nil {
		class, status := diag.Internal, http.StatusInternalServerError
		if ce, ok := errors.AsType[*classedError](err); ok {
			class, status = ce.class, http.StatusConflict
		}
		s.refuse(w, r, FormatRemoteRun, class, status, "judge the policy of plan %s: %v", plan.ID, err)
		return nil, "", false
	}
	d := s.judge(r.Context(), plan.Document, policy.Writ{Decision: policy.ApplyPoint, Now: s.now(), Environment: plan.Env, Stack: plan.Stack,
		Actor: policyActor(p), Approvals: counted}, m)
	if d.Outcome.Denies() {
		s.refusePolicy(w, r, plan, d)
		return nil, "", false
	}
	d.Approvals = control.ApprovalsDigest(have)
	b, _ := json.Marshal(d)
	return b, d.Approvals, true
}

// applyPointMask is the mask that sweeps the apply point's messages. It
// knows what the apply's mask starts with: the values a run of the plan's
// env gets from writ serve, and the secret set sealed with the state the
// plan read. A policy can build a past secret that the state keeps where
// nothing marks it, and only that set names it.
func (s *Server) applyPointMask(ctx context.Context, plan control.Plan) (*mask.Mask, error) {
	secrets, err := s.envSecrets(plan.Env)
	if err != nil {
		return nil, err
	}
	ts, _, err := s.tofuSourcesOf(ctx, plan)
	if err != nil {
		return nil, err
	}
	backend, err := s.stateBackend(plan.Env, plan.Stack, ts)
	if err != nil {
		return nil, err
	}
	key, err := s.stateKey(plan.Env)
	if err != nil {
		return nil, fmt.Errorf("the state key of env %s: %w", plan.Env, err)
	}
	onS3 := backend == control.BackendS3
	var saved []byte
	if onS3 {
		sealed, _, err := s.store.SavedPlan(ctx, plan.ID)
		if err != nil {
			return nil, fmt.Errorf("read the saved plan: %w", err)
		}
		if saved, err = key.OpenPlan(sealed, plan.ID, plan.Env, plan.Stack); err != nil {
			return nil, &classedError{diag.SavedPlan, err}
		}
	}
	learned, err := s.planSecrets(ctx, key, plan, onS3, saved)
	if err != nil {
		return nil, err
	}
	return mask.New(append(secrets, learned...)), nil
}

// envSecrets are the values a run of env would get from writ serve: its
// provider_env, or, under --dev, the TF_VAR_ variables of the shell writ
// serve started in.
func (s *Server) envSecrets(env string) ([]string, error) {
	var vars []string
	if s.cfg.Dev {
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "TF_VAR_") {
				vars = append(vars, kv)
			}
		}
	} else {
		e, ok := s.envOf(env)
		if !ok {
			return nil, fmt.Errorf("env %s is not served", env)
		}
		var err error
		if vars, err = readProviderEnv(e); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, kv := range vars {
		if _, v, ok := strings.Cut(kv, "="); ok {
			out = append(out, v)
		}
	}
	return out, nil
}

// refusePolicy answers an apply that a policy denies at the apply point:
// 403, a diagnostic of class policy that names the plan, and one for each
// result.
func (s *Server) refusePolicy(w http.ResponseWriter, r *http.Request, plan control.Plan, d policy.Decision) {
	req := reqOf(r)
	req.entry.Decision = control.Refused
	head := fmt.Sprintf("policy refuses the apply of plan %s", plan.ID)
	diags := []Diagnostic{{"error", string(diag.Policy), head}}
	var denies []string
	for _, res := range d.Results {
		severity := "warning"
		if res.Outcome.Denies() {
			severity = "error"
			denies = append(denies, res.Message)
		}
		diags = append(diags, Diagnostic{severity, string(diag.Policy), resultText(res)})
	}
	req.entry.Reason = head + ": " + strings.Join(denies, "; ")
	s.log.Info("policy refused an apply", "request", req.id, "plan", plan.ID, "env", plan.Env, "stack", plan.Stack, "person", req.entry.Person, "denies", len(denies))
	s.reply(w, http.StatusForbidden, refusal{Format: FormatRemoteRun, Diagnostics: diags})
}

func policyActor(p *principal) policy.Actor {
	return policy.Actor{Person: p.info.Person, Display: p.info.Display, Kind: p.info.Kind, Teams: slices.Clone(p.teams)}
}

// resultText is one result as the plan's text and a refusal print it.
func resultText(r policy.Result) string {
	if r.Set == "" {
		return fmt.Sprintf("%s: %s", r.Outcome, r.Message)
	}
	return fmt.Sprintf("%s: [%s] %s", r.Outcome, r.Set, r.Message)
}

// PolicyText is a decision as the plan's text ends: one line for each
// result, and the verdict.
func PolicyText(d policy.Decision) string {
	var b strings.Builder
	b.WriteString("\nPolicy: ")
	switch {
	case d.Outcome.Denies():
		b.WriteString("denied, so this plan cannot be approved or applied.\n")
	case d.Outcome == policy.Warn:
		b.WriteString("passed with warnings.\n")
	default:
		b.WriteString("passed.\n")
	}
	for _, r := range d.Results {
		b.WriteString("  " + resultText(r) + "\n")
	}
	return b.String()
}
