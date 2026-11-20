package runner

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/mr-karan/writ/internal/egress"
	"github.com/mr-karan/writ/internal/runas"
	"github.com/mr-karan/writ/internal/tofu"
)

// Local is the executor that runs each job in writ serve's own process:
// tofu as a provider uid no other job has, in the view and the network
// namespaces of internal/tofu, behind an egress proxy of the job's own.
type Local struct {
	Tofu Tofu
	// UIDs are the provider uids that jobs run as. Nil runs every job as
	// writ's own user.
	UIDs *UIDs
	// Work holds the files of each job, in a directory named by its id.
	Work string
	// Relay serves the ports of a job's own network that reach writ
	// serve.
	Relay Relay
}

// Relay returns the handlers of job's state port, the state backend, and
// of its credential port, the credential endpoint, nil when there is
// none.
type Relay func(job string) (state, creds http.Handler)

// Reserve takes a provider uid no other job has.
func (l *Local) Reserve(ctx context.Context) (Slot, error) {
	s := &localSlot{l: l}
	if l.UIDs == nil {
		return s, nil
	}
	uid, err := l.UIDs.take(ctx)
	if err != nil {
		return nil, err
	}
	s.user = &runas.User{UID: uid, GID: uid}
	return s, nil
}

// localSlot is a job's room in Local: its provider uid, nil without
// UIDs.
type localSlot struct {
	l    *Local
	user *runas.User
}

// Release ends what still runs as the slot's uid and gives it back.
func (s *localSlot) Release() {
	if s.user != nil {
		s.l.UIDs.give(s.user.UID)
	}
}

// Run writes the job's files into its work directory, which it removes
// when it returns, installs the providers, and runs tofu as the slot's
// uid.
func (s *localSlot) Run(start, ctx context.Context, job Job) (Result, error) {
	switch job.Kind {
	case Plan, Apply, Show, Pull:
	default:
		return Result{}, &StartError{fmt.Errorf("runner: a job of kind %q", job.Kind)}
	}
	work := filepath.Join(s.l.Work, job.ID)
	defer os.RemoveAll(work)
	if err := writeFiles(work, job.Files); err != nil {
		return Result{}, &StartError{err}
	}
	var proxy *egress.Proxy
	if job.Egress.Mode.On() {
		var err error
		if proxy, err = egress.New(egress.Options{Mode: job.Egress.Mode, List: job.Egress.List}); err != nil {
			return Result{}, &StartError{err}
		}
	}
	lock, install, err := s.fill(start, job)
	if err != nil {
		return Result{Network: record(job.Egress.Mode, install, proxy)}, &StartError{fmt.Errorf("install the providers of %s into the plugin cache: %w", tofu.LockFile, err)}
	}
	stk := tofu.Stack{Files: os.DirFS(work), Dir: job.Dir, Lock: lock, Env: job.Env, Creds: job.Creds, StateCreds: job.StateCreds, User: s.user, Net: s.net(job.ID, proxy)}
	var res Result
	t := s.l.Tofu
	switch job.Kind {
	case Plan:
		res.Plan, err = t.Plan(ctx, stk)
	case Apply:
		res.Applied, err = t.Apply(ctx, stk, job.SavedPlan)
	case Show:
		res.State, err = t.Show(ctx, stk, job.State)
	case Pull:
		res.State, err = t.Pull(ctx, stk)
	}
	res.Network = record(job.Egress.Mode, install, proxy)
	return res, err
}

// fill installs the providers of the job's lock file and returns the lock
// file the job's init reads, with the proxy of the install, nil under
// egress_mode off. The lock file is the stack's, and each provider's
// address starts with the registry the install asks, which chooses the
// hosts it downloads from. So under egress_mode report or enforce a
// provider from a registry that provider_registries does not list is
// refused, and the install runs in a network namespace of its own behind
// a proxy of its own, which allows only public addresses.
func (s *localSlot) fill(ctx context.Context, job Job) ([]byte, *egress.Proxy, error) {
	e := job.Egress
	if !e.Mode.On() {
		lock, err := s.l.Tofu.Fill(ctx, job.Lock, nil, nil)
		return lock, nil, err
	}
	providers, err := tofu.LockedProviders(job.Lock)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range providers {
		if host, _, _ := strings.Cut(p.Source, "/"); !slices.Contains(e.Registries, host) {
			return nil, nil, &Refusal{fmt.Errorf("provider %s is from registry %s, which provider_registries does not list: under egress_mode %s, writ serve installs providers only from %s; add %s to provider_registries to install from it",
				p.Source, host, e.Mode, strings.Join(e.Registries, ", "), host)}
		}
	}
	install, err := egress.New(egress.Options{Mode: e.Mode, Install: true})
	if err != nil {
		return nil, nil, err
	}
	lock, err := s.l.Tofu.Fill(ctx, job.Lock, &tofu.Net{Ports: map[int]http.Handler{EgressPort: install}}, EgressEnv)
	return lock, install, err
}

// net is the job's own network, nil when it has none: the egress proxy,
// the state backend, and the credential endpoint when there is one.
func (s *localSlot) net(job string, proxy *egress.Proxy) *tofu.Net {
	if proxy == nil {
		return nil
	}
	state, creds := s.l.Relay(job)
	n := &tofu.Net{Ports: map[int]http.Handler{EgressPort: proxy, StatePort: state}}
	if creds != nil {
		n.Ports[CredsPort] = creds
	}
	return n
}

// record is the network record of a job's proxies, nil when it has none.
// The install runs before every step of the job.
func record(mode egress.Mode, install, proxy *egress.Proxy) *Network {
	if proxy == nil {
		return nil
	}
	n := &Network{Mode: mode}
	if install != nil {
		var unrecorded int
		n.Install, unrecorded = install.Destinations()
		n.Unrecorded += unrecorded
	}
	var unrecorded int
	n.Run, unrecorded = proxy.Destinations()
	n.Unrecorded += unrecorded
	return n
}

// writeFiles writes files into the directory work, which it makes, and
// through an os.Root of it, so no path reaches outside it.
func writeFiles(work string, files map[string][]byte) error {
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer root.Close()
	for name, text := range files {
		if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
			return err
		}
		if err := root.WriteFile(name, text, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// UIDs is a pool of provider uids: a job takes one no other job has, and
// gives it back once every process that ran as it has ended.
type UIDs struct {
	free chan uint32
	reap func(uid uint32) error
	log  *slog.Logger
	mu   sync.Mutex
	// withheld counts the uids kept out of the pool because their
	// processes did not end.
	withheld int
}

// NewUIDs fills a pool with the uids first through last, after it ends
// what earlier runs left running as each. reap kills every process of a
// uid, and nil means runas.Reap.
func NewUIDs(first, last uint32, reap func(uid uint32) error, log *slog.Logger) (*UIDs, error) {
	if reap == nil {
		reap = runas.Reap
	}
	p := &UIDs{free: make(chan uint32, int(last-first)+1), reap: reap, log: log}
	for uid := first; ; uid++ {
		if err := reap(uid); err != nil {
			return nil, fmt.Errorf("end the processes of provider uid %d: %w", uid, err)
		}
		p.free <- uid
		if uid == last {
			return p, nil
		}
	}
}

// take takes a uid no other job has, and says why it got none before
// ctx ended.
func (p *UIDs) take(ctx context.Context) (uint32, error) {
	select {
	case uid := <-p.free:
		return uid, nil
	case <-ctx.Done():
		p.mu.Lock()
		withheld := p.withheld
		p.mu.Unlock()
		return 0, &Refusal{fmt.Errorf("%w, while it waited for a provider uid: %d of the %d provider_uids are withheld because their processes did not end, and the others serve running plans and applies",
			context.Cause(ctx), withheld, cap(p.free))}
	}
}

// give ends what still runs as uid and returns it to the pool. A uid
// whose processes will not end stays out of the pool.
func (p *UIDs) give(uid uint32) {
	if err := p.reap(uid); err != nil {
		p.mu.Lock()
		p.withheld++
		withheld := p.withheld
		p.mu.Unlock()
		p.log.Error("provider uid withheld: its processes did not end", "uid", uid, "withheld", withheld, "err", err)
		return
	}
	p.free <- uid
}
