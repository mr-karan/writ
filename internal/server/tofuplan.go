package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/repo"
	"github.com/mr-karan/writ/internal/tfstate"
	"github.com/mr-karan/writ/internal/tofu"
)

// TofuRunner runs OpenTofu for the stacks of .tf files. *tofu.Runner is
// one.
type TofuRunner interface {
	// Fill installs the providers a lock file pins into the plugin cache
	// that every run reads, and returns the lock file a run's init reads,
	// which a tofu.Stack's Lock takes.
	Fill(ctx context.Context, lock []byte) ([]byte, error)
	// Plan runs init, plan, and show of one stack and returns the raw
	// result, which only the mask may read.
	Plan(ctx context.Context, s tofu.Stack) (tofu.Plan, error)
	// Apply runs init, show of a saved plan, and apply, and returns the
	// raw show and `tofu apply -json` event stream, which only the mask
	// may read.
	Apply(ctx context.Context, s tofu.Stack, plan []byte) (tofu.Applied, error)
	// Show runs init without the backend and show of a state, and returns
	// the raw `tofu show -json`, which only the mask may read.
	Show(ctx context.Context, s tofu.Stack, state []byte) (tofu.State, error)
	// Pull runs init, state pull from the stack's backend, and show of
	// the pulled state, and returns the raw `tofu show -json`, which only
	// the mask may read, with the state's serial.
	Pull(ctx context.Context, s tofu.Stack) (tofu.State, error)
}

// maxTofuDiagnostics bounds the lines of a failed step's output that a
// refused plan keeps.
const maxTofuDiagnostics = 40

// initState makes the http state backend whose routes writ serve mounts
// under /v1/tfstate/, with each environment's state key from its key
// file, which the backend makes when it does not exist yet.
func (s *Server) initState() error {
	s.stateKeys = map[string]*tfstate.Key{}
	b, err := tfstate.New(tfstate.Options{Store: s.store, Now: s.now, Log: s.log.With("component", "tfstate"), Key: s.stateKey})
	if err != nil {
		return err
	}
	s.tfstate = b
	return nil
}

// stateKey is the key that seals the states and the saved plans of an
// OpenTofu stack in env.
func (s *Server) stateKey(env string) (*tfstate.Key, error) {
	e, ok := s.envOf(env)
	if !ok {
		return nil, fmt.Errorf("env %s is not served", env)
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if k, ok := s.stateKeys[e.Name]; ok {
		return k, nil
	}
	k, err := tfstate.OpenKey(e.Key)
	if err != nil {
		return nil, err
	}
	s.stateKeys[e.Name] = k
	return k, nil
}

// tofuRefused is a plan of an OpenTofu stack that writ serve does not
// run, with why.
func tofuRefused(format string, args ...any) error {
	return &classedError{diag.Usage, fmt.Errorf(format, args...)}
}

// planTofu plans the OpenTofu stack in the commit snap of a commit plan:
// it gathers the stack's files, gives the run a token that reads the
// stack's state on the http backend, or, for a stack on S3, the plan role
// for its backend's init too, fills the plugin cache, runs tofu as the
// run's uid, and masks the plan. Only the masked document and the text
// made from it leave the run, and only past planMask. A plan that tofu
// refuses comes back refused, with what planMask lets it keep.
func (s *Server) planTofu(ctx context.Context, ref, env, stack string, in planInput, snap *repo.Snapshot, p *principal) (control.PlanResult, error) {
	ts, err := repo.TofuSources(ctx, in.client, in.repo.ProjectID, snap, in.dir, s.store, repo.DefaultLimits)
	res := control.PlanResult{SourceDigest: ts.Digest}
	if err != nil {
		return res, &classedError{diag.Usage, err}
	}
	s.keepBlobs(in.repo.Name, snap)
	if s.tofuRunner == nil {
		return res, tofuRefused("stack %s is an OpenTofu stack, and this writ serve runs no OpenTofu: its config needs a tofu { } block, or, under --dev, OpenTofu %s on PATH", stack, tofu.Version)
	}
	backend, err := s.stateBackend(env, stack, ts)
	if err != nil {
		return res, err
	}
	onS3 := backend == control.BackendS3
	if onS3 {
		if err := s.s3Role(env, stack, planRole); err != nil {
			return res, err
		}
	}
	started := s.now()
	run, err := s.startRun(ctx, runSpec{id: ref, env: env, commit: in.source.Commit, ts: ts, dir: in.dir, role: planRole, p: p, stateCreds: onS3,
		what: fmt.Sprintf("stack %s is an OpenTofu stack", stack)})
	if err != nil {
		return res, err
	}
	defer run.done()
	key, err := s.stateKey(env)
	if err != nil {
		return res, fmt.Errorf("the state key of env %s: %w", env, err)
	}
	// A stack on S3 reads its state with the plan role, and a stack on
	// the http backend with a token that reads its state and nothing else.
	read := planRead{key: key, env: env, stack: stack, run: run, started: started}
	if onS3 {
		read.s3 = ts.Backend
	} else {
		v, err := s.store.LatestState(ctx, env, stack)
		if err != nil && !errors.Is(err, control.ErrNotFound) {
			return res, fmt.Errorf("read the state version of %s@%s: %w", stack, env, err)
		}
		read.version = v.Version
		if err := run.stateToken(ctx, stack, control.StatePlan, s.cfg.Limits.PlanTimeout+time.Minute); err != nil {
			return res, err
		}
	}
	stk, err := run.stack(ctx, run.mask())
	if err != nil {
		return res, err
	}
	raw, err := s.tofuRunner.Plan(ctx, stk)
	se, refused := errors.AsType[*tofu.StepError](err)
	if err != nil && !refused {
		return res, err
	}
	m, version, err := s.planMask(ctx, read, raw.File, refused)
	if err != nil {
		return res, err
	}
	if refused {
		return refusedTofu(res, se, m, onS3), nil
	}
	doc, err := m.Plan(raw.JSON)
	if err != nil {
		return res, fmt.Errorf("the mask refused the plan, so writ serve keeps nothing of it: %w", err)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return res, err
	}
	res.Changes, res.Risks, err = tofuChanges(doc)
	if err != nil {
		return res, tofuRefused("stack %s: %w; plan with a version of writ serve that knows it", stack, err)
	}
	res.Document, res.Text = body, tofuText(stack, env, doc, append(movedNotes(ts.Moved), tofuEvents(m, raw.Log, "warning")...))
	res.Status = control.RefusedPlan
	if !doc.Errored {
		res.ID, res.Status = tofuPlanID(body, raw.File, in.source.Commit, stack, env), control.Pending
		res.Saved = key.SealPlan(raw.File, res.ID, env, stack)
		res.Tofu = &control.TofuPlan{StateVersion: version}
	}
	s.judgeTofuPlan(ctx, &res, body, m, env, stack, p)
	return res, nil
}

// noSecretSet is a state version, or a serial on S3, that writ serve
// sealed no secret set with: an apply of writ serve wrote it before
// writ serve kept them, or whose mask could not read its plan, or
// something outside writ serve wrote it. A run against it cannot know
// the past secrets the state keeps where nothing marks them, so a plan
// keeps no detail and is never approved, and a drift check finds drift
// unknown.
type noSecretSet struct {
	env, stack string
	version    int64
}

func (e *noSecretSet) Error() string {
	return fmt.Sprintf("no secret set for state version %d of %s@%s, so writ serve keeps no detail of a plan against it and never approves one: "+
		"a state keeps a past secret where nothing marks it, as an address keyed by nonsensitive() of a variable whose value changed since, "+
		"and only the secret set sealed with the version names it; this version was written before writ serve kept secret sets, by an apply whose mask could not read its plan, or outside writ serve; "+
		"a person with adopt on %s@%s can seal a set for it with writ state adopt",
		e.version, e.stack, e.env, e.stack, e.env)
}

// stateSecrets opens the secret set sealed with the state a run plans or
// applies against: version v.Serial on the http backend, or the serial v
// on S3. A stack with no state has none. A state with no set is a
// *noSecretSet.
func (s *Server) stateSecrets(ctx context.Context, key *tfstate.Key, env, stack string, onS3 bool, v tofu.Serial) ([]string, error) {
	if v.Serial == 0 {
		return nil, nil
	}
	var sealed []byte
	var err error
	if onS3 {
		sealed, err = s.store.SerialSecrets(ctx, env, stack, v.Lineage, v.Serial)
	} else {
		sealed, err = s.store.IndexSecrets(ctx, env, stack, v.Serial)
	}
	if errors.Is(err, control.ErrNotFound) {
		return nil, &classedError{diag.State, &noSecretSet{env: env, stack: stack, version: v.Serial}}
	}
	if err != nil {
		return nil, fmt.Errorf("read the secret set of state version %d of %s@%s: %w", v.Serial, stack, env, err)
	}
	return key.OpenIndexSecrets(sealed, env, stack, v.Serial)
}

// knownSecrets opens every secret set writ serve keeps for stack@env, of
// any lineage, serial, or state version, and returns each secret once.
// The sets carry most secrets on from one to the next, and it stops once
// the distinct secrets pass what a mask sweeps, so a stack with many sets
// costs no more than that.
func (s *Server) knownSecrets(ctx context.Context, key *tfstate.Key, env, stack string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	total := 0
	err := s.store.SecretSets(ctx, env, stack, func(set control.SealedSet) error {
		secrets, err := key.OpenIndexSecrets(set.Sealed, env, stack, set.Version)
		if err != nil {
			return err
		}
		for _, x := range secrets {
			if _, ok := seen[x]; !ok {
				seen[x] = struct{}{}
				out = append(out, x)
				total += len(x)
			}
		}
		if total > mask.MaxSecretBytes {
			return fmt.Errorf("they hold more than %d bytes of secrets, more than the mask sweeps", mask.MaxSecretBytes)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the secret sets of %s@%s: %w", stack, env, err)
	}
	return out, nil
}

// planRead is what planMask needs to find the state a plan read.
type planRead struct {
	key        *tfstate.Key
	env, stack string
	run        *tofuRun
	// s3 is the stack's S3 backend, nil on the http backend.
	s3 *repo.Backend
	// version is, on the http backend, the newest state version when the
	// plan started.
	version int64
	started time.Time
}

// planMask is the one gate between a plan's output and what writ serve
// keeps of it, for a person's plan and a drift check alike, on the http
// backend and on S3. It finds the state the plan read and returns a mask
// that knows the run's own secrets and exactly the secret set sealed with
// that state, with the state version on the http backend: a state keeps
// a past secret where nothing marks it, as an address keyed by
// nonsensitive() of a variable whose value changed since, and only that
// set names it. A state version that moved while the plan ran, or a state
// with no set, is an error that holds nothing of tofu's output. A plan on
// S3 that tofu refused wrote no saved plan to read its serial from, so its
// mask knows every secret set writ serve keeps for the stack@env instead,
// and refusedTofu sweeps the instance keys a set may not name.
func (s *Server) planMask(ctx context.Context, r planRead, saved []byte, refused bool) (*mask.Mask, int64, error) {
	var v tofu.Serial
	switch {
	case r.s3 == nil:
		now, err := s.store.LatestState(ctx, r.env, r.stack)
		if err != nil && !errors.Is(err, control.ErrNotFound) {
			return nil, 0, fmt.Errorf("read the state version of %s@%s: %w", r.stack, r.env, err)
		}
		if now.Version != r.version {
			return nil, 0, &classedError{diag.State, fmt.Errorf("the state of %s@%s moved from version %d to %d while the plan ran, so the plan may have read a version whose secret set its mask did not learn, and writ serve keeps nothing of it: plan again", r.stack, r.env, r.version, now.Version)}
		}
		v.Serial = now.Version
	case refused:
		known, err := s.knownSecrets(ctx, r.key, r.env, r.stack)
		if err != nil {
			return nil, 0, err
		}
		return r.run.mask(known), 0, nil
	default:
		// The serial the plan read is the one its apply must find, and one
		// that no apply of writ serve wrote is drift.
		var err error
		if v, err = tofu.PlanSerial(saved); err != nil {
			return nil, 0, tofuRefused("stack %s: %v", r.stack, err)
		}
		if _, _, err := s.observeSerial(ctx, r.env, r.stack, r.s3, v, r.started); err != nil {
			return nil, 0, fmt.Errorf("record the serial of the state of %s@%s that the plan read: %w", r.stack, r.env, err)
		}
	}
	sealed, err := s.stateSecrets(ctx, r.key, r.env, r.stack, r.s3 != nil, v)
	if err != nil {
		return nil, 0, err
	}
	m := r.run.mask(sealed)
	if r.s3 != nil {
		return m, 0, nil
	}
	return m, v.Serial, nil
}

func writeTofuFiles(work string, files map[string][]byte, commit string) error {
	for name, text := range files {
		// The names come from the forge's tree.
		if !fs.ValidPath(name) {
			return tofuRefused("commit %s names a file %q, which is not a path within the repository", forge.Short(commit), name)
		}
		f := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(f, text, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// endState ends a run's tokens and locks on the http backend. It runs
// when the run ends however it ends, even after the server's context
// ended. After a failure here the record of the run's end, or else the
// sweep each minute, ends them.
func (s *Server) endState(run string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.tfstate.End(ctx, run); err != nil {
		s.log.Error("end a run's state token", "run", run, "err", err)
	}
}

// refusedTofu is the result of a plan whose step tofu failed: refused.
// The mask has not read a plan yet, so it knows the environment's secrets
// and the state's secret set but none of the plan's sensitive values,
// and a diagnostic's detail or snippet can show one, as
// nonsensitive() in an error_message does. So the result keeps only each
// diagnostic's severity, summary, and place, swept by the mask, or, when
// the step wrote no diagnostics, only which step failed.
//
// On S3 (unread) the serial the plan read is not known, and m knows every
// set writ serve keeps for the stack@env instead. That serial may have no
// set: one written outside writ serve, or the state of a stack writ serve
// never applied or adopted. Such a state can keep a past secret that no
// set names in an address keyed by nonsensitive(), which a summary such
// as "Cannot delete" quotes, so each quoted instance key becomes
// ["(sensitive)"]. The rest of the summary stays, because it says why the
// plan failed, as an AccessDenied names the action the role lacks. So a
// past secret such a state keeps in an attribute through nonsensitive()
// shows where a summary quotes that attribute; docs/serve.md states the
// limit.
func refusedTofu(res control.PlanResult, se *tofu.StepError, m *mask.Mask, unread bool) control.PlanResult {
	diags := []Diagnostic{{"error", string(diag.Usage), se.Error()}}
	res.Text = "writ: " + se.Error() + "\n"
	for _, d := range stepDiagnostics(m, se.Stdout, unread) {
		diags = append(diags, d)
		res.Text += d.Severity + ": " + d.Message + "\n"
	}
	res.Status = control.RefusedPlan
	res.Document, _ = json.Marshal(refusal{Format: "writ.plan.json/v2", Diagnostics: diags})
	return res
}

// instanceKeys matches a quoted instance key of an address, as OpenTofu
// writes it: ["KEY"], with KEY escaped as HCL escapes a string, and as a
// summary that quotes the address once more writes it: [\"KEY\"], where
// a quote in KEY is \\\" and a backslash \\\\. A key quoted three
// times or more is not matched.
var instanceKeys = regexp.MustCompile(`\["(?:[^"\\]|\\.)*"\]|\[\\"(?:[^\\]|\\\\(?:\\[\\"]|[^\\])|\\[^"\\])*\\"\]`)

// tofuEvent is one line of OpenTofu's -json event stream, with the fields
// writ serve reads: a diagnostic, an apply hook, or a change summary.
type tofuEvent struct {
	Type       string `json:"type"`
	Diagnostic struct {
		Severity string `json:"severity"`
		Summary  string `json:"summary"`
		Detail   string `json:"detail"`
		Range    *struct {
			Filename string `json:"filename"`
			Start    struct {
				Line   int `json:"line"`
				Column int `json:"column"`
			} `json:"start"`
		} `json:"range"`
	} `json:"diagnostic"`
	Hook struct {
		Resource struct {
			Addr string `json:"addr"`
		} `json:"resource"`
		Action  string `json:"action"`
		Elapsed int    `json:"elapsed_seconds"`
	} `json:"hook"`
	Changes *struct {
		TofuApplySummary
		Operation string `json:"operation"`
	} `json:"changes"`
}

// decodeEvent reads one line of an event stream. A line that is not an
// event is skipped.
func decodeEvent(line []byte) (tofuEvent, bool) {
	var ev tofuEvent
	err := json.Unmarshal(bytes.TrimSpace(line), &ev)
	return ev, err == nil
}

// stepDiagnostics returns the diagnostics of a -json event stream as
// "file:line:column: summary", at most maxTofuDiagnostics of them, each
// swept by the mask, and with sweepKeys each quoted instance key swept
// whole. It reads no detail and no snippet.
func stepDiagnostics(m *mask.Mask, log []byte, sweepKeys bool) []Diagnostic {
	var out []Diagnostic
	for line := range bytes.Lines(log) {
		ev, ok := decodeEvent(line)
		if !ok || ev.Type != "diagnostic" {
			continue
		}
		d := ev.Diagnostic
		severity := "error"
		if d.Severity == "warning" {
			severity = "warning"
		}
		text := d.Summary
		if r := d.Range; r != nil && r.Filename != "" {
			text = fmt.Sprintf("%s:%d:%d: %s", r.Filename, r.Start.Line, r.Start.Column, text)
		}
		if len(out) == maxTofuDiagnostics {
			out = append(out, Diagnostic{"error", string(diag.Usage), "(more diagnostics cut)"})
			break
		}
		// The mask runs on the summary as tofu wrote it, so a known secret
		// matches whole before the key sweep or the newline fold could
		// split it.
		text = string(m.Event([]byte(text)))
		text = strings.ReplaceAll(text, "\n", " ")
		if sweepKeys {
			text = instanceKeys.ReplaceAllLiteralString(text, `["`+mask.Sensitive+`"]`)
		}
		out = append(out, Diagnostic{severity, string(diag.Usage), printable(text)})
	}
	return out
}

// stepLines is the swept output of a failed step, to follow its error.
func stepLines(m *mask.Mask, err error) string {
	se, ok := errors.AsType[*tofu.StepError](err)
	if !ok {
		return ""
	}
	lines := sweptLines(m, se.Stderr)
	if len(lines) == 0 {
		return ""
	}
	return ":\n" + strings.Join(lines, "\n")
}

// sweptLines returns the non-empty lines of a step's text output, swept,
// at most maxTofuDiagnostics of them.
func sweptLines(m *mask.Mask, out []byte) []string {
	var lines []string
	for line := range bytes.Lines(out) {
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(lines) == maxTofuDiagnostics {
			lines = append(lines, "(more lines cut)")
			break
		}
		lines = append(lines, printable(string(m.Event(line))))
	}
	return lines
}

// movedNotes tell a plan's reader which providers of a lock file that
// Terraform wrote the run took from OpenTofu's registry.
func movedNotes(moved []tofu.Moved) []string {
	var out []string
	for _, m := range moved {
		how := "the same version and hashes"
		if m.Rehashed {
			how = "the same version, checked against the hashes of OpenTofu's build, which OpenTofu's registry signs"
		}
		if m.Kept {
			out = append(out, fmt.Sprintf("%s pins %s, which the code names; the run also used %s, for the code that names it with no host, at the same version and hashes", tofu.LockFile, m.From, m.To))
			continue
		}
		out = append(out, fmt.Sprintf("%s pins %s, as Terraform writes it; the run used %s at %s", tofu.LockFile, m.From, m.To, how))
	}
	return out
}

// tofuEvents returns the diagnostics of one severity in a -json event
// stream, each swept by the mask, as "summary: detail".
func tofuEvents(m *mask.Mask, log []byte, severity string) []string {
	var out []string
	for line := range bytes.Lines(log) {
		ev, ok := decodeEvent(m.Event(bytes.TrimSpace(line)))
		if !ok || ev.Type != "diagnostic" || ev.Diagnostic.Severity != severity {
			continue
		}
		d := ev.Diagnostic
		text := d.Summary
		if d.Detail != "" {
			text += ": " + d.Detail
		}
		if d.Range != nil && d.Range.Filename != "" {
			text = fmt.Sprintf("%s:%d: %s", d.Range.Filename, d.Range.Start.Line, text)
		}
		if len(out) == maxTofuDiagnostics {
			out = append(out, "(more diagnostics cut)")
			break
		}
		out = append(out, printable(strings.ReplaceAll(text, "\n", " ")))
	}
	return out
}

// tofuPlanID is a plan's id: the SHA-256 of the masked document, of the
// saved plan's digest, of the commit, of the stack, and of the
// environment, cut to the 32 hex digits a plan id has.
func tofuPlanID(doc, saved []byte, commit, stack, env string) string {
	sum := sha256.Sum256(saved)
	h := sha256.New()
	for _, part := range [][]byte{[]byte("writ opentofu plan v1"), doc, sum[:], []byte(commit), []byte(stack), []byte(env)} {
		h.Write(strconv.AppendInt(nil, int64(len(part)), 10))
		h.Write([]byte{0})
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// tofuAction is what Writ calls one action list of a resource change, the
// risk class that approval rules and grants match it by, and what it
// counts toward in OpenTofu's summary of a plan.
type tofuAction struct {
	kind, risk                   string
	add, change, destroy, forget int
}

// tofuActions is every action list OpenTofu 1.13 writes in a plan. A plan
// with any other is refused, so a kind of change Writ does not know never
// applies without the approvals its risk would need.
var tofuActions = map[string]tofuAction{
	"create":        {kind: "create", risk: authz.RiskCreate, add: 1},
	"update":        {kind: "update", risk: authz.RiskUpdate, change: 1},
	"delete":        {kind: "delete", risk: authz.RiskDelete, destroy: 1},
	"delete,create": {kind: "replace", risk: authz.RiskReplace, add: 1, destroy: 1},
	"create,delete": {kind: "replace", risk: authz.RiskReplace, add: 1, destroy: 1},
	// A forget leaves the object where it is and stops managing it, so
	// the stack loses control of it as a delete loses it.
	"forget":        {kind: "forget", risk: authz.RiskDelete, forget: 1},
	"forget,create": {kind: "replace", risk: authz.RiskReplace, add: 1, forget: 1},
	"no-op":         {},
	"read":          {},
}

// TofuChange names one resource change of an OpenTofu plan by what it
// does, as OpenTofu's human plan does, "" for one that changes nothing,
// and returns its risk classes. An import writes state for an object the
// stack did not manage, so its risk class is create. An action list that
// OpenTofu 1.13 does not write is an error that names it.
func TofuChange(c mask.ResourceChange) (string, []string, error) {
	actions := strings.Join(c.Change.Actions, ",")
	a, ok := tofuActions[actions]
	if !ok {
		return "", nil, fmt.Errorf("resource %s has the actions %s, which writ serve does not know, so it cannot tell which approvals the plan needs", printable(c.Address), printable(strconv.Quote(actions)))
	}
	var risks []string
	if a.risk != "" {
		risks = append(risks, a.risk)
	}
	kind := a.kind
	if c.Change.Importing != nil {
		risks = append(risks, authz.RiskCreate)
		if kind == "" {
			kind = "import"
		}
	}
	return kind, risks, nil
}

// tofuChanges counts the resource changes that change something, and
// lists the risk classes among them in authz.Risks order.
func tofuChanges(p *mask.Plan) (int, []string, error) {
	n := 0
	seen := map[string]bool{}
	for _, c := range p.ResourceChanges {
		k, rs, err := TofuChange(c)
		if err != nil {
			return 0, nil, err
		}
		if k != "" {
			n++
		}
		for _, r := range rs {
			seen[r] = true
		}
	}
	risks := []string{}
	for _, r := range authz.Risks {
		if seen[r] {
			risks = append(risks, r)
		}
	}
	return n, risks, nil
}

var changeMarks = map[string]string{"create": "+", "update": "~", "delete": "-", "replace": "-/+", "forget": "~", "import": "<-"}

// ChangeSign is the sign writ plan --remote prints beside a change of
// kind, as TofuChange names it, "" for a kind that changes nothing.
func ChangeSign(kind string) string { return changeMarks[kind] }

// tofuText is the plan as writ plan --remote prints it: each change, the
// summary OpenTofu prints, the outputs that change, and the warnings. The
// plan's changes passed tofuChanges.
func tofuText(stack, env string, p *mask.Plan, warnings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OpenTofu %s plan of %s@%s\n\n", printable(p.TerraformVersion), stack, env)
	var add, change, destroy, forget, imports int
	for _, c := range p.ResourceChanges {
		k, _, err := TofuChange(c)
		if err != nil || k == "" {
			continue
		}
		line := fmt.Sprintf("%3s %s (%s)", changeMarks[k], printable(c.Address), k)
		if k == "replace" && len(c.Change.ReplacePaths) > 0 {
			var paths []string
			for _, rp := range c.Change.ReplacePaths {
				paths = append(paths, pathText(rp))
			}
			line = fmt.Sprintf("%3s %s (replace, because of %s)", changeMarks[k], printable(c.Address), strings.Join(paths, ", "))
		}
		b.WriteString(line + "\n")
		a := tofuActions[strings.Join(c.Change.Actions, ",")]
		add, change, destroy, forget = add+a.add, change+a.change, destroy+a.destroy, forget+a.forget
		if c.Change.Importing != nil {
			imports++
		}
	}
	if add+change+destroy+forget+imports == 0 {
		b.WriteString("No changes.\n")
	} else {
		b.WriteString("\nPlan: ")
		if imports > 0 {
			fmt.Fprintf(&b, "%d to import, ", imports)
		}
		fmt.Fprintf(&b, "%d to add, %d to change, %d to destroy", add, change, destroy)
		if forget > 0 {
			fmt.Fprintf(&b, ", %d to forget", forget)
		}
		b.WriteString(".\n")
	}
	var outputs []string
	for _, name := range slices.Sorted(maps.Keys(p.OutputChanges)) {
		if a := strings.Join(p.OutputChanges[name].Actions, ","); a != "no-op" {
			outputs = append(outputs, printable(name))
		}
	}
	if len(outputs) > 0 {
		fmt.Fprintf(&b, "Outputs that change: %s.\n", strings.Join(outputs, ", "))
	}
	for _, w := range warnings {
		fmt.Fprintf(&b, "Warning: %s\n", w)
	}
	return b.String()
}

// pathText writes an attribute path of a plan, such as length or
// rule[0].port.
func pathText(path []any) string {
	var b strings.Builder
	for i, step := range path {
		switch v := step.(type) {
		case string:
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(v)
		default:
			fmt.Fprintf(&b, "[%v]", v)
		}
	}
	return printable(b.String())
}

// printable quotes text that holds a character a terminal would act on,
// such as an escape, since a stack's code chooses its addresses and keys.
func printable(s string) string {
	for _, r := range s {
		if !unicode.IsGraphic(r) {
			return strconv.QuoteToGraphic(s)
		}
	}
	return s
}
