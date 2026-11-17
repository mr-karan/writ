package importer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/tfcode"
	"github.com/mr-karan/writ/internal/tofu"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// TofuOptions configure OpenTofuStack.
type TofuOptions struct {
	Dirs   provider.Dirs
	Log    *slog.Logger
	Launch provider.Launch
	// Offline installs nothing that Writ's provider cache lacks.
	Offline bool
}

// TofuStack is an OpenTofu stack that a bulk import reads objects for: the
// providers its .terraform.lock.hcl pins, each at the version the lock
// names and checked against the lock's hashes, configured by the stack's
// provider blocks. It reads every object through the providers and writes
// nothing to the stack. It is a Reader and a Lister, safe for concurrent
// use, valid until Close.
type TofuStack struct {
	code    *tfcode.Stack
	lock    *provider.Lock
	schemas map[schema.Source]*schema.Provider
	pool    *provider.Pool
	hasher  value.Hasher

	mu      sync.Mutex
	configs map[tfcode.ProviderRef]*tofuConfig
}

// tofuConfig is one provider configuration, resolved on first use.
type tofuConfig struct {
	key   provider.ConfigKey
	value value.Value
	err   error
}

// TofuRequirements are the providers that the lock file of the stack in
// dir pins, each at its version and admitting only the archives whose zh:
// hashes the lock lists.
func TofuRequirements(dir string, locked []tofu.Provider) ([]provider.Requirement, error) {
	reqs := make([]provider.Requirement, len(locked))
	for i, p := range locked {
		src, err := schema.ParseSource(p.Source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", tofu.LockFile, err)
		}
		// Not nil, so a lock with no zh: hash accepts no archive.
		archives := [][32]byte{}
		for _, h := range p.Hashes {
			if hexed, ok := strings.CutPrefix(h, "zh:"); ok {
				if b, err := hex.DecodeString(hexed); err == nil && len(b) == 32 {
					archives = append(archives, [32]byte(b))
				}
			}
		}
		reqs[i] = provider.Requirement{StackDir: dir, Name: src.Type, Source: src, Version: p.Version, Archives: archives}
	}
	return reqs, nil
}

// IsTofuStack reports whether dir holds an OpenTofu stack: a
// .terraform.lock.hcl, OpenTofu code, and no .writ file.
func IsTofuStack(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, tofu.LockFile)); err != nil {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	code := false
	for _, e := range entries {
		switch filepath.Ext(e.Name()) {
		case ".writ":
			return false
		case ".tf", ".tofu":
			code = code || e.Type().IsRegular()
		}
	}
	return code
}

// OpenTofuStack reads the stack in dir and installs the providers its lock
// pins into Writ's provider cache. It starts no provider.
func OpenTofuStack(ctx context.Context, dir string, opts TofuOptions) (*TofuStack, error) {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	lockText, err := os.ReadFile(filepath.Join(dir, tofu.LockFile))
	if err != nil {
		return nil, fmt.Errorf("read the stack's providers: %w; run tofu init in the stack, which writes it", err)
	}
	locked, err := tofu.LockedProviders(lockText)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		if strings.HasSuffix(name, ".tf.json") || strings.HasSuffix(name, ".tofu.json") {
			return nil, fmt.Errorf("%s: writ import reads only .tf and .tofu files, and OpenTofu would read this one too, so names it declares could clash with the code it writes; convert it to .tf or move it out of the stack", filepath.Join(dir, name))
		}
		if ext != ".tf" && ext != ".tofu" {
			continue
		}
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file: writ import reads only regular .tf and .tofu files, and OpenTofu would follow it, so names it declares could clash with the code it writes; replace it with the file it points to", filepath.Join(dir, name))
		}
		if files[name], err = os.ReadFile(filepath.Join(dir, name)); err != nil {
			return nil, err
		}
	}
	code, err := tfcode.Read(files)
	if err != nil {
		return nil, err
	}
	reqs, err := TofuRequirements(dir, locked)
	if err != nil {
		return nil, err
	}
	// The lock's hashes pin each archive, so a cached provider needs no
	// registry, and Install starts none whose archive they do not list.
	install := provider.InstallOptions{Dirs: opts.Dirs, Log: opts.Log, Offline: true, Launch: opts.Launch}
	lock, err := provider.Install(ctx, install, nil, reqs)
	if err != nil && !opts.Offline {
		install.Offline = false
		lock, err = provider.Install(ctx, install, nil, reqs)
	}
	if errors.Is(err, provider.ErrUnlistedArchive) {
		return nil, fmt.Errorf("%w; run tofu providers lock -platform=%s in the stack, and check what it changes", err, provider.CurrentPlatform())
	}
	if err != nil {
		return nil, err
	}
	schemas, err := provider.Schemas(opts.Dirs, lock, reqs)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &TofuStack{
		code: code, lock: lock, schemas: schemas, hasher: value.NewHasher(key),
		pool:    provider.NewPool(provider.PoolOptions{Dirs: opts.Dirs, Lock: lock, Log: opts.Log, Launch: opts.Launch}),
		configs: map[tfcode.ProviderRef]*tofuConfig{},
	}, nil
}

// Close stops every provider the stack started.
func (s *TofuStack) Close(ctx context.Context) error { return s.pool.Close(ctx) }

// Taken are the names the stack's code declares: of its resources, of any
// type, and of its variables.
func (s *TofuStack) Taken() []string {
	var out []string
	for _, names := range s.code.Resources {
		for name := range names {
			out = append(out, name)
		}
	}
	for name := range s.code.Variables {
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Type is the type a selection names by the provider's name for it, such
// as random_uuid. Its provider is the local name OpenTofu implies: the
// name up to the first underscore.
func (s *TofuStack) Type(wire string) (types.Name, error) {
	local, _, ok := strings.Cut(wire, "_")
	if !ok {
		return types.Name{}, fmt.Errorf("type %q: want a resource type, such as random_uuid", wire)
	}
	src := s.code.Source(local)
	p, ok := s.schemas[src]
	if !ok {
		return types.Name{}, fmt.Errorf("type %q: %s does not pin provider %s", wire, tofu.LockFile, src)
	}
	r, err := p.ResourceByWire(wire)
	if err != nil {
		return types.Name{}, fmt.Errorf("type %q: %w", wire, err)
	}
	return types.Name{Provider: local, Type: r.Name.Type}, nil
}

// resource is the schema and the configuration of an object of typ read
// through via, the provider configuration a selection names: zero for the
// one without an alias, else NAME.ALIAS.
func (s *TofuStack) resource(typ types.Name, via addr.Addr) (*schema.Resource, schema.Source, tfcode.ProviderRef, error) {
	ref := tfcode.ProviderRef{Name: typ.Provider}
	if !via.IsZero() {
		name, alias, ok := strings.Cut(via.String(), ".")
		if !ok || name != typ.Provider || !addr.IsIdent(alias) {
			return nil, schema.Source{}, ref, fmt.Errorf("provider %s: want %s.ALIAS, the alias of a provider block of %s", via, typ.Provider, typ.Provider)
		}
		ref.Alias = alias
		if _, ok := s.code.Providers[ref]; !ok {
			return nil, schema.Source{}, ref, fmt.Errorf("the stack has no provider block %s", ref)
		}
	}
	src := s.code.Source(typ.Provider)
	p, ok := s.schemas[src]
	if !ok {
		return nil, src, ref, fmt.Errorf("%s does not pin provider %s", tofu.LockFile, src)
	}
	r, ok := p.Resource(typ.Type)
	if !ok {
		return nil, src, ref, fmt.Errorf("provider %s has no resource type %s", src, typ)
	}
	return r, src, ref, nil
}

// config resolves the provider configuration ref once.
func (s *TofuStack) config(ref tfcode.ProviderRef, src schema.Source) *tofuConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.configs[ref]; ok {
		return c
	}
	c := &tofuConfig{}
	s.configs[ref] = c
	e, _ := s.lock.Find(src, s.version(src))
	cfg, err := s.code.Providers[ref].Config(s.schemas[src].Config)
	if err != nil {
		c.err = err
		return c
	}
	canon, err := value.Canonical(cfg)
	if err != nil {
		c.err = fmt.Errorf("provider %s: %w", ref, err)
		return c
	}
	c.value = cfg
	c.key = provider.ConfigKey{Source: src, Version: e.Version, Digest: s.hasher.Sum(canon)}
	s.pool.Add(s.lock, []provider.Config{{Source: src, Version: e.Version, Value: cfg, Digest: c.key.Digest}})
	return c
}

func (s *TofuStack) version(src schema.Source) string {
	for _, e := range s.lock.Entries() {
		if e.Source == src {
			return e.Version
		}
	}
	return ""
}

// Prepare resolves the provider configuration of every selection before
// any read, so a provider block writ import cannot read refuses the run
// first, and Secrets holds every secret of the configurations.
func (s *TofuStack) Prepare(sels []Selection) error {
	for _, sel := range sels {
		_, src, ref, err := s.resource(sel.Type, sel.Provider)
		if err != nil {
			return err
		}
		if c := s.config(ref, src); c.err != nil {
			return c.err
		}
	}
	return nil
}

// Secrets are the secret values of the provider configurations the import
// used, so no code or message holds one.
func (s *TofuStack) Secrets() []value.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []value.Value
	for _, c := range s.configs {
		if c.err == nil {
			out = append(out, secretLeaves(c.value)...)
		}
	}
	return out
}

func secretLeaves(v value.Value) []value.Value {
	if v.IsSecret() {
		return []value.Value{v}
	}
	var out []value.Value
	switch v.Kind() {
	case value.List:
		for i := range v.Len() {
			out = append(out, secretLeaves(v.Index(i))...)
		}
	case value.Object:
		for _, k := range v.Keys() {
			out = append(out, secretLeaves(v.Get(k))...)
		}
	}
	return out
}

// client starts or finds the provider of an object of typ read through via.
func (s *TofuStack) client(ctx context.Context, typ types.Name, via addr.Addr) (*schema.Resource, schema.Source, *tofuConfig, *provider.Client, error) {
	r, src, ref, err := s.resource(typ, via)
	if err != nil {
		return nil, src, nil, nil, err
	}
	c := s.config(ref, src)
	if c.err != nil {
		return nil, src, nil, nil, c.err
	}
	client, err := s.pool.Client(ctx, c.key)
	if err != nil {
		return nil, src, nil, nil, s.subject(r, c).Error(err, s.redactor(c))
	}
	return r, src, c, client, nil
}

func (s *TofuStack) subject(r *schema.Resource, c *tofuConfig, vals ...value.Value) provider.Subject {
	var b *schema.Block
	if r != nil {
		b = &r.Block
	}
	if c != nil {
		vals = append(vals, c.value)
	}
	return provider.Subject{Block: b, Values: vals}
}

func (s *TofuStack) redactor(c *tofuConfig, vals ...value.Value) value.Redactor {
	var red value.Redactor
	if c != nil {
		red = red.With(c.value)
	}
	return red.With(vals...)
}

// shown is the provider's diagnostics as the rule of provider.Subject.Show
// prints them.
func shown(ds []provider.Diag, sub provider.Subject, red value.Redactor) diag.List {
	var out diag.List
	for _, d := range ds {
		sh := sub.Show(d)
		sev := diag.Warning
		if d.Error {
			sev = diag.Error
		}
		pd := diag.Diagnostic{Severity: sev, Class: diag.Provider, Message: sh.Text}
		if sh.Withheld {
			pd.Fix = &diag.Fix{Message: provider.WithheldHint}
		} else {
			pd.Message = red.Text(sh.Text)
		}
		out = append(out, pd)
	}
	return out
}

// failed is a failed provider call as a refusal.
func failed(err error, sub provider.Subject, red value.Redactor) diag.List {
	if pe, ok := errors.AsType[*provider.Error](err); ok && len(pe.Diags) > 0 {
		return shown(pe.Diags, sub, red)
	}
	var out diag.List
	out.Errorf(diag.Provider, diag.Span{}, "%s", sub.Error(err, red))
	return out
}

// Read reads the object id names, as tofu plan reads it for an import block.
func (s *TofuStack) Read(ctx context.Context, typ types.Name, via addr.Addr, id string) (Found, diag.List, error) {
	r, src, c, client, err := s.client(ctx, typ, via)
	if cerr := ctx.Err(); cerr != nil {
		return Found{}, nil, cerr
	}
	if err != nil {
		var d diag.List
		d.Errorf(diag.Provider, diag.Span{}, "%s", err)
		return Found{}, d, nil
	}
	at := time.Now()
	obj, err := client.Import(ctx, provider.ImportRequest{Type: r.Wire, ID: id, OneRead: true})
	if cerr := ctx.Err(); cerr != nil {
		return Found{}, nil, cerr
	}
	sub, red := s.subject(r, c, obj.State), s.redactor(c, obj.State)
	if err != nil {
		return Found{}, failed(err, sub, red), nil
	}
	diags := shown(obj.Warnings, sub, red)
	if obj.State.Kind() == value.Null {
		diags.Errorf(diag.Provider, diag.Span{}, "provider %s finds no %s with that import id", src, r.Wire)
		return Found{}, diags, nil
	}
	return Found{Schema: r, Source: src, State: obj.State, Private: obj.Private, Identity: obj.Identity, ReadAt: at, ProviderConfig: c.value}, diags, nil
}

// List lists the import ids of the objects of typ that the provider finds
// with query as its list config.
func (s *TofuStack) List(ctx context.Context, typ types.Name, via addr.Addr, query value.Value, limit int) ([]string, diag.List, error) {
	r, _, c, client, err := s.client(ctx, typ, via)
	if cerr := ctx.Err(); cerr != nil {
		return nil, nil, cerr
	}
	var diags diag.List
	if err != nil {
		diags.Errorf(diag.Provider, diag.Span{}, "%s", err)
		return nil, diags, nil
	}
	sub, red := provider.Subject{Values: []value.Value{query, c.value}}, s.redactor(c, query)
	ids, warnings, err := client.List(ctx, r.Wire, query, limit)
	if cerr := ctx.Err(); cerr != nil {
		return nil, nil, cerr
	}
	if nl, ok := errors.AsType[*provider.NoListError](err); ok {
		diags.Errorf(diag.Provider, diag.Span{}, "%s; select them by ids, values, or from", nl)
		return nil, diags, nil
	}
	if err != nil {
		return nil, failed(err, sub, red), nil
	}
	return ids, shown(warnings, sub, red), nil
}

// Validator asks the provider of typ whether it accepts a config.
func (s *TofuStack) Validator(typ types.Name, via addr.Addr) Validator {
	return func(ctx context.Context, wire string, cfg value.Value) ([]provider.Diag, error) {
		_, _, _, client, err := s.client(ctx, typ, via)
		if err != nil {
			return nil, err
		}
		return client.Validate(ctx, wire, cfg)
	}
}

// Planner plans a config against the object as Read returned it, as tofu
// plan does after it imports the object.
func (s *TofuStack) Planner(typ types.Name, via addr.Addr, found Found) Planner {
	prior := found.State
	return func(ctx context.Context, cfg value.Value) (Plan, error) {
		r, _, c, client, err := s.client(ctx, typ, via)
		if err != nil {
			return Plan{}, err
		}
		resp, err := client.Plan(ctx, provider.PlanRequest{Type: r.Wire, Prior: prior, Proposed: provider.ProposeNew(r.Block, prior, cfg), Config: cfg, PriorPrivate: found.Private, PriorIdentity: found.Identity})
		if err != nil {
			return Plan{}, s.subject(r, c, prior, cfg).Error(err, s.redactor(c, prior, cfg))
		}
		return Plan{Value: resp.Planned, RequiresReplace: resp.RequiresReplace, Private: resp.Private}, nil
	}
}
