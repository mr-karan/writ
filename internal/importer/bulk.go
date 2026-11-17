package importer

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/tfcode"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// Defaults for the limits of a bulk import.
const (
	DefaultMaxObjects = 500
	DefaultMaxBytes   = 1 << 20
)

// minPlainLink is the shortest value that becomes a reference in a plain
// String slot. Shorter values, such as "1" or "true", are coincidences.
const minPlainLink = 8

// Reader reads existing objects through their providers.
type Reader interface {
	// Read reads the object id names. Diagnostics are refusals of this one
	// object, and the error stops the whole import.
	Read(ctx context.Context, typ types.Name, config addr.Addr, id string) (Found, diag.List, error)
	// Validator asks the provider of typ whether it accepts a config.
	Validator(typ types.Name, config addr.Addr) Validator
	// Planner plans a config against the object as Read returned it.
	// Nil means the reader cannot plan, and then the code keeps every ""
	// the object holds.
	Planner(typ types.Name, config addr.Addr, prior Found) Planner
}

// Lister lists the import ids of the objects of a type that a provider can
// enumerate, with query as the provider's list config. It returns at most
// limit ids. Diagnostics are refusals of the whole entry, and the error
// stops the whole import.
type Lister interface {
	List(ctx context.Context, typ types.Name, config addr.Addr, query value.Value, limit int) ([]string, diag.List, error)
}

// Found is an object as its provider read it.
type Found struct {
	Schema *schema.Resource
	// Source is the provider's source, and Version the version that read it.
	Source schema.Source
	// State has sensitive leaves marked secret.
	State   value.Value
	Private []byte
	// Identity is the resource identity the provider returned, which
	// every plan of the object must keep.
	Identity provider.Identity
	ReadAt   time.Time
	// ProviderConfig is the provider configuration that read the object,
	// and null when the reader does not know it.
	ProviderConfig value.Value
}

// BulkOptions configures Bulk.
type BulkOptions struct {
	Selections []Selection
	Reader     Reader
	// Lister answers list selections.
	Lister Lister
	// Secrets are the secret scalars the stack's code holds, as
	// Known.Secrets.
	Secrets []value.Value
	// Taken are the top-level names the stack uses.
	Taken []string
	// MaxObjects bounds the objects read; MaxBytes bounds the code. Zero
	// means the default.
	MaxObjects int
	MaxBytes   int
	// Parallelism bounds the reads in flight. Zero means one at a time.
	Parallelism int
	// File names the generated file, for messages.
	File string
}

// BulkResult is what Bulk read and generated.
type BulkResult struct {
	// Code is the generated file, formatted as tofu fmt does.
	Code    []byte
	Objects []BulkObject
	// Imports are, in an OpenTofu stack, the objects as their import
	// blocks adopt them, in the order of Objects.
	Imports []Import
	Params  []Param
	// ParamValues holds each param's value, for checking and planning the
	// code. It must never be printed.
	ParamValues map[string]string
	Problems    []Problem
	// Linked counts literals written as references, and Literals the
	// literals left in typed-reference slots.
	Linked, Literals int
	// Groups counts the for expressions, and Singles the lets of one object.
	Groups, Singles int
}

// BulkObject is one object the generated code declares.
type BulkObject struct {
	Addr     addr.Addr
	Type     types.Name
	Provider addr.Addr
	// ID is the import id, "" when it is one of the object's own secrets.
	ID string
	// ImportID is the import id, a secret when ID is "". Never print it.
	ImportID string
	// Origin says which selection entry picked it, such as objects[1].ids[0].
	Origin string
	Found
}

// Failed reports whether an object could not be read.
func (r *BulkResult) Failed() bool {
	return slices.ContainsFunc(r.Problems, func(p Problem) bool { return p.Kind == NotImported })
}

// LimitError means the import passed a limit on real counts. Nothing is
// written.
type LimitError struct {
	What  string // "objects", "expansions", "bytes", or "secret bytes"
	Limit int
	Got   int // what was reached when the run stopped
}

func (e *LimitError) Error() string {
	switch e.What {
	case "objects":
		return fmt.Sprintf("the selection reads more than %d objects; raise --max-objects or narrow the selection", e.Limit)
	case "expansions":
		return fmt.Sprintf("the selection expands id templates more than %d times, %d for each object --max-objects allows; raise --max-objects or narrow the selection", e.Limit, ExpansionsPerObject)
	case "secret bytes":
		return fmt.Sprintf("the secrets that the import compares values against have %d bytes, more than the limit of %d; import fewer objects, or from a stack with fewer secrets in its code and state", e.Got, e.Limit)
	}
	return fmt.Sprintf("the generated code is %d bytes, more than the limit of %d; raise --max-bytes or import in parts", e.Got, e.Limit)
}

// DuplicateError means the selection names one object twice, by ids or
// values: the same type, provider configuration, and import id, or two
// ids that the provider read as one object. Nothing is written. It names
// the entries, not the id, which may be a secret.
type DuplicateError struct {
	First, Again string // origins, such as objects[0].ids[1]
	// Read is set when the import ids differ and the reads showed one
	// object.
	Read bool
}

func (e *DuplicateError) Error() string {
	why := "the type, the provider configuration, and the import id are the same"
	if e.Read {
		why = "the provider read one object for both ids"
	}
	return fmt.Sprintf("%s selects the same object as %s: %s; remove one", e.Again, e.First, why)
}

// bnode is one object in a bulk import.
type bnode struct {
	*node
	typ types.Name
	// via is the provider configuration the object was read through.
	via      addr.Addr
	id       string
	idSecret bool
	origin   string
	found    Found
	group    *bgroup
	key      string
	// params is set when a secret leaf of the config is not a reference,
	// so the object becomes a param and stays out of any group.
	params bool
	// writeOnly is set when the config lacks a write-only value, so the
	// provider is not asked to validate it.
	writeOnly bool
}

// bgroup is one let: a for over two or more objects, or one object.
type bgroup struct {
	let     string
	typ     types.Name
	members []*bnode
	// keyAttr is the attribute whose value is each member's key.
	keyAttr string
}

func (g *bgroup) many() bool { return len(g.members) > 1 }

// Bulk reads the objects the selections name and writes the code that
// declares them. It writes no file and no state.
func Bulk(ctx context.Context, opts BulkOptions) (*BulkResult, error) {
	if opts.MaxObjects <= 0 {
		opts.MaxObjects = DefaultMaxObjects
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	res := &BulkResult{ParamValues: map[string]string{}}
	rd, err := readAll(ctx, opts, res)
	if err != nil {
		return nil, err
	}
	nodes := rd.nodes
	peers := innerNodes(nodes)
	secrets, err := runSecrets(opts, rd)
	if err != nil {
		return nil, err
	}
	// A provider may echo an id or a value in a refusal, as free text.
	defer func() {
		red := secrets.redactor()
		for i := range res.Problems {
			res.Problems[i].Reason = red.Text(res.Problems[i].Reason)
		}
	}()
	for _, n := range nodes {
		// An id the object keeps as its own secret is never printed.
		if secrets.idLeaks(n.node, n.id) {
			res.Problems = append(res.Problems, Problem{Kind: NotImported, From: n.origin, Reason: "the import id equals, contains, or is part of a secret that another object of this run or the stack keeps, and writ never writes a secret where it is not secret; leave this object out, or import it in a run without the object that keeps the secret"})
			continue
		}
		n.idSecret = secrets.idSecret(n.node, n.id)
		if n.idSecret {
			res.Problems = append(res.Problems, Problem{Kind: NotImported, From: n.origin, Reason: secretIDReason})
			continue
		}
		// State keeps what the provider read, so a value of its own may
		// repeat in an attribute that is not sensitive, as local's id
		// repeats content_sha1, but another owner's may not.
		if secrets.inClear(n.schema.Block, n.state, n.node) {
			res.Problems = append(res.Problems, leakProblem(n))
			continue
		}
		n.plan = opts.Reader.Planner(n.typ, n.via, n.found)
		notes, writeOnly := configure(n.node, peers...)
		n.writeOnly = writeOnly
		res.Problems = append(res.Problems, notes...)
		// Code holds no literal that equals, contains, or is part of a secret,
		// the object's own included.
		if secrets.inClear(n.schema.Block, n.config, nil) {
			res.Problems = append(res.Problems, leakProblem(n))
		}
		if why := notNFC(n); why != "" {
			res.Problems = append(res.Problems, Problem{Kind: NotImported, From: n.origin, Reason: why})
		}
	}
	if res.Failed() || len(nodes) == 0 {
		sortProblems(res.Problems)
		return res, nil
	}
	ss := indexSources(innerNodes(nodes))
	for _, n := range nodes {
		if !n.writeOnly {
			more, err := omitDisabled(ctx, n.node)
			if err != nil {
				return nil, err
			}
			res.Problems = append(res.Problems, more...)
		}
	}

	for _, n := range nodes {
		ask := opts.Reader.Validator(n.typ, n.via)
		if !n.writeOnly {
			p, err := validate(ctx, ask, n.node, ss)
			if err != nil {
				return nil, err
			}
			res.Problems = append(res.Problems, p...)
		}
		p, err := prove(ctx, ask, n.node, ss, n.writeOnly)
		if err != nil {
			return nil, err
		}
		res.Problems = append(res.Problems, p...)
		if !n.writeOnly {
			more, err := planConfig(ctx, ask, n.node)
			if err != nil {
				return nil, err
			}
			res.Problems = append(res.Problems, more...)
		}
	}
	b := newBulkGen(opts, nodes)
	b.plan()
	code := b.renderHCL()
	if len(code) > opts.MaxBytes {
		return nil, &LimitError{What: "bytes", Limit: opts.MaxBytes, Got: len(code)}
	}
	res.Code = code
	res.Params, res.ParamValues = b.gen.params, b.gen.paramVals
	res.Linked, res.Literals = b.linked, b.literals
	for _, g := range b.groups {
		if g.many() {
			res.Groups++
		} else {
			res.Singles++
		}
	}
	for _, f := range b.gen.initials {
		res.Problems = append(res.Problems, Problem{Kind: Gap, From: f.n.tf, Reason: f.attr + " is in lifecycle.ignore_changes, because a runtime usually owns it; remove it there if the code should own it"})
	}
	for _, n := range b.sorted() {
		obj := BulkObject{Addr: n.addr, Type: n.typ, Provider: n.via, Origin: n.origin, ImportID: n.id, Found: n.found}
		if !n.idSecret {
			obj.ID = n.id
		}
		obj.Addr = hclAddr(n)
		imp := Import{Type: n.schema.Wire, Name: n.group.let, ID: n.id}
		if n.group.many() {
			key := n.key
			imp.Key = &key
		}
		res.Imports = append(res.Imports, imp)
		res.Objects = append(res.Objects, obj)
	}
	sortProblems(res.Problems)
	return res, nil
}

// leakProblem refuses an object that would write a known secret value
// where it is not secret: in code, or in state that is not sealed.
func leakProblem(n *bnode) Problem {
	return Problem{Kind: NotImported, From: n.origin, Reason: "an attribute that is not sensitive, or a map key, equals, contains, or is part of a secret that another object of this run or the stack keeps, and writ never writes a secret where it is not secret; leave this object out, or import it in a run without the object that keeps the secret"}
}

// notNFC is why OpenTofu code would not hold n exactly, "" when it would:
// its import id, or a string of its config, is not in Unicode normal form
// C. A for_each key is the import id, the name, or the Name tag, so a key
// is checked with them.
func notNFC(n *bnode) string {
	const why = " is not in Unicode normal form C; OpenTofu keeps every string of its code in NFC, so tofu would import or configure another string than the provider read and plan to replace the object; leave this object out"
	if !tfcode.InNFC(n.id) {
		return "the import id" + why
	}
	for _, k := range n.config.Keys() {
		if !stringsInNFC(n.config.Get(k)) {
			return k + " holds a string that" + why
		}
	}
	return ""
}

// stringsInNFC reports whether every string and object key of v is in
// Unicode normal form C.
func stringsInNFC(v value.Value) bool {
	switch v.Kind() {
	case value.String:
		return tfcode.InNFC(v.AsString())
	case value.List:
		for i := range v.Len() {
			if !stringsInNFC(v.Index(i)) {
				return false
			}
		}
	case value.Object:
		for _, k := range v.Keys() {
			if !tfcode.InNFC(k) || !stringsInNFC(v.Get(k)) {
				return false
			}
		}
	}
	return true
}

func innerNodes(ns []*bnode) []*node {
	out := make([]*node, len(ns))
	for i, n := range ns {
		out[i] = n.node
	}
	return out
}

// runSecrets collects what Known holds, and the secrets of every object the
// run read, each owned by the object of the run that keeps it.
func runSecrets(opts BulkOptions, rd *reader) (*secretSet, error) {
	s := Known{Secrets: opts.Secrets}.set()
	// A list query may hold a secret that a provider echoes in any
	// refusal of the run.
	for _, sel := range opts.Selections {
		s.addValue(nil, sel.List)
	}
	for _, n := range rd.nodes {
		s.addState(n.node, n.schema.Block, n.state)
	}
	for _, p := range rd.passed {
		var owner *node
		if p.owner != nil {
			owner = p.owner.node
		}
		s.addState(owner, p.found.Schema.Block, p.found.State)
	}
	return s, s.ready()
}

// bsource is one value a literal may refer to.
type bsource struct {
	// holder identifies the object: one per object, resource, or stack
	// lookup, so two attributes of one holder are one candidate.
	holder string
	attr   string
	typ    types.Type
	secret bool
	run    *bnode // an object of this run
	expr   string // otherwise, the expression that reads the value
	order  int    // schema order within the holder
}

// bulkGen decides and renders the code of a bulk import.
type bulkGen struct {
	opts    BulkOptions
	nodes   []*bnode
	sources map[string][]bsource
	groups  []*bgroup
	gen     *gen
	// decided holds the reference chosen for each leaf, by node and path;
	// a nil entry is a literal.
	decided map[*bnode]map[string]*bsource
	// edges are the references between lets, to refuse a cycle.
	edges    map[string]map[string]bool
	linked   int
	literals int
}

func newBulkGen(opts BulkOptions, nodes []*bnode) *bulkGen {
	b := &bulkGen{opts: opts, nodes: nodes, sources: map[string][]bsource{}}
	add := func(r *schema.Resource, state value.Value, holder string, run *bnode, expr func(attr string) string) {
		for i, a := range r.Block.Attributes {
			v := state.Get(a.Name)
			if v.Kind() != value.String || v.AsString() == "" {
				continue
			}
			self := a.Type.Kind() == types.Ref && names(a.Type, r.Name)
			if !self && a.Name != "id" && a.Name != "arn" {
				continue
			}
			s := bsource{holder: holder, attr: a.Name, typ: a.Type, secret: v.IsSecret() || a.Sensitive, run: run, order: i}
			if expr != nil {
				s.expr = expr(a.Name)
			}
			b.sources[v.AsString()] = append(b.sources[v.AsString()], s)
		}
	}
	for i, n := range nodes {
		add(n.schema, n.state, "run:"+strconv.Itoa(i), n, nil)
	}
	return b
}

// resolve returns the source a literal s in a slot of from may refer to,
// before cycles are considered.
func (b *bulkGen) resolve(from *bnode, slotName string, slot types.Type, slotSecret bool, s string) (*bsource, bool) {
	plain := slot.Kind() == types.String
	if slot.Kind() != types.Ref && !plain || plain && len(s) < minPlainLink {
		return nil, false
	}
	var cands []bsource
	for _, src := range b.sources[s] {
		if src.run == from || src.secret && !slotSecret {
			continue
		}
		if _, ok := types.Assign(slot, src.typ); ok {
			cands = append(cands, src)
		}
	}
	if len(cands) == 0 {
		return nil, false
	}
	for _, c := range cands[1:] {
		if c.holder != cands[0].holder {
			return nil, false
		}
	}
	best := slices.MinFunc(cands, func(x, y bsource) int {
		rank := func(c bsource) int {
			switch {
			case c.attr == slotName:
				return 0
			case c.attr == "id":
				return 1
			}
			return 2
		}
		return cmp.Or(cmp.Compare(rank(x), rank(y)), cmp.Compare(x.order, y.order))
	})
	return &best, true
}

// plan groups the objects, names them, and decides every reference. An
// object that keeps a secret literal leaves its group, and the decisions
// are made again, until no group holds one.
func (b *bulkGen) plan() {
	excluded := map[*bnode]bool{}
	for _, n := range b.nodes {
		if leafSecretWithoutSource(b, n) {
			excluded[n] = true
		}
	}
	for {
		b.group(excluded)
		b.decide()
		again := false
		for _, n := range b.nodes {
			if n.params && n.group.many() {
				excluded[n] = true
				again = true
			}
		}
		if !again {
			return
		}
	}
}

// leafSecretWithoutSource reports whether a secret leaf of n's config has
// no source at all, so it must become a param.
func leafSecretWithoutSource(b *bulkGen, n *bnode) bool {
	found := false
	walkConfig(n.schema.Block, n.config, nil, func(p value.Path, slotName string, slot types.Type, slotSecret bool, leaf value.Value) {
		if !leaf.IsSecret() {
			return
		}
		if leaf.Kind() != value.String {
			found = true
			return
		}
		if _, ok := b.resolve(n, slotName, slot, slotSecret, leaf.AsString()); !ok {
			found = true
		}
	})
	return found
}

// shape is what objects of one group share: their type, their provider
// configuration, and the attributes their code sets.
func shape(n *bnode) string {
	var b strings.Builder
	b.WriteString(n.typ.String() + "\x00" + n.via.String())
	for _, a := range n.schema.Block.Attributes {
		if n.config.Get(a.Name).Kind() != value.Null {
			b.WriteString("\x00" + a.Name)
		}
	}
	for _, nb := range n.schema.Block.Blocks {
		if v := n.config.Get(nb.Name); v.Kind() != value.Null && (v.Kind() != value.List || v.Len() > 0) {
			b.WriteString("\x00" + nb.Name)
		}
	}
	return b.String()
}

// group forms the groups and names them.
func (b *bulkGen) group(excluded map[*bnode]bool) {
	l := newLabeler(b.nodes)
	byShape := map[string][]*bnode{}
	var shapes []string
	var singles []*bnode
	for _, n := range b.nodes {
		if excluded[n] {
			singles = append(singles, n)
			continue
		}
		s := shape(n) + hclShape(n)
		if _, ok := byShape[s]; !ok {
			shapes = append(shapes, s)
		}
		byShape[s] = append(byShape[s], n)
	}
	var groups []*bgroup
	for _, s := range shapes {
		members := byShape[s]
		if len(members) < 2 {
			singles = append(singles, members...)
			continue
		}
		g := &bgroup{typ: members[0].typ, members: members}
		g.keyAttr = l.keyMembers(members)
		groups = append(groups, g)
	}
	slices.SortStableFunc(groups, func(x, y *bgroup) int {
		return cmp.Or(strings.Compare(x.typ.String(), y.typ.String()), strings.Compare(x.members[0].via.String(), y.members[0].via.String()), strings.Compare(x.members[0].key, y.members[0].key))
	})
	// Singles are keyed one type at a time, so an object whose id is a
	// secret is numbered among the objects of its type.
	byType := map[types.Name][]*bnode{}
	for _, n := range singles {
		byType[n.typ] = append(byType[n.typ], n)
	}
	var single []*bgroup
	for _, ns := range byType {
		l.keyMembers(ns)
		for _, n := range ns {
			single = append(single, &bgroup{typ: n.typ, members: []*bnode{n}})
		}
	}
	slices.SortFunc(single, func(x, y *bgroup) int {
		return cmp.Or(strings.Compare(x.typ.String(), y.typ.String()), strings.Compare(x.members[0].via.String(), y.members[0].via.String()), compareKeys(x.members[0].key, y.members[0].key))
	})
	taken := map[string]bool{}
	for _, name := range b.opts.Taken {
		taken[name] = true
	}
	name := func(base string) string {
		if !validLet(base) {
			base = "imported_" + base
		}
		out := base
		for i := 2; taken[out] || !validLet(out); i++ {
			out = base + "_" + strconv.Itoa(i)
		}
		taken[out] = true
		return out
	}
	b.groups = nil
	for _, g := range groups {
		g.let = name(letBase(g.members[0].schema.Wire, ""))
		b.groups = append(b.groups, g)
	}
	for _, g := range single {
		n := g.members[0]
		g.let = name(letBase(n.schema.Wire, cmp.Or(plain(nameOf(n)), l.derived(n), plain(tagOf(n)), n.key)))
		b.groups = append(b.groups, g)
	}
	for _, g := range b.groups {
		for _, n := range g.members {
			n.group = g
			if g.many() {
				n.addr = addr.Root(g.let).Key(n.key)
			} else {
				n.addr = addr.Root(g.let)
			}
		}
	}
}

// labeler names objects in code by what a reader knows them by. A name
// never holds an ARN or an account id, which an object's import id often
// does.
type labeler struct {
	// byID finds an object of the run by its provider configuration and
	// import id.
	byID map[string]*bnode
}

func newLabeler(nodes []*bnode) labeler {
	byID := map[string]*bnode{}
	for _, n := range nodes {
		byID[n.via.String()+" "+n.id] = n
	}
	return labeler{byID: byID}
}

func (l labeler) derived(n *bnode) string { return l.derive(n, 0) }

// derive builds the label that n's type's Label rule gives it, in lower
// case: the label of its parent when the run imports the parent, then its
// Name tag or its own Format. It is "" for a type with no rule. depth
// bounds a chain of parents that a provider could make circular.
func (l labeler) derive(n *bnode, depth int) string {
	rule := n.schema.Label
	if rule == nil || depth > 4 {
		return ""
	}
	own := plain(tagOf(n))
	if own == "" {
		t, err := ParseTemplate(rule.Format)
		if err != nil {
			panic(fmt.Sprintf("the Label format of %s: %v", n.schema.Wire, err))
		}
		// An attribute the format reads may be null or secret, and then
		// the object has no label of its own.
		if own, err = t.Expand(n.state); err != nil {
			return ""
		}
	}
	if id := n.state.Get(rule.Parent); rule.Parent != "" && id.Kind() == value.String && !id.IsSecret() {
		if p := l.byID[n.via.String()+" "+id.AsString()]; p != nil {
			if pl := cmp.Or(plain(nameOf(p)), l.derive(p, depth+1), plain(tagOf(p))); pl != "" {
				own = pl + "_" + own
			}
		}
	}
	return plain(strings.ToLower(own))
}

// plain is label, or "" when it holds an ARN or a run of exactly twelve
// digits, as an AWS account id is. An import id often holds both, and
// code that a user commits should not name the account. The last group of
// a UUID may be twelve digits too, and is no account id, but only in a
// whole UUID: a UUID-shaped part that a letter or digit touches may be an
// account id with hex around it.
func plain(label string) string {
	if strings.Contains(strings.ToLower(label), "arn:") || accountID.MatchString(withoutUUIDs(label)) {
		return ""
	}
	return label
}

var accountID = regexp.MustCompile(`(^|[^0-9])[0-9]{12}([^0-9]|$)`)

// uuidShape is the form of a UUID: x is a hex digit.
const uuidShape = "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"

// withoutUUIDs replaces each whole UUID in s with "-".
func withoutUUIDs(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if isUUIDAt(s, i) {
			b.WriteByte('-')
			i += len(uuidShape)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// isUUIDAt reports whether a whole UUID starts at s[i]: one with no
// letter or digit right before or after it. Any byte of a non-ASCII
// character counts as a letter.
func isUUIDAt(s string, i int) bool {
	end := i + len(uuidShape)
	if end > len(s) || i > 0 && isWordByte(s[i-1]) || end < len(s) && isWordByte(s[end]) {
		return false
	}
	for j := range len(uuidShape) {
		c := s[i+j]
		if uuidShape[j] == '-' {
			if c != '-' {
				return false
			}
		} else if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

func isWordByte(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c >= 0x80
}

// keyMembers gives each object a key and sorts them by it: their name,
// else the label their type derives, else their Name tag, when every
// member has a different one, else their import id, else their position.
// No key holds an ARN or an account id. It returns "name" when the keys
// are the name attribute, which the body then writes as the key variable.
func (l labeler) keyMembers(ns []*bnode) string {
	keyAttr := ""
	ids := !slices.ContainsFunc(ns, func(n *bnode) bool { return n.idSecret || plain(n.id) == "" })
	name := func(n *bnode) string { return plain(nameOf(n)) }
	tag := func(n *bnode) string { return plain(tagOf(n)) }
	many := len(ns) > 1
	switch {
	case many && uniqueLabels(ns, name):
		keyAttr = "name"
		for _, n := range ns {
			n.key = nameOf(n)
		}
	case many && uniqueLabels(ns, l.derived):
		for _, n := range ns {
			n.key = l.derived(n)
		}
	case many && uniqueLabels(ns, tag):
		for _, n := range ns {
			n.key = tagOf(n)
		}
	case ids:
		for _, n := range ns {
			n.key = n.id
		}
	default:
		// The hash orders the objects without revealing their ids.
		slices.SortFunc(ns, func(x, y *bnode) int {
			hx, hy := sha256.Sum256([]byte(x.id)), sha256.Sum256([]byte(y.id))
			return strings.Compare(string(hx[:]), string(hy[:]))
		})
		for i, n := range ns {
			n.key = strconv.Itoa(i + 1)
		}
	}
	slices.SortFunc(ns, func(x, y *bnode) int { return compareKeys(x.key, y.key) })
	return keyAttr
}

// nameOf is the object's name attribute, and tagOf its Name tag, or "" when
// it has none that is a plain string.
func nameOf(n *bnode) string { return label(n.config.Get("name")) }

func tagOf(n *bnode) string {
	tags := n.config.Get("tags")
	if tags.Kind() != value.Object || tags.IsSecret() {
		return ""
	}
	return label(tags.Get("Name"))
}

func label(v value.Value) string {
	if v.Kind() != value.String || v.IsSecret() {
		return ""
	}
	return v.AsString()
}

// uniqueLabels reports whether every object has a label and no two share
// one.
func uniqueLabels(ns []*bnode, of func(*bnode) string) bool {
	seen := map[string]bool{}
	for _, n := range ns {
		l := of(n)
		if l == "" || seen[l] {
			return false
		}
		seen[l] = true
	}
	return true
}

// compareKeys is a total order on keys, so the code never depends on the
// order the objects were read: numbers before other keys, numbers by value
// so "10" follows "9" and then by text so "01" precedes "1", other keys by
// text.
func compareKeys(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return cmp.Or(cmp.Compare(ai, bi), strings.Compare(a, b))
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// letBase is the name of a let: the wire type, and the label made into an
// identifier, cut at 64 bytes.
func letBase(wire, label string) string {
	base := wire
	if label != "" {
		base += "_" + strings.ToLower(label)
	}
	var b strings.Builder
	under := false
	for i := 0; i < len(base); i++ {
		c := base[i]
		ok := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
		switch {
		case ok:
			b.WriteByte(c)
			under = false
		case !under:
			b.WriteByte('_')
			under = true
		}
	}
	out := strings.TrimRight(b.String(), "_")
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "_")
		// Cutting a long label can leave a run of exactly twelve digits
		// that the whole label did not have, so the cut name is checked
		// again. Its UUIDs are spelled with "_" by now, so a cut name
		// with twelve digits anywhere falls back to the type's name.
		if label != "" && accountID.MatchString(out) {
			return letBase(wire, "")
		}
	}
	return ident(out)
}

// decide chooses the reference, or the literal, for every leaf of every
// config, in the order the code is written, and refuses a reference that
// would close a cycle between lets.
func (b *bulkGen) decide() {
	b.decided = map[*bnode]map[string]*bsource{}
	b.edges = map[string]map[string]bool{}
	b.linked, b.literals = 0, 0
	for _, g := range b.groups {
		for _, n := range g.members {
			n.params = false
			b.decided[n] = map[string]*bsource{}
			walkConfig(n.schema.Block, n.config, nil, func(p value.Path, slotName string, slot types.Type, slotSecret bool, leaf value.Value) {
				if leaf.Kind() != value.String {
					n.params = n.params || leaf.IsSecret()
					return
				}
				src, ok := b.resolve(n, slotName, slot, slotSecret, leaf.AsString())
				if ok && src.run != nil {
					to := src.run.group.let
					if to == g.let || b.reaches(to, g.let) {
						ok = false
					} else {
						if b.edges[g.let] == nil {
							b.edges[g.let] = map[string]bool{}
						}
						b.edges[g.let][to] = true
					}
				}
				switch {
				case ok:
					b.decided[n][p.String()] = src
					b.linked++
				default:
					if slot.Kind() == types.Ref {
						b.literals++
					}
					n.params = n.params || leaf.IsSecret()
				}
			})
		}
	}
}

func (b *bulkGen) reaches(from, to string) bool {
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(l string) bool {
		if l == to {
			return true
		}
		if seen[l] {
			return false
		}
		seen[l] = true
		for next := range b.edges[l] {
			if visit(next) {
				return true
			}
		}
		return false
	}
	return visit(from)
}

// walkConfig calls fn for every scalar leaf of a block value, with the
// slot's name and type, in the order the code writes them. A list or map
// that is secret as a whole is one leaf, since it becomes one param.
func walkConfig(blk schema.Block, v value.Value, p value.Path, fn func(p value.Path, slotName string, slot types.Type, slotSecret bool, leaf value.Value)) {
	var walkExpr func(slotName string, v value.Value, t types.Type, secret bool, p value.Path)
	walkExpr = func(slotName string, v value.Value, t types.Type, secret bool, p value.Path) {
		secret = secret || t.IsSecret()
		if secretContainer(v) {
			fn(p, slotName, t, secret, v)
			return
		}
		switch v.Kind() {
		case value.Null:
		case value.List:
			for i := range v.Len() {
				walkExpr(slotName, v.Index(i), t.Elem(), secret, p.Index(int64(i)))
			}
		case value.Object:
			for _, k := range v.Keys() {
				ft := t.Elem()
				if t.Kind() == types.Record {
					f, _ := t.Field(k)
					ft = f.Type
				}
				walkExpr(k, v.Get(k), ft, secret, p.Key(k))
			}
		default:
			fn(p, slotName, t, secret, v)
		}
	}
	for _, a := range blk.Attributes {
		walkExpr(a.Name, v.Get(a.Name), a.Type, a.Sensitive, p.Attr(a.Name))
	}
	for _, nb := range blk.Blocks {
		bv := v.Get(nb.Name)
		bp := p.Attr(nb.Name)
		switch {
		case bv.Kind() == value.Null:
		case nb.IsRecord():
			walkConfig(nb.Block, bv, bp, fn)
		case nb.Nesting == schema.NestMap:
			for _, k := range bv.Keys() {
				walkConfig(nb.Block, bv.Get(k), bp.Key(k), fn)
			}
		default:
			for i := range bv.Len() {
				walkConfig(nb.Block, bv.Index(i), bp.Index(int64(i)), fn)
			}
		}
	}
}

// sorted returns every object in the order the code declares them.
func (b *bulkGen) sorted() []*bnode {
	var out []*bnode
	for _, g := range b.order() {
		out = append(out, g.members...)
	}
	return out
}

// order returns the lets so that each comes after the lets it reads, ties
// broken by the order groups were formed.
func (b *bulkGen) order() []*bgroup {
	pos := map[string]int{}
	for i, g := range b.groups {
		pos[g.let] = i
	}
	done := map[string]bool{}
	var out []*bgroup
	var visit func(g *bgroup)
	visit = func(g *bgroup) {
		if done[g.let] {
			return
		}
		done[g.let] = true
		deps := make([]string, 0, len(b.edges[g.let]))
		for d := range b.edges[g.let] {
			deps = append(deps, d)
		}
		slices.SortFunc(deps, func(x, y string) int { return cmp.Compare(pos[x], pos[y]) })
		for _, d := range deps {
			visit(b.groups[pos[d]])
		}
		out = append(out, g)
	}
	for _, g := range b.groups {
		visit(g)
	}
	return out
}

// field is one top-level attribute or block of a resource body.
type bfield struct {
	name    string
	initial bool
}

func (b *bulkGen) fields(n *bnode) []bfield {
	var out []bfield
	for _, a := range n.schema.Block.Attributes {
		if n.config.Get(a.Name).Kind() == value.Null {
			continue
		}
		out = append(out, bfield{name: a.Name, initial: slices.Contains(n.schema.Scaled, a.Name)})
	}
	for _, nb := range n.schema.Block.Blocks {
		if v := n.config.Get(nb.Name); v.Kind() != value.Null && (v.Kind() != value.List || v.Len() > 0) {
			out = append(out, bfield{name: nb.Name})
		}
	}
	return out
}
