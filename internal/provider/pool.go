package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

// Config is one resolved provider configuration.
type Config struct {
	Source  schema.Source
	Version string
	Value   value.Value // wholly known; secret leaves marked
	// Digest identifies Value. The caller computes it with a keyed
	// hasher, so the pool never hashes a secret itself.
	Digest value.Digest
}

// ConfigKey names one running provider process: the source, the version,
// and the digest of the resolved config. Two configurations with one key,
// in one stack or across stacks, share a process. There is no way to ask
// for a provider by source alone, so a resource can never reach another
// configuration's process.
type ConfigKey struct {
	Source  schema.Source
	Version string
	Digest  value.Digest
}

// Key returns the key the pool starts c under.
func (c Config) Key() ConfigKey {
	return ConfigKey{Source: c.Source, Version: c.Version, Digest: c.Digest}
}

func (k ConfigKey) String() string { return k.Source.String() + " " + k.Version }

// PoolOptions configures a Pool.
type PoolOptions struct {
	Dirs    Dirs
	Lock    *Lock
	Configs []Config
	Log     *slog.Logger
	// Launch is how the pool starts each provider process.
	Launch Launch
}

// Pool starts each provider at most once per command, on first use. A plan
// whose every resource is a cache hit never calls Client, so it starts no
// process.
type Pool struct {
	opts PoolOptions
	// mu guards configs, clients, started, closed, and each entry's client
	// and err once its start has finished.
	mu sync.Mutex
	// configs holds every configuration the pool may start, each with the
	// writ.lock that pins its binary.
	configs map[ConfigKey]lockedConfig
	clients map[ConfigKey]*poolEntry
	started int
	closed  bool
}

type lockedConfig struct {
	Config
	lock *Lock
}

type poolEntry struct {
	once   sync.Once
	client *Client
	err    error
}

var errPoolClosed = errors.New("provider pool is closed")

// NewPool returns an empty pool.
func NewPool(opts PoolOptions) *Pool {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Lock == nil {
		opts.Lock = &Lock{}
	}
	p := &Pool{opts: opts, configs: map[ConfigKey]lockedConfig{}, clients: make(map[ConfigKey]*poolEntry)}
	p.Add(opts.Lock, opts.Configs)
	return p
}

// Add lets the pool start configs, each pinned by lock. A run over several
// stacks shares one pool: each stack adds its configurations with its own
// writ.lock, and two stacks whose configurations have one key share one
// process. A key the pool has already keeps its first lock.
func (p *Pool) Add(lock *Lock, configs []Config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range configs {
		if _, ok := p.configs[c.Key()]; !ok {
			p.configs[c.Key()] = lockedConfig{Config: c, lock: lock}
		}
	}
}

// Client returns the running, configured client for key, starting it on
// first use. The first caller's ctx governs the start, and its outcome,
// success or error, is what every later caller gets.
func (p *Pool) Client(ctx context.Context, key ConfigKey) (*Client, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errPoolClosed
	}
	e, ok := p.clients[key]
	if !ok {
		e = &poolEntry{}
		p.clients[key] = e
	}
	p.mu.Unlock()
	e.once.Do(func() {
		c, err := p.start(ctx, key)
		p.mu.Lock()
		e.client, e.err = c, err
		p.mu.Unlock()
	})
	return e.client, e.err
}

func (p *Pool) start(ctx context.Context, key ConfigKey) (*Client, error) {
	p.mu.Lock()
	cfg, ok := p.configs[key]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("provider %s is not configured in this run", key)
	}
	src := key.Source
	missing := &MissingError{Missing: []Requirement{{Source: src, Version: cfg.Version}}}
	entry, ok := cfg.lock.Find(src, cfg.Version)
	if !ok {
		return nil, missing
	}
	bin := p.opts.Dirs.BinaryPath(src, cfg.Version, entry.Platform)
	sum, err := binaryHash(bin, true, p.opts.Log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, missing
	}
	if err != nil {
		return nil, err
	}
	if sum != entry.Binary {
		return nil, fmt.Errorf("provider %s %s: %s does not match its hash in %s; delete it and run the command again to install it afresh", src, cfg.Version, bin, LockFile)
	}
	cache, err := openSchemaCache(p.opts.Dirs, entry.Binary)
	if errors.Is(err, errNoSchema) {
		return nil, missing
	}
	if err != nil {
		return nil, err
	}
	sch, wire, err := cache.build(src, cfg.Version)
	if err != nil {
		return nil, err
	}
	launch := p.opts.Launch
	if job, err := sch.ResourceByWire("nomad_job"); err == nil {
		launch.nomadPlanResponse = job.PatchedNomad()
	}
	proc, err := start(ctx, bin, launch)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.started++
	p.mu.Unlock()
	c := &Client{src: src, proc: proc, proto: proc.client(), wire: wire, nomadPatched: launch.nomadPlanResponse}
	err = c.ready(ctx, cache.index.GetSchemaOptional)
	if err == nil {
		err = c.configure(ctx, cfg.Value)
	}
	if err != nil {
		_ = proc.shutdown(context.WithoutCancel(ctx))
		return nil, err
	}
	p.opts.Log.Debug("started provider", "source", src.String(), "version", cfg.Version, "protocol", proc.protocol)
	return c, nil
}

// Started returns how many provider processes this pool started.
func (p *Pool) Started() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started
}

// Close shuts every provider down and waits for the processes to exit,
// killing any that outlive the context. A start still in progress finishes
// first and is then shut down; no provider starts after Close.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	entries := make([]*poolEntry, 0, len(p.clients))
	for _, e := range p.clients {
		entries = append(entries, e)
	}
	p.mu.Unlock()
	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Go(func() {
			e.once.Do(func() {
				p.mu.Lock()
				e.err = errPoolClosed
				p.mu.Unlock()
			})
			p.mu.Lock()
			c := e.client
			p.mu.Unlock()
			if c != nil {
				errs[i] = c.proc.shutdown(ctx)
			}
		})
	}
	wg.Wait()
	p.opts.Log.Debug("closed providers", "started", p.Started())
	return errors.Join(errs...)
}
