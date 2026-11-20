package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/repo"
	"github.com/mr-karan/writ/internal/runner"
	"github.com/mr-karan/writ/internal/tfstate"
)

// baseProviderEnv is every provider's environment before the run's own
// variables. The instance metadata switch stops the AWS SDKs from asking
// the host's identity; the firewall rule stops a provider that asks
// anyway.
var baseProviderEnv = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}

// initProviders fills the pool of provider uids after it ends what
// earlier runs left running as them, makes the executor of the runs when
// writ serve runs OpenTofu, and starts the credential endpoint when an
// environment names a role.
func (s *Server) initProviders(opts Options) error {
	var uids *runner.UIDs
	if r := s.cfg.ProviderUIDs; r != nil {
		if uid := uint32(os.Getuid()); r.Contains(uid) {
			return fmt.Errorf("writ serve runs as uid %d, which is in provider_uids %s", uid, r)
		}
		var err error
		if uids, err = runner.NewUIDs(r.First, r.Last, opts.Reap, s.log); err != nil {
			return err
		}
	}
	if opts.Tofu != nil {
		s.exec = &runner.Local{Tofu: opts.Tofu, UIDs: uids, Work: filepath.Join(s.cfg.Data, "work"), Relay: s.relay}
		if opts.WrapExecutor != nil {
			s.exec = opts.WrapExecutor(s.exec)
		}
	}
	roles := false
	for _, e := range s.cfg.Envs {
		roles = roles || e.role(planRole) != "" || e.role(applyRole) != ""
	}
	if !roles {
		return nil
	}
	if opts.Assumer == nil {
		return errors.New("an environment names a role, and writ serve has no way to assume it")
	}
	b, err := awsrole.NewBroker(awsrole.BrokerOptions{Assumer: opts.Assumer, Now: s.now, Log: s.log.With("component", "credentials"),
		Assumed: func(ctx context.Context, a awsrole.AssumedSession) error {
			return s.store.RecordRoleSession(ctx, control.RoleSession(a))
		}})
	if err != nil {
		return err
	}
	s.broker = b
	s.log.Info("credential endpoint listening", "url", b.URL())
	return nil
}

func (s *Server) closeProviders() {
	if s.broker != nil {
		s.broker.Close()
	}
}

// classedError is an error with the class its plan document reports.
type classedError struct {
	class diag.Class
	err   error
}

func (e *classedError) Error() string { return e.err.Error() }
func (e *classedError) Unwrap() error { return e.err }

// runSpec is how one OpenTofu run of writ serve starts.
type runSpec struct {
	// id names the run: its work directory and its role session.
	id  string
	env string
	// ts holds the files of commit the run gets, and dir is the stack's
	// directory among them.
	commit string
	ts     repo.TofuStack
	dir    string
	// role is the environment's role whose credentials tofu gets, with
	// the environment's provider_env. A run with no role gets neither:
	// it reads a state it is handed, and its mask still learns the
	// provider_env values, which a state can hold where nothing marks
	// them.
	role roleKind
	p    *principal
	// stateCreds marks a stack on S3, whose backend tofu reaches with the
	// role's credentials.
	stateCreds bool
	// what names the run in the refusal of a config with no
	// provider_uids, "" for a run of writ serve's own.
	what string
}

// tofuRun is an OpenTofu run that startRun prepared: a slot of the
// executor, and an environment built from the run alone.
type tofuRun struct {
	s          *Server
	spec       runSpec
	slot       runner.Slot
	env, creds []string
	// secrets are the run's own: the values writ serve gives it and its
	// state token.
	secrets []string
	// served returns the secrets of the run's role credentials, nil for a
	// run with no role. The environment holds only the token that fetches
	// them, and a refresh adds to them while the run goes on.
	served func() []string
	// net is the network record of the run's job, nil before it ran and
	// under egress_mode off.
	net  *runner.Network
	undo []func()
}

// startRun prepares the run spec describes: it checks the commit's
// files, starts the role session behind a token that dies with the run,
// and reserves a slot of the executor. done ends the session and gives
// the slot back once whatever ran in it has ended. When startRun fails,
// it has undone what it did.
func (s *Server) startRun(ctx context.Context, spec runSpec) (_ *tofuRun, err error) {
	r := &tofuRun{s: s, spec: spec}
	defer func() {
		if err != nil {
			r.done()
		}
	}()
	if err := checkTofuFiles(spec.ts.Files, spec.commit); err != nil {
		return nil, err
	}
	vars, err := s.runVars(spec.env)
	if err != nil {
		return nil, err
	}
	r.secrets = values(vars)
	r.env = baseProviderEnv
	if s.cfg.EgressMode.On() {
		r.env = append(slices.Clone(r.env), runner.EgressEnv...)
	}
	switch {
	case spec.role == "":
	case s.cfg.Dev:
		// The dev control plane assumes no role and has no provider
		// uids. Its runs get the shell's TF_VAR_ variables, which set the
		// stack's variables.
		r.env = append(slices.Clone(r.env), vars...)
	default:
		r.creds = vars
		if arn := s.cfg.Envs[spec.env].role(spec.role); arn != "" {
			sess, err := s.broker.Start(ctx, awsrole.Session{
				Role:           arn,
				Name:           awsrole.SessionName(spec.p.sessionName(), spec.id),
				SourceIdentity: awsrole.SourceIdentity(spec.p.sourceIdentity()),
				Duration:       s.cfg.AWS.Session,
			})
			if err != nil {
				return nil, &classedError{diag.Denied, fmt.Errorf("the %s role of env %s: %w", spec.role, spec.env, err)}
			}
			r.undo = append(r.undo, sess.End)
			env := sess.Env()
			if s.cfg.EgressMode.On() {
				env = sess.EnvAt(credsURL)
			}
			r.creds = append(slices.Clone(r.creds), env...)
			r.secrets = append(r.secrets, values(env)...)
			r.served = sess.Secrets
		}
	}
	if s.cfg.ProviderUIDs == nil && !s.cfg.Dev {
		if spec.what == "" {
			return nil, tofuRefused("this writ serve has no provider_uids to run OpenTofu as, so it would run as writ's own user: set provider_uids")
		}
		return nil, tofuRefused("%s, and this writ serve has no provider_uids to run it as, so it would run as writ's own user: set provider_uids", spec.what)
	}
	slot, err := s.exec.Reserve(ctx)
	if err != nil {
		return nil, classed(err)
	}
	r.undo = append(r.undo, slot.Release)
	r.slot = slot
	return r, nil
}

// done undoes what startRun and stateToken did, the last first.
func (r *tofuRun) done() {
	for i := len(r.undo) - 1; i >= 0; i-- {
		r.undo[i]()
	}
	r.undo = nil
}

// mask is a mask of the run that knows its own secrets and known, and
// follows the role credentials served to the run, so it covers those
// served after it is made.
func (r *tofuRun) mask(known ...[]string) *mask.Mask {
	m := mask.New(slices.Concat(append([][]string{r.secrets}, known...)...))
	if r.served != nil {
		m.Follow(r.served)
	}
	return m
}

// stateToken gives the run a token of scope on the http backend for the
// stack, until ttl from now: tofu reaches the backend with it, the mask
// learns it, and done ends it with the run's locks.
func (r *tofuRun) stateToken(ctx context.Context, stack string, scope control.StateScope, ttl time.Duration) error {
	s := r.s
	token, err := s.tfstate.Issue(ctx, control.StateToken{RunID: r.spec.id, Env: r.spec.env, Stack: stack, Scope: scope, ExpiresAt: s.now().Add(ttl)})
	if err != nil {
		return err
	}
	r.undo = append(r.undo, func() { s.endState(r.spec.id) })
	r.env = append(slices.Clone(r.env), tfstate.ClientEnv(s.stateBase(), stack, token)...)
	r.secrets = append(r.secrets, token)
	return nil
}

// job is the run's job of kind.
func (r *tofuRun) job(kind runner.Kind) runner.Job {
	s := r.s
	e, _ := s.envOf(r.spec.env)
	return runner.Job{ID: r.spec.id, Kind: kind, Files: r.spec.ts.Files, Dir: r.spec.dir, Lock: r.spec.ts.Lock,
		Env: r.env, Creds: r.creds, StateCreds: r.spec.stateCreds,
		Egress: runner.Egress{Mode: s.cfg.EgressMode, List: e.egress, Registries: s.cfg.ProviderRegistries}}
}

// run runs job in the run's slot, with its provider install under start
// and tofu under ctx, and keeps its network record. A job that failed
// before tofu ran, as a failed install, comes back with the output of the
// failed step swept by m, and notStarted reports it.
func (r *tofuRun) run(start, ctx context.Context, job runner.Job, m *mask.Mask) (runner.Result, error) {
	res, err := r.slot.Run(start, ctx, job)
	r.net = res.Network
	if notStarted(err) {
		err = fmt.Errorf("%w%s", err, stepLines(m, err))
	}
	return res, classed(err)
}

// notStarted reports whether err is of a job that failed before tofu ran.
func notStarted(err error) bool {
	_, ok := errors.AsType[*runner.StartError](err)
	return ok
}

// classed gives an executor's refusal the class usage, as writ serve's
// own refusals have.
func classed(err error) error {
	if _, ok := errors.AsType[*runner.Refusal](err); ok {
		return &classedError{diag.Usage, err}
	}
	return err
}

// readProviderEnv reads the provider_env files of e, as NAME=VALUE in the
// order of their names.
func readProviderEnv(e Env) ([]string, error) {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(e.ProviderEnv)) {
		b, err := os.ReadFile(e.ProviderEnv[name])
		if err != nil {
			return nil, fmt.Errorf("provider_env %s of env %s: %w", name, e.Name, err)
		}
		out = append(out, name+"="+strings.TrimSuffix(string(b), "\n"))
	}
	return out, nil
}

// runVars are the variables writ serve gives a run of env as NAME=VALUE:
// its provider_env, or, under --dev, the TF_VAR_ variables of the shell
// writ serve started in, and nothing else of that shell. A variable the
// base environment already holds is not the run's own.
func (s *Server) runVars(env string) ([]string, error) {
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
	return slices.DeleteFunc(vars, func(kv string) bool { return slices.Contains(baseProviderEnv, kv) }), nil
}

// values are the values of variables written NAME=VALUE.
func values(vars []string) []string {
	var out []string
	for _, kv := range vars {
		if _, v, ok := strings.Cut(kv, "="); ok {
			out = append(out, v)
		}
	}
	return out
}

// roleKind is which of an environment's roles a run's providers get.
type roleKind string

const (
	planRole  roleKind = "plan"
	applyRole roleKind = "apply"
)

// role is the ARN of e's role of kind, "" when e names none.
func (e Env) role(kind roleKind) string {
	if kind == applyRole {
		return e.ApplyRole
	}
	return e.PlanRole
}
