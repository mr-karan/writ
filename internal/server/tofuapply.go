package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/tfstate"
	"github.com/mr-karan/writ/internal/tofu"
)

// FormatTofuApply is the document of a run that applied a plan of an
// OpenTofu stack.
const FormatTofuApply = "writ.tofu-apply/v0"

// TofuApply is writ.tofu-apply/v0: how an apply of an OpenTofu plan ended,
// the events OpenTofu reported for each resource, masked, and the state
// version the apply left.
type TofuApply struct {
	Format  string `json:"format"`
	OK      bool   `json:"ok"`
	Applied bool   `json:"applied"`
	// Status is applied or apply-failed.
	Status string `json:"status"`
	// StateVersion is the newest version of the stack's state when the
	// apply ended, 0 when it has none. For a stack on S3, it is the serial
	// of the state the apply left, 0 when it could not be read.
	StateVersion int64             `json:"state_version"`
	Summary      *TofuApplySummary `json:"summary"`
	Events       []TofuApplyEvent  `json:"events"`
	Diagnostics  []Diagnostic      `json:"diagnostics"`
}

// TofuApplySummary is OpenTofu's count of what the apply changed.
type TofuApplySummary struct {
	Add    int `json:"add"`
	Change int `json:"change"`
	Import int `json:"import"`
	Remove int `json:"remove"`
}

// TofuApplyEvent is one apply_start, apply_complete, or apply_errored
// event of one resource. It keeps no id: a provider chooses a resource's
// id, and nothing marks one that holds a secret.
type TofuApplyEvent struct {
	Type           string `json:"type"`
	Address        string `json:"address"`
	Action         string `json:"action"`
	ElapsedSeconds int    `json:"elapsed_seconds"`
}

// applyTofu applies the saved plan of an OpenTofu plan: it opens the
// sealed saved plan, gathers the plan's commit again and checks its
// sources are the plan's, gives the run a token that locks and writes the
// stack's state, and runs init and apply as the run's uid with the
// environment's apply role. The run's token ends when applyTofu returns.
// Only the masked events and diagnostics leave the run, and the rows of
// the state index that the mask reads from the state the apply left.
func (s *Server) applyTofu(startCtx, ctx context.Context, run control.Run, plan control.Plan, p *principal) (ApplyOutput, error) {
	if s.tofuRunner == nil {
		return ApplyOutput{}, tofuRefused("plan %s is of an OpenTofu stack, and this writ serve runs no OpenTofu", plan.ID)
	}
	sealed, err := s.store.SavedPlan(startCtx, plan.ID)
	if err != nil {
		return ApplyOutput{}, fmt.Errorf("read the saved plan: %w", err)
	}
	key, err := s.stateKey(plan.Env)
	if err != nil {
		return ApplyOutput{}, fmt.Errorf("the state key of env %s: %w", plan.Env, err)
	}
	saved, err := key.OpenPlan(sealed, plan.ID, plan.Env, plan.Stack)
	if err != nil {
		return ApplyOutput{}, &classedError{diag.SavedPlan, err}
	}
	if tofuPlanID(plan.Document, saved, plan.Source.Commit, plan.Stack, plan.Env) != plan.ID {
		return ApplyOutput{}, &classedError{diag.SavedPlan, fmt.Errorf("the saved plan of plan %s is not the one its id names", plan.ID)}
	}
	ts, dir, err := s.tofuSourcesOf(startCtx, plan)
	if err != nil {
		return ApplyOutput{}, err
	}
	backend, err := s.stateBackend(plan.Env, plan.Stack, ts)
	if err != nil {
		return ApplyOutput{}, err
	}
	onS3 := backend == control.BackendS3
	if onS3 {
		if err := s.s3Role(plan.Env, plan.Stack, applyRole); err != nil {
			return ApplyOutput{}, err
		}
	}
	tr, err := s.startRun(startCtx, runSpec{id: run.ID, env: plan.Env, commit: plan.Source.Commit, ts: ts, dir: dir, role: applyRole, p: p, stateCreds: onS3,
		what: fmt.Sprintf("plan %s is of an OpenTofu stack", plan.ID)})
	if err != nil {
		return ApplyOutput{}, err
	}
	defer tr.done()
	// A stack on S3 writes its state and its lock file with the apply
	// role. tofu writes the state it reached after an interrupt at the
	// apply's timeout, within ApplyStop.
	if !onS3 {
		if err := tr.stateToken(startCtx, plan.Stack, control.StateApply, s.cfg.Limits.ApplyStartTimeout+s.cfg.Limits.ApplyTimeout+tofu.ApplyStop+time.Minute); err != nil {
			return ApplyOutput{}, err
		}
	}
	// The mask learns the secret set of the state the plan read, as the
	// plan's mask did, so the events of a resource whose address holds a
	// past secret are swept, and the set the apply seals with the state it
	// writes carries those secrets on.
	learned, err := s.planSecrets(startCtx, key, plan, onS3, saved)
	if err != nil {
		return ApplyOutput{}, err
	}
	m := tr.mask(learned)
	stk, err := tr.stack(startCtx, m)
	if err != nil {
		return ApplyOutput{}, err
	}
	res, err := s.tofuRunner.Apply(ctx, stk, saved)
	doc := TofuApply{Format: FormatTofuApply, Status: "apply-failed", Events: []TofuApplyEvent{}, Diagnostics: []Diagnostic{}}
	// The network document is made once the mask learned the plan's
	// secrets, below, before the run's output is read.
	var net *NetworkDoc
	// The mask learns the plan's secrets, as it did when it masked the
	// plan, before it reads the events: an address can hold a value that
	// only the plan marks, as a for_each key of nonsensitive() does.
	_, merr := m.Plan(res.Plan)
	withheld := len(res.Plan) > 0 && merr != nil
	if !withheld {
		net = tr.network(m)
	}
	switch {
	case len(res.Plan) == 0:
	case withheld:
		doc.Diagnostics = append(doc.Diagnostics, Diagnostic{"error", string(diag.Internal), fmt.Sprintf("the mask could not read the saved plan, so the run keeps none of OpenTofu's events: %v", merr)})
	default:
		doc.Events, doc.Summary = applyEvents(m, res.Log), applySummary(res.Log)
	}
	switch se, isStep := errors.AsType[*tofu.StepError](err); {
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || timedOut(ctx) != nil):
		doc.Diagnostics = append(doc.Diagnostics, Diagnostic{"error", string(diag.Usage), fmt.Sprintf("the apply ran past limits.apply_timeout, %s: OpenTofu was interrupted, started no more changes, and wrote the state it reached; plan again", shortDuration(s.cfg.Limits.ApplyTimeout))})
	case errors.Is(err, tofu.ErrStale):
		doc.Diagnostics = append(doc.Diagnostics, Diagnostic{"error", string(diag.State), fmt.Sprintf("OpenTofu refused plan %s: the state of %s@%s changed after the plan was made; plan again", plan.ID, plan.Stack, plan.Env)})
	case isStep:
		doc.Diagnostics = append(doc.Diagnostics, Diagnostic{"error", string(diag.Usage), se.Error()})
		if !withheld {
			doc.Diagnostics = append(doc.Diagnostics, stepDiagnostics(m, se.Stdout, false)...)
		}
		doc.Diagnostics = append(doc.Diagnostics, egressDiagnostics(net, plan.Env, se.Stdout, se.Stderr)...)
	case err != nil:
		doc.Diagnostics = append(doc.Diagnostics, Diagnostic{"error", string(diag.Usage), err.Error()})
	default:
		doc.OK, doc.Applied, doc.Status = true, true, "applied"
	}
	// The apply's own context may have ended, and the run still records
	// the state it left.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if onS3 {
		doc.StateVersion = s.appliedS3(readCtx, run, plan, key, m, len(res.Plan) > 0 && !withheld, saved, res, doc.OK)
		body, err := json.Marshal(doc)
		if err != nil {
			return ApplyOutput{}, err
		}
		return ApplyOutput{Document: body, Text: tofuApplyText(plan, doc), Network: net.raw()}, nil
	}
	// Each version the run wrote, whether the apply succeeded or failed,
	// carries the set on before anything else can end the run.
	if len(res.Plan) > 0 && !withheld {
		s.keepIndexSecrets(readCtx, run, plan, key, m)
	}
	latest, err := s.store.LatestState(readCtx, plan.Env, plan.Stack)
	switch {
	case err == nil:
		doc.StateVersion = latest.Version
	case !errors.Is(err, control.ErrNotFound):
		return ApplyOutput{}, fmt.Errorf("read the state version of %s@%s after the apply: %w", plan.Stack, plan.Env, err)
	}
	if doc.OK {
		s.indexApplied(readCtx, run, plan, m, res, latest)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return ApplyOutput{}, err
	}
	return ApplyOutput{Document: body, Text: tofuApplyText(plan, doc), Network: net.raw()}, nil
}

// planSecrets opens the secret set sealed with the state that plan read:
// its state version on the http backend, or on S3 the serial its opened
// saved plan holds.
func (s *Server) planSecrets(ctx context.Context, key *tfstate.Key, plan control.Plan, onS3 bool, saved []byte) ([]string, error) {
	read := tofu.Serial{Serial: plan.Tofu.StateVersion}
	if onS3 {
		var err error
		if read, err = tofu.PlanSerial(saved); err != nil {
			return nil, &classedError{diag.SavedPlan, err}
		}
	}
	return s.stateSecrets(ctx, key, plan.Env, plan.Stack, onS3, read)
}

// applyEvents returns the apply_start, apply_complete, and apply_errored
// events of an `apply -json` stream, each address swept by the mask.
func applyEvents(m *mask.Mask, log []byte) []TofuApplyEvent {
	out := []TofuApplyEvent{}
	for line := range bytes.Lines(log) {
		ev, ok := decodeEvent(line)
		if !ok {
			continue
		}
		switch ev.Type {
		case "apply_start", "apply_complete", "apply_errored":
		default:
			continue
		}
		out = append(out, TofuApplyEvent{Type: ev.Type, Address: printable(string(m.Event([]byte(ev.Hook.Resource.Addr)))),
			Action: printable(ev.Hook.Action), ElapsedSeconds: ev.Hook.Elapsed})
	}
	return out
}

// applySummary is the change_summary event of an `apply -json` stream,
// nil when tofu wrote none.
func applySummary(log []byte) *TofuApplySummary {
	var sum *TofuApplySummary
	for line := range bytes.Lines(log) {
		if ev, ok := decodeEvent(line); ok && ev.Type == "change_summary" && ev.Changes != nil && ev.Changes.Operation == "apply" {
			s := ev.Changes.TofuApplySummary
			sum = &s
		}
	}
	return sum
}

// tofuApplyText is the apply as writ apply --remote prints it.
func tofuApplyText(plan control.Plan, doc TofuApply) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "OpenTofu apply of %s@%s\n", printable(plan.Stack), printable(plan.Env))
	for _, ev := range doc.Events {
		switch ev.Type {
		case "apply_complete":
			fmt.Fprintf(&b, "  %s: %s complete\n", ev.Address, ev.Action)
		case "apply_errored":
			fmt.Fprintf(&b, "  %s: %s errored\n", ev.Address, ev.Action)
		}
	}
	for _, d := range doc.Diagnostics {
		fmt.Fprintf(&b, "%s: %s\n", d.Severity, d.Message)
	}
	if s := doc.Summary; s != nil {
		fmt.Fprintf(&b, "Apply: %d added, %d changed, %d destroyed", s.Add, s.Change, s.Remove)
		if s.Import > 0 {
			fmt.Fprintf(&b, ", %d imported", s.Import)
		}
		b.WriteString(".\n")
	}
	if doc.StateVersion > 0 {
		fmt.Fprintf(&b, "State version %d of %s@%s.\n", doc.StateVersion, printable(plan.Stack), printable(plan.Env))
	}
	return b.String()
}
