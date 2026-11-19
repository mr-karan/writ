package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/runas"
)

// baseProviderEnv is every provider's environment before the run's own
// variables. The instance metadata switch stops the AWS SDKs from asking
// the host's identity; the firewall rule stops a provider that asks
// anyway.
var baseProviderEnv = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}

// initProviders starts the credential endpoint when an environment names
// a role, and fills the pool of provider uids after it ends what earlier
// runs left running as them.
func (s *Server) initProviders(opts Options) error {
	s.reap = opts.Reap
	if s.reap == nil {
		s.reap = runas.Reap
	}
	if r := s.cfg.ProviderUIDs; r != nil {
		if uid := uint32(os.Getuid()); r.Contains(uid) {
			return fmt.Errorf("writ serve runs as uid %d, which is in provider_uids %s", uid, r)
		}
		s.uids = make(chan uint32, r.Count())
		for uid := r.First; ; uid++ {
			if err := s.reap(uid); err != nil {
				return fmt.Errorf("end the processes of provider uid %d: %w", uid, err)
			}
			s.uids <- uid
			if uid == r.Last {
				break
			}
		}
	}
	roles := false
	for _, e := range s.cfg.Envs {
		roles = roles || e.PlanRole != ""
	}
	if !roles {
		return nil
	}
	if opts.Assumer == nil {
		return errors.New("an environment names a role, and writ serve has no way to assume it")
	}
	b, err := awsrole.NewBroker(awsrole.BrokerOptions{Assumer: opts.Assumer, Now: s.now, Log: s.log.With("component", "credentials"),
		Assumed: s.store.RecordRoleSession})
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

// runLaunch is how the providers of one plan or apply start, and the
// role credentials the broker has served them so far.
type runLaunch struct {
	provider.Launch
	// served returns the secrets of the run's role credentials, nil for a
	// run with no role. The environment holds only the token that fetches
	// them, and a refresh adds to them while the run goes on.
	served func() []string
}

// mask is a mask of the run that knows secrets and follows the role
// credentials served to the run, so it covers those served after it is
// made.
func (l runLaunch) mask(secrets ...[]string) *mask.Mask {
	m := mask.New(slices.Concat(secrets...))
	if l.served != nil {
		m.Follow(l.served)
	}
	return m
}

// launch prepares how the providers of one plan or apply start: an
// environment built from the run alone, with the credentials of role, the
// environment's plan or apply role, behind a token that dies with the run,
// and a uid no other running plan or apply has. done ends the token and
// frees the uid once whatever ran as it has ended. run names the run in
// the role session's name.
func (s *Server) launch(ctx context.Context, run, env string, role roleKind, p *principal) (launch runLaunch, done func(), err error) {
	if s.cfg.Dev {
		// The dev control plane assumes no role and takes no uid. tofuEnv
		// gives its runs the base environment and the shell's TF_VAR_
		// variables.
		return runLaunch{}, func() {}, nil
	}
	e := s.cfg.Envs[env]
	pe, err := readProviderEnv(e)
	if err != nil {
		return runLaunch{}, nil, err
	}
	vars := append(slices.Clone(baseProviderEnv), pe...)
	var undo []func()
	done = func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	arn := e.PlanRole
	if role == applyRole {
		arn = e.ApplyRole
	}
	if arn != "" {
		sess, err := s.broker.Start(ctx, awsrole.Session{
			Role:           arn,
			Name:           awsrole.SessionName(p.sessionName(), run),
			SourceIdentity: awsrole.SourceIdentity(p.sourceIdentity()),
			Duration:       s.cfg.AWS.Session,
		})
		if err != nil {
			return runLaunch{}, nil, &classedError{diag.Denied, fmt.Errorf("the %s role of env %s: %w", role, env, err)}
		}
		undo = append(undo, sess.End)
		vars = append(vars, sess.Env()...)
		launch.served = sess.Secrets
	}
	launch.Launch = provider.Launch{OwnEnv: true, Env: vars}
	user, free, err := s.takeUID(ctx)
	if err != nil {
		done()
		return runLaunch{}, nil, err
	}
	undo = append(undo, free)
	launch.User = user
	return launch, done, nil
}

// takeUID takes a provider uid no other run has, nil when the config sets
// no provider_uids. free ends what still runs as it and returns it.
func (s *Server) takeUID(ctx context.Context) (user *provider.User, free func(), err error) {
	if s.uids == nil {
		return nil, func() {}, nil
	}
	select {
	case uid := <-s.uids:
		return &provider.User{UID: uid, GID: uid}, func() { s.freeUID(uid) }, nil
	case <-ctx.Done():
		return nil, nil, s.noUID(ctx)
	}
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

// roleKind is which of an environment's roles a run's providers get.
type roleKind string

const (
	planRole  roleKind = "plan"
	applyRole roleKind = "apply"
)

// freeUID ends what still runs as uid and returns it to the pool. A uid
// whose processes will not end stays out of the pool.
func (s *Server) freeUID(uid uint32) {
	if err := s.reap(uid); err != nil {
		s.mu.Lock()
		s.withheld++
		withheld := s.withheld
		s.mu.Unlock()
		s.log.Error("provider uid withheld: its processes did not end", "uid", uid, "withheld", withheld, "err", err)
		return
	}
	s.uids <- uid
}

// noUID says why a run got no provider uid before ctx ended.
func (s *Server) noUID(ctx context.Context) error {
	s.mu.Lock()
	withheld := s.withheld
	s.mu.Unlock()
	return &classedError{diag.Usage, fmt.Errorf("%w, while it waited for a provider uid: %d of the %d provider_uids are withheld because their processes did not end, and the others serve running plans and applies",
		context.Cause(ctx), withheld, s.cfg.ProviderUIDs.Count())}
}
