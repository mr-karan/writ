package importer_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/tofu"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// These tests run Bulk on hand-built objects against the real schemas of
// hashicorp/random and local. A stand-in reader returns the objects, and a
// stand-in lister lists them, because no test provider serves
// ListResource. internal/cli tests the same code against real reads.

var versions = map[string]string{"random": "3.9.1", "tls": "4.4.2", "local": "2.9.1"}

var installedCache struct {
	once sync.Once
	dirs provider.Dirs
	err  error
}

// installed puts the providers of internal/tofu's test lock in Writ's
// cache. As OpenTofuStack does, it takes them from the cache, pinned by the
// lock's zh: hashes, and asks the registry only when the cache lacks one,
// so a warm cache needs no network.
func installed(t *testing.T) provider.Dirs {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: reads the schemas of real providers")
	}
	c := &installedCache
	c.once.Do(func() {
		if c.dirs, c.err = provider.DefaultDirs(); c.err != nil {
			return
		}
		lock, err := os.ReadFile("../tofu/testdata/lock.hcl")
		if err != nil {
			c.err = err
			return
		}
		locked, err := tofu.LockedProviders(lock)
		if err != nil {
			c.err = err
			return
		}
		reqs, err := importer.TofuRequirements("", locked)
		if err != nil {
			c.err = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		opts := provider.InstallOptions{Dirs: c.dirs, Offline: true}
		if _, cached := provider.Install(ctx, opts, nil, reqs); cached != nil {
			opts.Offline = false
			if _, err := provider.Install(ctx, opts, nil, reqs); err != nil {
				c.err = errors.Join(cached, err)
			}
		}
	})
	if c.err != nil {
		t.Fatal(c.err)
	}
	return c.dirs
}

func schemas(t *testing.T) map[schema.Source]*schema.Provider {
	t.Helper()
	dirs := installed(t)
	var reqs []provider.Requirement
	for name, v := range versions {
		reqs = append(reqs, provider.Requirement{Name: name, Source: schema.Source{Namespace: "hashicorp", Type: name}, Version: v})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	lock, err := provider.Install(ctx, provider.InstallOptions{Dirs: dirs, Offline: true}, nil, reqs)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := provider.Schemas(dirs, lock, reqs)
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

func resourceOf(t *testing.T, ss map[schema.Source]*schema.Provider, typ types.Name) (*schema.Resource, schema.Source) {
	t.Helper()
	src := schema.Source{Namespace: "hashicorp", Type: typ.Provider}
	r, ok := ss[src].Resource(typ.Type)
	if !ok {
		t.Fatalf("no type %s", typ)
	}
	return r, src
}

// fakeReader answers reads from a table, by type and id.
type fakeReader struct {
	objects map[string]importer.Found
	reads   atomic.Int64
}

func (f *fakeReader) Read(_ context.Context, typ types.Name, _ addr.Addr, id string) (importer.Found, diag.List, error) {
	f.reads.Add(1)
	o, ok := f.objects[typ.String()+" "+id]
	if !ok {
		var d diag.List
		d.Errorf(diag.Provider, diag.Span{}, "no %s with that id", typ)
		return importer.Found{}, d, nil
	}
	return o, nil, nil
}

// Planner says the stand-in cannot plan, so the code keeps every "".
func (f *fakeReader) Planner(types.Name, addr.Addr, importer.Found) importer.Planner { return nil }

func (f *fakeReader) Validator(types.Name, addr.Addr) importer.Validator {
	return func(context.Context, string, value.Value) ([]provider.Diag, error) { return nil, nil }
}

// textReader refuses every read with text, as a provider that echoes a
// value it was given.
type textReader struct {
	fakeReader
	text string
}

func (e *textReader) Read(context.Context, types.Name, addr.Addr, string) (importer.Found, diag.List, error) {
	var d diag.List
	d.Errorf(diag.Provider, diag.Span{}, "%s", e.text)
	return importer.Found{}, d, nil
}

// fakeLister lists the ids it holds, at most limit, or refuses with its
// diagnostics.
type fakeLister struct {
	ids     []string
	diags   diag.List
	limits  []int
	queries []value.Value
}

func (f *fakeLister) List(_ context.Context, _ types.Name, _ addr.Addr, query value.Value, limit int) ([]string, diag.List, error) {
	f.limits = append(f.limits, limit)
	f.queries = append(f.queries, query)
	if f.diags.HasErrors() {
		return nil, f.diags, nil
	}
	return f.ids[:min(limit, len(f.ids))], nil, nil
}

var (
	uuidType    = types.Name{Provider: "random", Type: "Uuid"}
	integerType = types.Name{Provider: "random", Type: "Integer"}
	sensitive   = types.Name{Provider: "local", Type: "SensitiveFile"}
)

func uuidObj(t *testing.T, ss map[schema.Source]*schema.Provider, id string) importer.Found {
	r, src := resourceOf(t, ss, uuidType)
	return importer.Found{Schema: r, Source: src, State: value.ObjectVal(map[string]value.Value{
		"id": value.StringVal(id), "result": value.StringVal(id), "keepers": value.NullVal(),
	})}
}

func integerObj(t *testing.T, ss map[schema.Source]*schema.Provider, result int64, seed string) importer.Found {
	r, src := resourceOf(t, ss, integerType)
	return importer.Found{Schema: r, Source: src, State: value.ObjectVal(map[string]value.Value{
		"id": value.StringVal(strings.Repeat("9", 8) + string(rune('0'+result))), "result": value.IntVal(result),
		"min": value.IntVal(1), "max": value.IntVal(10), "seed": value.StringVal(seed), "keepers": value.NullVal(),
	})}
}

func TestBulkReferences(t *testing.T) {
	ss := schemas(t)
	const (
		u1      = "11111111-aaaa-4bbb-8ccc-000000000001"
		u2      = "11111111-aaaa-4bbb-8ccc-000000000002"
		shortID = "short1"
	)
	ints := map[string]importer.Found{
		// seed refers to a uuid of the run.
		"1": integerObj(t, ss, 1, u1),
		// seed equals a uuid id shorter than 8 bytes in a plain String
		// slot, so it stays a literal.
		"3": integerObj(t, ss, 3, shortID),
		// The members of one group refer to each other's ids: a reference
		// inside the group would be a cycle, so both stay literals.
		"5": integerObj(t, ss, 5, "999999996"),
		"6": integerObj(t, ss, 6, "999999995"),
	}
	r := &fakeReader{objects: map[string]importer.Found{}}
	for _, id := range []string{u1, u2, shortID} {
		r.objects["random.Uuid "+id] = uuidObj(t, ss, id)
	}
	for id, f := range ints {
		r.objects["random.Integer "+id] = f
	}
	res, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{
			{Type: integerType, IDs: []string{"1", "3", "5", "6"}},
			{Type: uuidType, IDs: []string{u1, u2, shortID}},
		},
		Reader: r,
		File:   "imported.tf",
	})
	if err != nil {
		t.Fatal(err)
	}
	code := strings.Join(strings.Fields(string(res.Code)), " ")
	for _, want := range []string{
		`"1" = { seed = random_uuid.random_uuid["11111111-aaaa-4bbb-8ccc-000000000001"].id }`,
		`"3" = { seed = "short1" }`,
		`"5" = { seed = "999999996" }`,
		`"6" = { seed = "999999995" }`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("the code lacks %s:\n%s", want, code)
		}
	}
	if res.Linked != 1 || res.Groups != 2 || res.Singles != 0 {
		t.Errorf("linked %d, groups %d, singles %d; want 1, 2, 0:\n%s", res.Linked, res.Groups, res.Singles, code)
	}
}

func TestBulkSecretsStayOutOfGroups(t *testing.T) {
	ss := schemas(t)
	r, src := resourceOf(t, ss, sensitive)
	file := func(name, content string) importer.Found {
		fields := map[string]value.Value{
			"filename": value.StringVal("/srv/" + name), "content": value.StringVal(content).Secret(),
			"file_permission": value.StringVal("0700"), "directory_permission": value.StringVal("0700"),
			"id": value.StringVal(name + "-0123456789abcdef"),
		}
		return importer.Found{Schema: r, Source: src, State: value.ObjectVal(fields)}
	}
	reader := &fakeReader{objects: map[string]importer.Found{
		"local.SensitiveFile a": file("a", "first-secret-content"),
		"local.SensitiveFile b": file("b", "second-secret-content"),
	}}
	res, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{{Type: sensitive, IDs: []string{"a", "b"}}},
		Reader:     reader, File: "imported.tf",
	})
	if err != nil {
		t.Fatal(err)
	}
	code := string(res.Code)
	if res.Groups != 0 || res.Singles != 2 || len(res.Params) != 2 {
		t.Fatalf("groups %d, singles %d, params %+v; want two lets, each with a param:\n%s", res.Groups, res.Singles, res.Params, code)
	}
	for _, secret := range []string{"first-secret-content", "second-secret-content"} {
		if strings.Contains(code, secret) {
			t.Errorf("the code holds a secret:\n%s", code)
		}
	}
	if len(res.ParamValues) != 2 || !slices.Contains(mapValues(res.ParamValues), "first-secret-content") {
		t.Errorf("param values = %d entries, want both contents for checking the code", len(res.ParamValues))
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestBulkList(t *testing.T) {
	ss := schemas(t)
	ids := []string{"44444444-aaaa-4bbb-8ccc-000000000003", "44444444-aaaa-4bbb-8ccc-000000000001", "44444444-aaaa-4bbb-8ccc-000000000002"}
	objects := map[string]importer.Found{}
	for _, id := range ids {
		objects[uuidType.String()+" "+id] = uuidObj(t, ss, id)
	}
	query := value.ObjectVal(map[string]value.Value{"tag": value.StringVal("web")})
	sel := []importer.Selection{{Type: uuidType, List: query}}

	t.Run("reads each listed id into one for", func(t *testing.T) {
		l := &fakeLister{ids: ids}
		r := &fakeReader{objects: objects}
		res, err := importer.Bulk(context.Background(), importer.BulkOptions{Selections: sel, Reader: r, Lister: l, File: "imported.tf"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Objects) != 3 || res.Groups != 1 || !slices.Equal(l.limits, []int{importer.DefaultMaxObjects + 1}) || r.reads.Load() != 3 {
			t.Fatalf("objects %d, groups %d, limits %v, reads %d:\n%s", len(res.Objects), res.Groups, l.limits, r.reads.Load(), res.Code)
		}
		if !value.Equal(l.queries[0], query) {
			t.Errorf("query = %v, want the selection's", l.queries[0])
		}
		if res.Objects[0].Addr.String() != `random_uuid.random_uuid["44444444-aaaa-4bbb-8ccc-000000000001"]` || res.Objects[0].Origin != "objects[0].list[1]" || res.Objects[0].ImportID != ids[1] {
			t.Errorf("first object = %+v, want keys sorted", res.Objects[0])
		}
	})
	t.Run("stops at the limit before any read", func(t *testing.T) {
		l := &fakeLister{ids: ids}
		r := &fakeReader{objects: objects}
		_, err := importer.Bulk(context.Background(), importer.BulkOptions{Selections: sel, Reader: r, Lister: l, MaxObjects: 2, File: "imported.tf"})
		var limit *importer.LimitError
		if !errors.As(err, &limit) || limit.What != "objects" || !slices.Equal(l.limits, []int{3}) || r.reads.Load() != 0 {
			t.Fatalf("err = %v, limits %v, reads %d; want an object limit after asking for 3, and no read", err, l.limits, r.reads.Load())
		}
	})
	t.Run("the limit counts objects read before the list", func(t *testing.T) {
		l := &fakeLister{ids: ids[1:]}
		r := &fakeReader{objects: objects}
		_, err := importer.Bulk(context.Background(), importer.BulkOptions{
			Selections: []importer.Selection{{Type: uuidType, IDs: ids[:1]}, sel[0]},
			Reader:     r, Lister: l, MaxObjects: 2, File: "imported.tf",
		})
		var limit *importer.LimitError
		if !errors.As(err, &limit) || !slices.Equal(l.limits, []int{3}) {
			t.Fatalf("err = %v, limits %v; want an object limit after asking for 3", err, l.limits)
		}
	})
	t.Run("ids selected before do not count again", func(t *testing.T) {
		// Two listings of one object fit a limit of 1: the second asks
		// for one more id per id already selected, and skips it.
		l := &fakeLister{ids: ids[:1]}
		r := &fakeReader{objects: objects}
		res, err := importer.Bulk(context.Background(), importer.BulkOptions{
			Selections: []importer.Selection{sel[0], sel[0]},
			Reader:     r, Lister: l, MaxObjects: 1, File: "imported.tf",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Objects) != 1 || len(res.Problems) != 1 || res.Problems[0].Kind != importer.Skipped || !slices.Equal(l.limits, []int{2, 2}) || r.reads.Load() != 1 {
			t.Fatalf("objects %d, problems %+v, limits %v, reads %d; want one object, one skip, limits [2 2], one read", len(res.Objects), res.Problems, l.limits, r.reads.Load())
		}
	})
	t.Run("a full listing of known ids still finds the limit", func(t *testing.T) {
		// One id selected, room for one more: the listing asks for 3. A
		// full answer holds at least 2 new ids, which pass the limit.
		l := &fakeLister{ids: ids}
		r := &fakeReader{objects: objects}
		_, err := importer.Bulk(context.Background(), importer.BulkOptions{
			Selections: []importer.Selection{{Type: uuidType, IDs: ids[:1]}, sel[0]},
			Reader:     r, Lister: l, MaxObjects: 2, File: "imported.tf",
		})
		var limit *importer.LimitError
		if !errors.As(err, &limit) || !slices.Equal(l.limits, []int{3}) || r.reads.Load() != 1 {
			t.Fatalf("err = %v, limits %v, reads %d; want an object limit after asking for 3", err, l.limits, r.reads.Load())
		}
	})
	t.Run("a listing that repeats an id cannot hide one past the limit", func(t *testing.T) {
		// The provider has A, A, B and stops at the limit of 2 events:
		// two events of one object do not prove the list ends there.
		l := &fakeLister{ids: []string{ids[0], ids[0], ids[1]}}
		r := &fakeReader{objects: objects}
		res, err := importer.Bulk(context.Background(), importer.BulkOptions{Selections: sel, Reader: r, Lister: l, MaxObjects: 1, File: "imported.tf"})
		if err != nil || !res.Failed() || len(res.Objects) != 0 || r.reads.Load() != 0 || !strings.Contains(res.Problems[0].Reason, "repeat") {
			t.Fatalf("err %v, objects %d, problems %+v, reads %d; want the entry refused before any read", err, len(res.Objects), res.Problems, r.reads.Load())
		}
	})
	t.Run("an id listed again is skipped", func(t *testing.T) {
		l := &fakeLister{ids: ids}
		r := &fakeReader{objects: objects}
		res, err := importer.Bulk(context.Background(), importer.BulkOptions{
			Selections: []importer.Selection{{Type: uuidType, IDs: ids[:1]}, sel[0]},
			Reader:     r, Lister: l, File: "imported.tf",
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Failed() || len(res.Objects) != 3 || len(res.Problems) != 1 || res.Problems[0].From != "objects[1].list[0]" || res.Problems[0].Kind != importer.Skipped || r.reads.Load() != 3 {
			t.Fatalf("objects %d, problems %+v, reads %d; want 3 objects, one skip, and 3 reads", len(res.Objects), res.Problems, r.reads.Load())
		}
	})
	t.Run("a listed id that is not readable", func(t *testing.T) {
		l := &fakeLister{ids: []string{ids[0], "44444444-aaaa-4bbb-8ccc-000000000009"}}
		res, err := importer.Bulk(context.Background(), importer.BulkOptions{Selections: sel, Reader: &fakeReader{objects: objects}, Lister: l, File: "imported.tf"})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Failed() || res.Problems[0].From != "objects[0].list[1]" || res.Problems[0].Kind != importer.NotImported {
			t.Fatalf("problems = %+v, want objects[0].list[1] not imported", res.Problems)
		}
	})
	t.Run("a refusal of a query that holds a secret is withheld", func(t *testing.T) {
		const secret = "u21-list-filter-secret"
		const key = "u21-list-secret-key"
		for name, q := range map[string]value.Value{
			"value":   value.ObjectVal(map[string]value.Value{"tag": value.StringVal(secret).Secret()}),
			"map key": value.ObjectVal(map[string]value.Value{"tags": value.ObjectVal(map[string]value.Value{key: value.StringVal("web")}).Secret()}),
		} {
			t.Run(name, func(t *testing.T) {
				var d diag.List
				d.Errorf(diag.Provider, diag.Span{}, "no filter named %q and %s; key %q", secret, secret, key)
				l := &fakeLister{diags: d}
				res, err := importer.Bulk(context.Background(), importer.BulkOptions{
					Selections: []importer.Selection{{Type: uuidType, List: q}},
					Reader:     &fakeReader{objects: objects}, Lister: l, File: "imported.tf",
				})
				if err != nil {
					t.Fatal(err)
				}
				r := res.Problems[0].Reason
				if !res.Failed() || strings.Contains(r, secret) || strings.Contains(r, key) || strings.Contains(r, "no filter named") {
					t.Fatalf("reason = %q, want the provider's text withheld", r)
				}
			})
		}
	})
	t.Run("a secret of the query is a secret of the run", func(t *testing.T) {
		// The provider echoes the secret in an unrelated object's refusal.
		const secret = "u21-list-filter-secret"
		var d diag.List
		d.Errorf(diag.Provider, diag.Span{}, "no object named %s", secret)
		res, err := importer.Bulk(context.Background(), importer.BulkOptions{
			Selections: []importer.Selection{
				{Type: uuidType, IDs: []string{"44444444-aaaa-4bbb-8ccc-000000000009"}},
				{Type: uuidType, List: value.ObjectVal(map[string]value.Value{"tag": value.StringVal(secret).Secret()})},
			},
			Reader: &textReader{text: "no object named " + secret}, Lister: &fakeLister{}, File: "imported.tf",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range res.Problems {
			if strings.Contains(p.Reason, secret) {
				t.Fatalf("problem %+v holds the query's secret", p)
			}
		}
	})
	t.Run("a type the provider does not list", func(t *testing.T) {
		var d diag.List
		d.Errorf(diag.Provider, diag.Span{}, "hashicorp/random does not list objects of random_uuid; select them by ids, values, or from")
		l := &fakeLister{diags: d}
		r := &fakeReader{objects: objects}
		res, err := importer.Bulk(context.Background(), importer.BulkOptions{Selections: sel, Reader: r, Lister: l, File: "imported.tf"})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Failed() || res.Problems[0].From != "objects[0]" || !strings.Contains(res.Problems[0].Reason, "does not list objects of random_uuid") || r.reads.Load() != 0 {
			t.Fatalf("problems = %+v, reads %d; want objects[0] not imported with the lister's reason", res.Problems, r.reads.Load())
		}
	})
}

// TestBulkLimitCountsReads requires that ids whose reads fail count toward
// the object limit, so a selection of bad ids cannot read without bound,
// and that an entry that would pass the limit reads nothing.
func TestBulkLimitCountsReads(t *testing.T) {
	r := &fakeReader{objects: map[string]importer.Found{}}
	_, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{
			{Type: uuidType, IDs: []string{"a", "b"}},
			{Type: integerType, IDs: []string{"c"}},
		},
		Reader: r, MaxObjects: 2, Parallelism: 4,
	})
	var limit *importer.LimitError
	if !errors.As(err, &limit) || limit.Got != 3 || r.reads.Load() != 2 {
		t.Fatalf("err = %v after %d reads, want an object limit at 3 after the 2 failed reads", err, r.reads.Load())
	}
}

// slowReader answers from a table after a delay that is longest for the
// first id, so reads end in the reverse of the order they start, and it
// records how many reads were in flight at once.
type slowReader struct {
	fakeReader
	delay            map[string]time.Duration
	inFlight, peakIn atomic.Int64
}

func (s *slowReader) Read(ctx context.Context, typ types.Name, config addr.Addr, id string) (importer.Found, diag.List, error) {
	n := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		peak := s.peakIn.Load()
		if n <= peak || s.peakIn.CompareAndSwap(peak, n) {
			break
		}
	}
	select {
	case <-time.After(s.delay[id]):
	case <-ctx.Done():
		return importer.Found{}, nil, ctx.Err()
	}
	return s.fakeReader.Read(ctx, typ, config, id)
}

// TestBulkReadsInParallel requires that reads run at most Parallelism at a
// time and that the code does not depend on which read ends first.
func TestBulkReadsInParallel(t *testing.T) {
	ss := schemas(t)
	var ids []string
	newReader := func() *slowReader {
		r := &slowReader{fakeReader: fakeReader{objects: map[string]importer.Found{}}, delay: map[string]time.Duration{}}
		for i, id := range ids {
			r.objects["random.Uuid "+id] = uuidObj(t, ss, id)
			r.delay[id] = time.Duration(len(ids)-i) * 5 * time.Millisecond
		}
		return r
	}
	for i := range 8 {
		ids = append(ids, fmt.Sprintf("55555555-aaaa-4bbb-8ccc-%012d", i))
	}
	var codes []string
	for _, par := range []int{1, 3} {
		r := newReader()
		res, err := importer.Bulk(context.Background(), importer.BulkOptions{
			Selections: []importer.Selection{{Type: uuidType, IDs: ids}},
			Reader:     r, Parallelism: par, File: "imported.tf",
		})
		if err != nil {
			t.Fatal(err)
		}
		if peak := r.peakIn.Load(); peak > int64(par) || par > 1 && peak < 2 {
			t.Errorf("parallelism %d: %d reads in flight at once", par, peak)
		}
		if len(res.Objects) != len(ids) {
			t.Fatalf("parallelism %d: %d objects, want %d", par, len(res.Objects), len(ids))
		}
		codes = append(codes, string(res.Code))
	}
	if codes[0] != codes[1] {
		t.Fatalf("parallel reads changed the code:\n%s\n---\n%s", codes[0], codes[1])
	}
}

// TestBulkSkips requires that an id a from entry derives again, an object
// the stack already manages, and an id template with no value are each
// skipped with a reason, and that a skip does not fail the run.
func TestBulkSkips(t *testing.T) {
	ss := schemas(t)
	const a = "66666666-aaaa-4bbb-8ccc-000000000001"
	r := &fakeReader{objects: map[string]importer.Found{"random.Uuid " + a: uuidObj(t, ss, a)}}
	tmpl, err := importer.ParseTemplate("{name}")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := importer.ParseTemplate("{parent.id}")
	if err != nil {
		t.Fatal(err)
	}
	res, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{
			{Type: uuidType, IDs: []string{a}},
			{Type: uuidType, From: uuidType, ID: parent},
			{Type: uuidType, Values: []map[string]value.Value{{"name": value.NullVal()}}, ID: tmpl},
			{Type: uuidType, From: integerType, ID: tmpl},
		},
		Reader: r,
		File:   "imported.tf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed() || len(res.Objects) != 1 || res.Objects[0].ID != a {
		t.Fatalf("objects %+v, problems %+v; want only %s imported", res.Objects, res.Problems, a)
	}
	want := map[string]string{
		"objects[1].from[0]":   "names the same object as objects[0].ids[0], which this run imports",
		"objects[2].values[0]": "{name} has no value",
		"objects[3]":           "this run read no random.Integer object to derive ids from",
	}
	for _, p := range res.Problems {
		if p.Kind != importer.Skipped || want[p.From] != p.Reason {
			t.Errorf("problem %+v (%s), want a skip that says %q", p, p.Kind, want[p.From])
		}
		delete(want, p.From)
	}
	if len(want) != 0 {
		t.Errorf("no skip for %v", want)
	}
}

// TestBulkDuplicateIsAUsageError requires that an object the user names
// twice, in ids or in values, stops the run with an error that names both
// entries, and that the run reads nothing more.
func TestBulkDuplicateIsAUsageError(t *testing.T) {
	ss := schemas(t)
	const a = "66666666-aaaa-4bbb-8ccc-000000000003"
	tmpl, err := importer.ParseTemplate("{u}")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		sels        []importer.Selection
		first, more string
	}{
		{name: "ids", first: "objects[0].ids[0]", more: "objects[0].ids[1]", sels: []importer.Selection{{Type: uuidType, IDs: []string{a, a}}}},
		{name: "values after ids", first: "objects[0].ids[0]", more: "objects[1].values[0]", sels: []importer.Selection{
			{Type: uuidType, IDs: []string{a}},
			{Type: uuidType, Values: []map[string]value.Value{{"u": value.StringVal(a)}}, ID: tmpl},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeReader{objects: map[string]importer.Found{"random.Uuid " + a: uuidObj(t, ss, a)}}
			_, err := importer.Bulk(context.Background(), importer.BulkOptions{Selections: tc.sels, Reader: r})
			dup, ok := errors.AsType[*importer.DuplicateError](err)
			if !ok || dup.First != tc.first || dup.Again != tc.more {
				t.Fatalf("err = %v, want a duplicate of %s at %s", err, tc.first, tc.more)
			}
			if strings.Contains(err.Error(), a) {
				t.Errorf("the error %q names the id, which may be a secret", err)
			}
		})
	}
}

// TestBulkTwoIDsOfOneObjectIsAUsageError requires that two ids that read
// one object stop the run with an error that names both.
func TestBulkTwoIDsOfOneObjectIsAUsageError(t *testing.T) {
	ss := schemas(t)
	const u = "66666666-aaaa-4bbb-8ccc-000000000004"
	r := &fakeReader{objects: map[string]importer.Found{"random.Uuid " + u: uuidObj(t, ss, u), "random.Uuid alias": uuidObj(t, ss, u)}}
	_, err := importer.Bulk(context.Background(), importer.BulkOptions{Selections: []importer.Selection{{Type: uuidType, IDs: []string{u, "alias"}}}, Reader: r})
	dup, ok := errors.AsType[*importer.DuplicateError](err)
	if !ok || !dup.Read || dup.First != "objects[0].ids[0]" || dup.Again != "objects[0].ids[1]" {
		t.Fatalf("err = %v, want two ids of one object refused", err)
	}
}

// failingReader fails the read of one id with an error that stops the run,
// and holds every other read until the run is cancelled.
type failingReader struct {
	fakeReader
	fail string
	err  error
}

func (f *failingReader) Read(ctx context.Context, _ types.Name, _ addr.Addr, id string) (importer.Found, diag.List, error) {
	if id == f.fail {
		return importer.Found{}, nil, f.err
	}
	<-ctx.Done()
	return importer.Found{}, nil, ctx.Err()
}

// TestBulkReadErrorStopsTheRun requires that an error from one read stops
// the reads in flight and is the error Bulk returns.
func TestBulkReadErrorStopsTheRun(t *testing.T) {
	stop := errors.New("the provider exited")
	r := &failingReader{fail: "c", err: stop}
	_, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{{Type: uuidType, IDs: []string{"a", "b", "c", "d"}}},
		Reader:     r, Parallelism: 4,
	})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
}

var (
	passwordType = types.Name{Provider: "random", Type: "Password"}
	stringType   = types.Name{Provider: "random", Type: "String"}
)

// passwordObj is a random_password as its provider imports it: the id is
// "none" and the import id is the result, a secret.
func passwordObj(t *testing.T, ss map[schema.Source]*schema.Provider, result string) importer.Found {
	r, src := resourceOf(t, ss, passwordType)
	return importer.Found{Schema: r, Source: src, State: value.ObjectVal(map[string]value.Value{
		"id": value.StringVal("none"), "result": value.StringVal(result).Secret(), "length": value.IntVal(int64(len(result))),
		"bcrypt_hash": value.StringVal("$2a$10$" + strings.Repeat("h", 53)).Secret(), "keepers": value.NullVal(),
	})}
}

// stringObj is a random_string as its provider imports it: the import id
// is the result, which is not sensitive.
func stringObj(t *testing.T, ss map[schema.Source]*schema.Provider, result string) importer.Found {
	r, src := resourceOf(t, ss, stringType)
	return importer.Found{Schema: r, Source: src, State: value.ObjectVal(map[string]value.Value{
		"id": value.StringVal(result), "result": value.StringVal(result), "length": value.IntVal(int64(len(result))), "keepers": value.NullVal(),
	})}
}

// TestBulkRefusesAnotherObjectsSecret imports objects that hold, outside a
// sensitive attribute, a secret that another object of the run or the
// stack keeps. Each is refused, so nothing is generated, and no problem
// repeats the secret.
func TestBulkRefusesAnotherObjectsSecret(t *testing.T) {
	ss := schemas(t)
	const (
		secret = "u20-synthetic-secret"
		u1     = "77777777-aaaa-4bbb-8ccc-000000000001"
	)
	keyedObj := stringObj(t, ss, "keyed")
	keyedObj.State = withAttr(keyedObj.State, "keepers", value.ObjectVal(map[string]value.Value{secret: value.StringVal("x")}))
	r := &fakeReader{objects: map[string]importer.Found{
		"random.Password " + secret:    passwordObj(t, ss, secret),
		"random.String " + secret:      stringObj(t, ss, secret),
		"random.String another-public": stringObj(t, ss, "another-public"),
		"random.Integer 5":             integerObj(t, ss, 5, secret),
		"random.String keyed":          keyedObj,
	}}
	tmpl, err := importer.ParseTemplate("{x}")
	if err != nil {
		t.Fatal(err)
	}
	password := importer.Selection{Type: passwordType, IDs: []string{secret}}
	for _, tc := range []struct {
		name string
		opts importer.BulkOptions
		// refused is the origin of the object that holds the secret.
		refused string
	}{
		{name: "a string's id is a password of the run", refused: "objects[1].ids[0]", opts: importer.BulkOptions{Selections: []importer.Selection{
			password, {Type: stringType, IDs: []string{secret, "another-public"}},
		}}},
		{name: "an integer of another group has the password as its seed", refused: "objects[1].ids[0]", opts: importer.BulkOptions{Selections: []importer.Selection{
			password, {Type: integerType, IDs: []string{"5"}},
		}}},
		{name: "a template writes the password as an id", refused: "objects[1].values[0]", opts: importer.BulkOptions{Selections: []importer.Selection{
			password, {Type: stringType, Values: []map[string]value.Value{{"x": value.StringVal(secret)}}, ID: tmpl},
		}}},
		{name: "a map key is the password", refused: "objects[1].ids[0]", opts: importer.BulkOptions{Selections: []importer.Selection{
			password, {Type: stringType, IDs: []string{"keyed"}},
		}}},
		{name: "a Secret param is the id", refused: "objects[0].ids[0]", opts: importer.BulkOptions{
			Selections: []importer.Selection{{Type: stringType, IDs: []string{secret}}},
			Secrets:    []value.Value{value.StringVal(secret).Secret()},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Reader, tc.opts.File = r, "imported.tf"
			res, err := importer.Bulk(context.Background(), tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Failed() || len(res.Code) != 0 || len(res.Objects) != 0 {
				t.Fatalf("problems %+v, objects %+v, code:\n%s\nwant the run refused", res.Problems, res.Objects, res.Code)
			}
			refused := false
			for _, p := range res.Problems {
				if strings.Contains(p.Reason, secret) || strings.Contains(p.From, secret) {
					t.Errorf("problem %+v repeats the secret", p)
				}
				if p.Kind == importer.NotImported && p.From == tc.refused && strings.Contains(p.Reason, "keeps, and writ never writes a secret") {
					refused = true
				}
			}
			if !refused {
				t.Errorf("problems %+v, want %s refused", res.Problems, tc.refused)
			}
		})
	}
}

// withAttr returns the object v with attribute k set to x.
func withAttr(v value.Value, k string, x value.Value) value.Value {
	fields := map[string]value.Value{}
	for _, f := range v.Keys() {
		fields[f] = v.Get(f)
	}
	fields[k] = x
	return value.ObjectVal(fields)
}

// TestBulkSecretIsDefinedByType imports, beside a password the stack keeps, objects that
// repeat it only as a short part, only as an attribute's name, or in
// another case. A secret is a value in a sensitive attribute, and a value
// in an attribute that is not sensitive is not one. A value is refused
// when it equals the password, or holds a password of eight bytes or more;
// TestBulkRefusesAContainedSecret covers that. The comparison is
// case-sensitive. docs/import-bulk.md says so.
func TestBulkSecretIsDefinedByType(t *testing.T) {
	ss := schemas(t)
	for _, tc := range []struct {
		name, password, other string
	}{
		{name: "a seven-byte password inside an id", password: "hunter2", other: "prefix-hunter2-suffix"},
		{name: "a long password in another case", password: "U20-Synthetic-Secret", other: "u20-synthetic-secret"},
		{name: "a password that is an attribute name", password: "result", other: "unrelated-public-value"},
		{name: "a password that is a field name", password: "keepers", other: "keepers-public"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeReader{objects: map[string]importer.Found{"random.String " + tc.other: stringObj(t, ss, tc.other)}}
			res, err := importer.Bulk(context.Background(), importer.BulkOptions{
				Selections: []importer.Selection{{Type: stringType, IDs: []string{tc.other}}},
				Secrets:    []value.Value{passwordObj(t, ss, tc.password).State.Get("result")},
				Reader:     r, File: "imported.tf",
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Failed() || len(res.Objects) != 1 || res.Objects[0].ID != tc.other {
				t.Fatalf("objects %+v, problems %+v; want the string imported", res.Objects, res.Problems)
			}
		})
	}
}

// TestBulkExpansionsAreBounded requires that a run stops expanding id
// templates at ExpansionsPerObject times its object limit, before any
// read, even when every expansion is skipped.
func TestBulkExpansionsAreBounded(t *testing.T) {
	ss := schemas(t)
	tmpl, err := importer.ParseTemplate("{u}")
	if err != nil {
		t.Fatal(err)
	}
	var recs []map[string]value.Value
	for range importer.ExpansionsPerObject + 1 {
		recs = append(recs, map[string]value.Value{"u": value.NullVal()})
	}
	r := &fakeReader{objects: map[string]importer.Found{}}
	_, err = importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{{Type: uuidType, Values: recs, ID: tmpl}},
		Reader:     r, MaxObjects: 1,
	})
	var limit *importer.LimitError
	if !errors.As(err, &limit) || limit.What != "expansions" || r.reads.Load() != 0 {
		t.Fatalf("err = %v after %d reads, want an expansion limit and no read", err, r.reads.Load())
	}

	// A list that a provider returned is bounded the same way.
	const parent = "88888888-aaaa-4bbb-8ccc-000000000001"
	keepers := map[string]value.Value{}
	for i := range 50 {
		keepers[fmt.Sprintf("k%02d", i)] = value.StringVal("x")
	}
	p := uuidObj(t, ss, parent)
	p.State = value.ObjectVal(map[string]value.Value{"id": value.StringVal(parent), "result": value.StringVal(parent), "keepers": value.ObjectVal(keepers)})
	over, err := importer.ParseTemplate("{parent.id}{item.missing}")
	if err != nil {
		t.Fatal(err)
	}
	r = &fakeReader{objects: map[string]importer.Found{"random.Uuid " + parent: p}}
	_, err = importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{
			{Type: uuidType, IDs: []string{parent}},
			{Type: stringType, From: uuidType, Over: value.Path{}.Attr("keepers"), ID: over},
		},
		Reader: r, MaxObjects: 2,
	})
	if !errors.As(err, &limit) || limit.What != "expansions" || limit.Limit != 2*importer.ExpansionsPerObject || r.reads.Load() != 1 {
		t.Fatalf("err = %v after %d reads, want an expansion limit of %d after the parent's read", err, r.reads.Load(), 2*importer.ExpansionsPerObject)
	}
}

// TestBulkKeyOrderIsTotal imports ids that are one number, "1" and "01",
// in both orders, and requires the same code with "01" first.
func TestBulkKeyOrderIsTotal(t *testing.T) {
	ss := schemas(t)
	r := &fakeReader{objects: map[string]importer.Found{"random.Uuid 1": uuidObj(t, ss, "1"), "random.Uuid 01": uuidObj(t, ss, "01")}}
	var codes []string
	for _, ids := range [][]string{{"1", "01"}, {"01", "1"}} {
		res, err := importer.Bulk(context.Background(), importer.BulkOptions{Reader: r, Selections: []importer.Selection{{Type: uuidType, IDs: ids}}, File: "imported.tf"})
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, string(res.Code))
	}
	if codes[0] != codes[1] || !strings.Contains(codes[0], `toset(["01", "1"])`) {
		t.Fatalf("the order of the ids changed the code, or 01 is not first:\n%s\n---\n%s", codes[0], codes[1])
	}
}

// TestBulkNamesHoldNoAccountID imports objects whose ids hold an AWS
// account id or are ARNs. A name or key built from such an id would put
// the account into code, so bulk numbers them; an id with no account id
// stays the key.
func TestBulkNamesHoldNoAccountID(t *testing.T) {
	ss := schemas(t)
	for _, tc := range []struct {
		name     string
		ids      []string
		numbered bool
		want     []string // addresses, in the order of ids
	}{
		{name: "account ids", numbered: true, ids: []string{"123456789012-a", "123456789012-b"}, want: []string{`random_uuid.random_uuid["1"]`, `random_uuid.random_uuid["2"]`}},
		{name: "an ARN", numbered: true, ids: []string{"arn:aws:sts::123456789012:example"}, want: []string{"random_uuid.random_uuid_1"}},
		{name: "longer runs of digits", ids: []string{"1234567890120-a", "1234567890120-b"}, want: []string{`random_uuid.random_uuid["1234567890120-a"]`, `random_uuid.random_uuid["1234567890120-b"]`}},
		// A UUID whose last group is twelve digits holds no account id,
		// but only a whole UUID: one that no letter or digit touches.
		{name: "UUIDs", ids: []string{"app-01234567-89ab-4def-8123-123456789012", "01234567-89ab-4def-8123-123456789012"}, want: []string{`random_uuid.random_uuid["app-01234567-89ab-4def-8123-123456789012"]`, `random_uuid.random_uuid["01234567-89ab-4def-8123-123456789012"]`}},
		{name: "a last group of thirteen hex digits", numbered: true, ids: []string{"app-01234567-89ab-4def-8123-123456789012f", "app-01234567-89ab-4def-8123-123456789012e"}, want: []string{`random_uuid.random_uuid["1"]`, `random_uuid.random_uuid["2"]`}},
		{name: "a UUID glued to a letter", numbered: true, ids: []string{"app01234567-89ab-4def-8123-123456789012"}, want: []string{"random_uuid.random_uuid_1"}},
		{name: "a UUID", ids: []string{"app-01234567-89ab-4def-8123-123456789012"}, want: []string{"random_uuid.random_uuid_app_01234567_89ab_4def_8123_123456789012"}},
		{name: "a last group of thirteen hex digits, alone", numbered: true, ids: []string{"app-01234567-89ab-4def-8123-123456789012f"}, want: []string{"random_uuid.random_uuid_1"}},
		{name: "a UUID glued to digits", numbered: true, ids: []string{"901234567-89ab-4def-8123-123456789012", "001234567-89ab-4def-8123-123456789012"}, want: []string{`random_uuid.random_uuid["1"]`, `random_uuid.random_uuid["2"]`}},
		// Cutting a long label to 64 bytes must not leave twelve digits
		// that the whole label did not have.
		{name: "a label cut to twelve digits", ids: []string{strings.Repeat("x", 40) + "1234567890120"}, want: []string{"random_uuid.random_uuid"}},
		{name: "an account id beside a UUID", numbered: true, ids: []string{"123456789012-01234567-89ab-4def-8123-0123456789ab", "01234567-89ab-4def-8123-0123456789ab_123456789012"}, want: []string{`random_uuid.random_uuid["1"]`, `random_uuid.random_uuid["2"]`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeReader{objects: map[string]importer.Found{}}
			for _, id := range tc.ids {
				r.objects["random.Uuid "+id] = uuidObj(t, ss, id)
			}
			res, err := importer.Bulk(context.Background(), importer.BulkOptions{Reader: r, Selections: []importer.Selection{{Type: uuidType, IDs: tc.ids}}, File: "imported.tf"})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, o := range res.Objects {
				got[o.ImportID] = o.Addr.String()
			}
			var addrs []string
			for _, id := range tc.ids {
				addrs = append(addrs, got[id])
			}
			// The hash of each id orders numbered objects, so only the set
			// of their addresses is fixed.
			if tc.numbered {
				slices.Sort(addrs)
			}
			if !slices.Equal(addrs, tc.want) {
				t.Fatalf("addresses = %q, want %q", addrs, tc.want)
			}
		})
	}
}

// TestBulkSecretContainerIsOneParam reads objects whose schema marks a map
// and a list sensitive. Each container is one secret param, keys included:
// no key or element of it is in the code, a param's name or attribute, or
// a problem.
func TestBulkSecretContainerIsOneParam(t *testing.T) {
	const key, elem, item = "u20-secret-map-key", "u20-secret-map-value", "u20-secret-list-item"
	typ := types.Name{Provider: "vault", Type: "Secret"}
	res := &schema.Resource{Name: typ, Wire: "vault_secret", Block: schema.Block{Attributes: []schema.Attribute{
		{Name: "id", Type: types.TString, Computed: true},
		{Name: "name", Type: types.TString, Required: true},
		{Name: "data", Type: types.MapOf(types.TString), Optional: true, Sensitive: true},
		{Name: "tokens", Type: types.ListOf(types.TString), Optional: true, Sensitive: true},
	}}}
	src := schema.Source{Namespace: "hashicorp", Type: "vault"}
	obj := func(name string) importer.Found {
		return importer.Found{Schema: res, Source: src, State: value.ObjectVal(map[string]value.Value{
			"id": value.StringVal(name), "name": value.StringVal(name),
			"data":   value.ObjectVal(map[string]value.Value{key: value.StringVal(elem)}).Secret(),
			"tokens": value.ListVal([]value.Value{value.StringVal(item)}).Secret(),
		})}
	}
	r := &fakeReader{objects: map[string]importer.Found{"vault.Secret app": obj("app"), "vault.Secret web": obj("web")}}
	got, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{{Type: typ, IDs: []string{"app", "web"}}},
		Reader:     r, File: "imported.tf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Failed() || len(got.Objects) != 2 || len(got.Params) != 4 {
		t.Fatalf("objects %+v, params %+v, problems %+v; want two objects with a param for each container", got.Objects, got.Params, got.Problems)
	}
	shown := fmt.Sprintf("%s\n%+v\n%+v\n%+v", got.Code, got.Params, got.Problems, got.Objects)
	for _, s := range []string{key, elem, item} {
		if strings.Contains(shown, s) {
			t.Errorf("%s is in the code, a param, a problem, or an object:\n%s", s, shown)
		}
	}
	types := map[string]string{"data": "map(string)", "tokens": "list(string)"}
	for _, p := range got.Params {
		if p.Type != types[p.Attr] {
			t.Errorf("param %+v, want the type %q", p, types[p.Attr])
		}
		v := got.ParamValues[p.Name]
		if want := map[string]string{"data": `{"` + key + `":"` + elem + `"}`, "tokens": `["` + item + `"]`}[p.Attr]; v != want {
			t.Errorf("param %s has the value %q, want the container as JSON %q", p.Name, v, want)
		}
	}

	// The key is a secret the run knows: an object whose import id it is
	// is refused, and a provider's refusal that repeats it is redacted.
	r.objects["vault.Secret "+key] = obj(key)
	got, err = importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{{Type: typ, IDs: []string{"app", key, "absent " + key}}},
		Reader:     echoReader{r}, File: "imported.tf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got.Problems, func(p importer.Problem) bool {
		return p.Kind == importer.NotImported && p.From == "objects[0].ids[1]"
	}) {
		t.Errorf("problems %+v, want objects[0].ids[1] refused for an import id that is a secret map key", got.Problems)
	}
	if shown := fmt.Sprintf("%+v", got.Problems); strings.Contains(shown, key) || !strings.Contains(shown, "(secret)") {
		t.Errorf("problems %s, want the key redacted", shown)
	}
}

// echoReader repeats the id in a refusal, as a provider may.
type echoReader struct{ *fakeReader }

func (e echoReader) Read(ctx context.Context, typ types.Name, cfg addr.Addr, id string) (importer.Found, diag.List, error) {
	f, diags, err := e.fakeReader.Read(ctx, typ, cfg, id)
	if diags.HasErrors() {
		diags = nil
		diags.Errorf(diag.Provider, diag.Span{}, "no object named %q", id)
	}
	return f, diags, err
}

type boundedReader struct{ fakeReader }

func (r *boundedReader) Validator(types.Name, addr.Addr) importer.Validator {
	return func(_ context.Context, _ string, c value.Value) ([]provider.Diag, error) {
		for _, k := range c.Keys() {
			if strings.HasPrefix(k, "optional_") && c.Get(k).Kind() != value.Null {
				return []provider.Diag{{Error: true, Path: value.Path{}.Attr(k), Summary: "invalid zero"}}, nil
			}
		}
		return nil, nil
	}
}
func TestBulkContinuesAfterPruningBound(t *testing.T) {
	src := schema.Source{Namespace: "test", Type: "test"}
	r := &schema.Resource{Name: types.Name{Provider: "test", Type: "Object"}, Wire: "test_object"}
	fields := map[string]value.Value{}
	for i := range 130 {
		k := fmt.Sprintf("optional_%03d", i)
		r.Block.Attributes = append(r.Block.Attributes, schema.Attribute{Name: k, Type: types.TInt, Optional: true})
		fields[k] = value.IntVal(0)
	}
	reader := &boundedReader{fakeReader{objects: map[string]importer.Found{
		"test.Object large": {Source: src, Schema: r, State: value.ObjectVal(fields)},
		"test.Object next":  {Source: src, Schema: r, State: value.ObjectVal(nil)},
	}}}
	res, err := importer.Bulk(context.Background(), importer.BulkOptions{File: "imported.tf", Reader: reader, Selections: []importer.Selection{{Type: r.Name, IDs: []string{"large", "next"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 2 {
		t.Fatalf("resources=%v", res.Objects)
	}
	found := false
	for _, p := range res.Problems {
		if strings.Contains(p.Reason, "128 provider calls") {
			if !strings.Contains(p.Reason, p.From) || p.From == "" {
				t.Fatalf("unnamed note: %+v", p)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no bound note: %v", res.Problems)
	}
}
