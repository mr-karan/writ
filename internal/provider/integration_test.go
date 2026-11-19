package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// The integration tests install real providers into the shared cache
// (WRIT_CACHE_DIR, or the user cache directory) and run them. readTestLock
// pins the archives, so a rerun on a warm cache needs no network for the
// installs, and a test that asks a registry asks cacheRegistry.

var testReqs = []Requirement{
	{Name: "random", Source: randomSrc, Version: "3.9.1"},
	{Name: "tls", Source: tlsSrc, Version: "4.4.2"},
	{Name: "local", Source: localSrc, Version: "2.9.1"},
}

var shared struct {
	once sync.Once
	dirs Dirs
	lock *Lock
	err  error
}

func installed(t *testing.T) (Dirs, *Lock) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: installs and runs real providers")
	}
	shared.once.Do(func() {
		if shared.dirs, shared.err = DefaultDirs(); shared.err != nil {
			return
		}
		pinned, err := readTestLock()
		if err != nil {
			shared.err = err
			return
		}
		reqs := slices.Clone(testReqs)
		for i, r := range reqs {
			if e, ok := pinned.Find(r.Source, r.Version); ok {
				reqs[i].Archives = [][32]byte{e.Archive}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		opts := InstallOptions{Dirs: shared.dirs, Offline: true}
		lock, cached := Install(ctx, opts, reqs)
		if cached != nil {
			opts.Offline = false
			if lock, err = Install(ctx, opts, reqs); err != nil {
				shared.err = errors.Join(cached, err)
				return
			}
		}
		shared.lock = lock
	})
	if shared.err != nil {
		t.Fatal(shared.err)
	}
	return shared.dirs, shared.lock
}

func newTestPool(t *testing.T) *Pool {
	t.Helper()
	dirs, lock := installed(t)
	var configs []Config
	for _, r := range testReqs {
		configs = append(configs, Config{Source: r.Source, Version: r.Version, Value: value.NullVal()})
	}
	p := NewPool(PoolOptions{Dirs: dirs})
	p.Add(lock, configs)
	return p
}

// running returns the clients p has started.
func (p *Pool) running() []*Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*Client
	for _, e := range p.clients {
		if e.client != nil {
			out = append(out, e.client)
		}
	}
	return out
}

// closePool closes p and checks that every provider it started has exited
// and left no socket directory behind.
func closePool(t *testing.T, p *Pool) {
	t.Helper()
	clients := p.running()
	if err := p.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	for _, c := range clients {
		select {
		case <-c.proc.exited:
		default:
			t.Errorf("provider %s is still running after Close", c.src)
		}
		if _, err := os.Stat(c.proc.socketDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("provider %s left its socket directory %s", c.src, c.proc.socketDir)
		}
	}
	if kids := providerChildren(t); len(kids) > 0 {
		t.Errorf("provider processes still running after Close: %v", kids)
	}
}

// providerChildren lists this test process's children whose name starts with
// terraform-provider, from /proc. It returns nothing where /proc is absent.
func providerChildren(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := strconv.Itoa(os.Getpid())
	var kids []string
	for _, e := range entries {
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...; comm may hold spaces and parens.
		open, end := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
		if open < 0 || end < open {
			continue
		}
		fields := strings.Fields(string(stat[end+1:]))
		comm := string(stat[open+1 : end])
		if len(fields) > 1 && fields[1] == self && strings.HasPrefix(comm, "terraform-prov") {
			kids = append(kids, e.Name()+" "+comm)
		}
	}
	return kids
}

func TestIntegrationInstall(t *testing.T) {
	dirs, lock := installed(t)
	pinned, err := readTestLock()
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range testReqs {
		t.Run(req.Name, func(t *testing.T) {
			e, ok := lock.Find(req.Source, req.Version)
			if !ok {
				t.Fatalf("no lock entry for %s %s", req.Source, req.Version)
			}
			data, err := os.ReadFile(dirs.BinaryPath(req.Source, req.Version, e.Platform))
			if sum := sha256.Sum256(data); err != nil || sum != e.Binary {
				t.Errorf("installed binary hash %x, %v; lock says %x", sum, err, e.Binary)
			}
			if p, ok := pinned.Find(req.Source, req.Version); ok && p != e {
				t.Errorf("lock entry differs from the pinned test lock:\n got %+v\nwant %+v", e, p)
			}
			cache, err := openSchemaCache(dirs, e.Binary)
			if err != nil {
				t.Fatalf("schema not cached: %v", err)
			}
			if cs := cache.index; cs.Protocol != 5 {
				t.Errorf("cached schema protocol %d; the registry lists only protocol 5 for %s", cs.Protocol, req.Source)
			}
		})
	}
}

type attrRef struct {
	src       schema.Source
	res, attr string
}

func TestIntegrationSchemasReadOnlyTheCache(t *testing.T) {
	dirs, lock := installed(t)
	// A cache holding only schema files: no provider binary exists, so
	// Schemas cannot have started one.
	isolated := Dirs{Cache: t.TempDir()}
	for _, req := range testReqs {
		e, _ := lock.Find(req.Source, req.Version)
		if err := os.CopyFS(isolated.SchemaDir(e.Binary), os.DirFS(dirs.SchemaDir(e.Binary))); err != nil {
			t.Fatal(err)
		}
	}
	schemas, err := Schemas(isolated, lock, testReqs)
	if err != nil {
		t.Fatal(err)
	}
	attr := func(r attrRef) types.Type {
		t.Helper()
		res, ok := schemas[r.src].Resource(r.res)
		if !ok {
			t.Fatalf("no resource %s", r.res)
		}
		a, ok := res.Block.Lookup([]string{r.attr})
		if !ok {
			t.Fatalf("%s has no attribute %s", r.res, r.attr)
		}
		return a.Type
	}

	typed := []struct {
		at   attrRef
		want string
	}{
		{attrRef{tlsSrc, "PrivateKey", "private_key_pem"}, "Secret<Pem<tls.PrivateKey>>"},
		{attrRef{randomSrc, "Pet", "length"}, "Float"},
		{attrRef{randomSrc, "Pet", "id"}, "Id<random.Pet>"},
		{attrRef{randomSrc, "Pet", "keepers"}, "{String: String}"},
		{attrRef{localSrc, "File", "filename"}, "String"},
	}
	for _, tt := range typed {
		if got := attr(tt.at).String(); got != tt.want {
			t.Errorf("%s.%s: %s, want %s", tt.at.res, tt.at.attr, got, tt.want)
		}
	}

	privateKey := attrRef{tlsSrc, "PrivateKey", "private_key_pem"}
	selfSigned := attrRef{tlsSrc, "SelfSignedCert", "cert_pem"}
	flows := []struct {
		dst, src attrRef
		ok       bool
	}{
		{attrRef{tlsSrc, "SelfSignedCert", "private_key_pem"}, privateKey, true},
		{attrRef{tlsSrc, "CertRequest", "private_key_pem"}, privateKey, true},
		{attrRef{tlsSrc, "LocallySignedCert", "ca_private_key_pem"}, privateKey, true},
		{attrRef{tlsSrc, "LocallySignedCert", "ca_cert_pem"}, selfSigned, true},
		{attrRef{tlsSrc, "LocallySignedCert", "cert_request_pem"}, attrRef{tlsSrc, "CertRequest", "cert_request_pem"}, true},
		{attrRef{tlsSrc, "LocallySignedCert", "cert_request_pem"}, selfSigned, false},
	}
	for _, f := range flows {
		dst, src := attr(f.dst), attr(f.src)
		if m, ok := types.Assign(dst, src); ok != f.ok {
			t.Errorf("%s.%s (%s) <- %s.%s (%s): allowed %v, want %v (%s)", f.dst.res, f.dst.attr, dst, f.src.res, f.src.attr, src, ok, f.ok, m.Reason)
		}
	}

	req, _ := schemas[tlsSrc].Resource("CertRequest")
	i := slices.IndexFunc(req.Block.Blocks, func(nb schema.NestedBlock) bool { return nb.Name == "subject" })
	if i < 0 || !req.Block.Blocks[i].IsRecord() {
		t.Error("tls.CertRequest.subject is not a single record block")
	}
}

func TestIntegrationProtocol5Handshake(t *testing.T) {
	pool := newTestPool(t)
	defer closePool(t, pool)
	ctx := context.Background()

	clients := make([]*Client, 8)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			c, err := pool.Client(ctx, keyOf(randomSrc))
			if err != nil {
				t.Error(err)
			}
			clients[i] = c
		})
	}
	wg.Wait()
	for _, c := range clients[1:] {
		if c != clients[0] {
			t.Fatal("concurrent first calls got different clients")
		}
	}
	if pool.Started() != 1 {
		t.Errorf("Started = %d after eight calls for one provider, want 1", pool.Started())
	}
	if p := clients[0].proc.protocol; p != 5 {
		t.Errorf("random negotiated protocol %d, want 5", p)
	}
}

// TestIntegrationImport reads an object by its import id: random_id takes
// the id's bytes as the object, and random_pet refuses import.
func TestIntegrationImport(t *testing.T) {
	pool := newTestPool(t)
	defer closePool(t, pool)
	ctx := context.Background()
	c, err := pool.Client(ctx, keyOf(randomSrc))
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.Import(ctx, ImportRequest{Type: "random_id", ID: "AQIDBA"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]value.Value{"id": value.StringVal("AQIDBA"), "hex": value.StringVal("01020304"), "byte_length": value.IntVal(4)}
	for name, v := range want {
		if !value.Equal(got.State.Get(name), v) {
			t.Errorf("imported %s = %s, want %s", name, got.State.Get(name), v)
		}
	}
	read, err := c.Read(ctx, ReadRequest{Type: "random_id", Current: got.State, Private: got.Private})
	if err != nil {
		t.Fatal(err)
	}
	if !value.Equal(read.New, got.State) {
		t.Errorf("a read after the import gave %s, want the imported state %s", read.New, got.State)
	}

	_, err = c.Import(ctx, ImportRequest{Type: "random_pet", ID: "calm-owl"})
	var perr *Error
	if !errors.As(err, &perr) || perr.Op != "ImportResourceState" || !strings.Contains(err.Error(), "does not support import") {
		t.Errorf("importing a random_pet: %v, want the provider's refusal as an *Error", err)
	}
	if _, err := c.Import(ctx, ImportRequest{Type: "random_nothing", ID: "x"}); err == nil {
		t.Error("importing a type the provider does not have succeeded")
	}
}

// TestIntegrationTLSPlan plans a key and a request that uses it: the
// provider's sensitive attribute arrives marked secret even while unknown,
// and validation diagnostics keep the path they are about.
func TestIntegrationTLSPlan(t *testing.T) {
	pool := newTestPool(t)
	defer closePool(t, pool)
	ctx := context.Background()
	c, err := pool.Client(ctx, keyOf(tlsSrc))
	if err != nil {
		t.Fatal(err)
	}

	keyCfg := obj(map[string]value.Value{"algorithm": str("ECDSA"), "ecdsa_curve": str("P256")})
	keyPlan, err := c.Plan(ctx, PlanRequest{Type: "tls_private_key", Prior: value.NullVal(), Proposed: keyCfg, Config: keyCfg})
	if err != nil {
		t.Fatal(err)
	}
	pem := keyPlan.Planned.Get("private_key_pem")
	if pem.Kind() != value.Unknown || !pem.IsSecret() {
		t.Errorf("planned private_key_pem = %s, secret %v; want a secret known after apply", pem, pem.IsSecret())
	}
	if pub := keyPlan.Planned.Get("public_key_pem"); pub.IsSecret() {
		t.Error("public_key_pem is marked secret; the schema does not mark it sensitive")
	}

	reqCfg := obj(map[string]value.Value{
		"private_key_pem": pem,
		"subject":         obj(map[string]value.Value{"common_name": str("example.com"), "organization": str("Writ")}),
	})
	diags, err := c.Validate(ctx, "tls_cert_request", reqCfg)
	if err != nil || hasError(diags) {
		t.Fatalf("Validate tls_cert_request: %v %+v", err, diags)
	}
	reqPlan, err := c.Plan(ctx, PlanRequest{Type: "tls_cert_request", Prior: value.NullVal(), Proposed: reqCfg, Config: reqCfg})
	if err != nil {
		t.Fatal(err)
	}
	if subject := reqPlan.Planned.Get("subject"); subject.Kind() != value.Object || !value.Equal(subject.Get("common_name"), str("example.com")) {
		t.Errorf("planned subject = %s, want a record with common_name example.com", subject)
	}

	t.Run("provider diagnostics keep their path", func(t *testing.T) {
		diags, err := c.Validate(ctx, "tls_private_key", obj(map[string]value.Value{"algorithm": str("FOO")}))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range diags {
			if d.Error && d.Path.String() == "algorithm" {
				return
			}
		}
		t.Errorf("diagnostics %+v, want an error about algorithm", diags)
	})
}

// TestIntegrationReadGone reads a local_file whose file does not exist: the
// object is gone, so the read answers null. Only the local provider starts.
func TestIntegrationReadGone(t *testing.T) {
	pool := newTestPool(t)
	defer closePool(t, pool)
	ctx := context.Background()
	c, err := pool.Client(ctx, keyOf(localSrc))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hello.txt")
	current := obj(map[string]value.Value{
		"id":       str("f572d396fae9206628714fb2ce00f72e94f2258f"),
		"filename": str(path),
		"content":  str("hello\n"),
	})
	read, err := c.Read(ctx, ReadRequest{Type: "local_file", Current: current})
	if err != nil {
		t.Fatal(err)
	}
	if read.New.Kind() != value.Null {
		t.Errorf("read of a removed file = %s, want null: the object is gone", read.New)
	}
	if pool.Started() != 1 {
		t.Errorf("Started = %d, want 1: only local was used", pool.Started())
	}
}

// keyOf is the pool key of src's test config, which has a null value.
func keyOf(src schema.Source) ConfigKey {
	for _, r := range testReqs {
		if r.Source == src {
			return ConfigKey{Source: src, Version: r.Version}
		}
	}
	panic("no test requirement for " + src.String())
}

// readTestLock returns the lock of the pinned test providers: the
// linux_amd64 archives the registry served on 2026-11-08.
func readTestLock() (*Lock, error) {
	l := &Lock{}
	for _, e := range []struct {
		src             schema.Source
		version         string
		archive, binary string
	}{
		{localSrc, "2.9.1", "5f25e3e5e58d516e3c81ef1ba712bdab3a40e45028bbb62c7ec98659a6dab65f", "ae9294a55eda6ff85dcbb4461d69b92412d36cdcd3bf89f8493465861e3a66e6"},
		{randomSrc, "3.9.1", "624fb6ed552abc34a5aaac41e76a373da65ac08e524b09b672f29c60e6ac896a", "d8de21d2fc126adcd0bb9582755700809698594f7c9d9417aafb61996c82372d"},
		{tlsSrc, "4.4.2", "9607e7c627014eb6a1d583860b572291283adb50bd6e8be5a7a7821e42b3d31d", "8692a99dc77005a7a86bee6e61bc2ed3de03e81796a2d19446d05a01c82b0b20"},
	} {
		archive, err := parseHash("archive=sha256:"+e.archive, "archive")
		if err != nil {
			return nil, err
		}
		binary, err := parseHash("binary=sha256:"+e.binary, "binary")
		if err != nil {
			return nil, err
		}
		l = l.with(LockEntry{Source: e.src, Version: e.version, Platform: "linux_amd64", Archive: archive, Binary: binary})
	}
	return l, nil
}
