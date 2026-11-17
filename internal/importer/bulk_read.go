package importer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// want is one object a selection names, before it is read.
type want struct {
	sel    *Selection
	id     string
	origin string
	// typed is set when the user wrote the id, in ids or values, so a
	// second one is a mistake to fix, not a skip.
	typed bool
}

// identity is what names one object in a run: its type, the provider
// configuration it is read through, and its import id. The state's id
// attribute is not one: every random_password has the id "none".
func identity(s *Selection, id string) string {
	return s.Type.String() + "\x00" + s.Provider.String() + "\x00" + id
}

// passedState is an object read and not imported. owner is the object of
// the run it repeats, or nil, as for one the stack already manages.
type passedState struct {
	owner *bnode
	found Found
}

// reader reads the objects of a run and enforces the object limit on what
// it actually reads.
type reader struct {
	opts  BulkOptions
	res   *BulkResult
	nodes []*bnode
	// seen maps the identity of each object selected to its origin.
	seen map[string]string
	// passed holds the state of every object read and not imported, so
	// its secrets are known.
	passed []passedState
	// reads counts the objects asked for, read or refused, and expansions
	// the id templates expanded.
	reads, expansions int
}

func readAll(ctx context.Context, opts BulkOptions, res *BulkResult) (*reader, error) {
	r := &reader{opts: opts, res: res, seen: map[string]string{}}
	for i := range opts.Selections {
		s := &opts.Selections[i]
		at := fmt.Sprintf("objects[%d]", i)
		var wants []want
		var err error
		switch {
		case s.IDs != nil:
			for j, id := range s.IDs {
				wants = append(wants, want{sel: s, id: id, origin: fmt.Sprintf("%s.ids[%d]", at, j), typed: true})
			}
		case s.Values != nil:
			for j, rec := range s.Values {
				if wants, err = r.derive(wants, s, value.ObjectVal(rec), fmt.Sprintf("%s.values[%d]", at, j)); err != nil {
					return nil, err
				}
			}
		case s.From != (types.Name{}):
			parents := slices.DeleteFunc(slices.Clone(r.nodes), func(n *bnode) bool { return n.typ != s.From })
			if len(parents) == 0 {
				r.res.Problems = append(r.res.Problems, Problem{Kind: Skipped, From: at, Reason: fmt.Sprintf("this run read no %s object to derive ids from", s.From)})
			}
			for j, p := range parents {
				origin := fmt.Sprintf("%s.from[%d]", at, j)
				if len(s.Over) == 0 {
					if wants, err = r.derive(wants, s, value.ObjectVal(map[string]value.Value{"parent": p.state}), origin); err != nil {
						return nil, err
					}
					continue
				}
				items := p.state.At(s.Over)
				for k, item := range elements(items) {
					if wants, err = r.derive(wants, s, value.ObjectVal(map[string]value.Value{"parent": p.state, "item": item}), fmt.Sprintf("%s.%s[%d]", origin, s.Over, k)); err != nil {
						return nil, err
					}
				}
			}
		default:
			if err := r.list(ctx, s, at); err != nil {
				return nil, err
			}
			continue
		}
		if err := r.readEntry(ctx, wants); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// elements returns the elements of a list, or the values of a map in key
// order. Anything else has none.
func elements(v value.Value) []value.Value {
	switch v.Kind() {
	case value.List:
		out := make([]value.Value, v.Len())
		for i := range out {
			out[i] = v.Index(i)
		}
		return out
	case value.Object:
		var out []value.Value
		for _, k := range v.Keys() {
			out = append(out, v.Get(k))
		}
		return out
	}
	return nil
}

// derive expands the id template of s once. It fails when the run passes
// its bound on expansions, so a long list in an object read cannot make
// the run do unbounded work.
func (r *reader) derive(wants []want, s *Selection, vars value.Value, origin string) ([]want, error) {
	limit := r.opts.MaxObjects * ExpansionsPerObject
	if r.expansions++; r.expansions > limit {
		return nil, &LimitError{What: "expansions", Limit: limit, Got: r.expansions}
	}
	id, err := s.ID.Expand(vars)
	if skip, ok := errors.AsType[*errSkip](err); ok {
		r.res.Problems = append(r.res.Problems, Problem{Kind: Skipped, From: origin, Reason: skip.Error()})
		return wants, nil
	}
	if err == nil {
		err = CheckID(id)
	}
	if err != nil {
		r.res.Problems = append(r.res.Problems, Problem{Kind: NotImported, From: origin, Reason: err.Error()})
		return wants, nil
	}
	return append(wants, want{sel: s, id: id, origin: origin, typed: s.Values != nil}), nil
}

// count refuses a read past the object limit, and counts the read.
func (r *reader) count(more int) error {
	if r.reads+more > r.opts.MaxObjects {
		return &LimitError{What: "objects", Limit: r.opts.MaxObjects, Got: r.reads + more}
	}
	r.reads += more
	return nil
}

// readEntry reads the objects of one entry, at most Parallelism at a time.
// It counts every new id against the limit before the first read, so an
// entry that would pass the limit reads nothing. Results are kept in the
// order of wants, so the run does not depend on which read ends first.
func (r *reader) readEntry(ctx context.Context, wants []want) error {
	var fresh []want
	for _, w := range wants {
		again, err := r.again(w.sel, w.id, w.origin, w.typed)
		if err != nil {
			return err
		}
		if !again {
			fresh = append(fresh, w)
		}
	}
	if err := r.count(len(fresh)); err != nil {
		return err
	}
	type result struct {
		found Found
		diags diag.List
	}
	results := make([]result, len(fresh))
	// The first error stops the other reads, which then fail with the
	// cancellation, so only the first is reported.
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	slots := make(chan struct{}, max(r.opts.Parallelism, 1))
	var wg sync.WaitGroup
	for i, w := range fresh {
		select {
		case slots <- struct{}{}:
		case <-rctx.Done():
		}
		if rctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			f, diags, err := r.opts.Reader.Read(rctx, w.sel.Type, w.sel.Provider, w.id)
			if err != nil {
				cancel(err)
				return
			}
			results[i] = result{found: f, diags: diags}
		})
	}
	wg.Wait()
	if rctx.Err() != nil {
		return context.Cause(rctx)
	}
	for i, w := range fresh {
		res := results[i]
		if res.diags.HasErrors() {
			r.res.Problems = append(r.res.Problems, Problem{Kind: NotImported, From: w.origin, Reason: errorMessages(res.diags)})
			continue
		}
		if err := r.add(w.sel, w.id, w.origin, w.typed, res.found); err != nil {
			return err
		}
	}
	return nil
}

// again reports whether an earlier entry selected the same object. A
// second id the user wrote is an error; a second id that a from or a list
// produced is skipped, since the object is imported once already.
func (r *reader) again(s *Selection, id, origin string, typed bool) (bool, error) {
	k := identity(s, id)
	first, ok := r.seen[k]
	switch {
	case !ok:
		r.seen[k] = origin
		return false, nil
	case typed:
		return true, &DuplicateError{First: first, Again: origin}
	}
	r.res.Problems = append(r.res.Problems, Problem{Kind: Skipped, From: origin, Reason: "names the same object as " + first + ", which this run imports"})
	return true, nil
}

func errorMessages(diags diag.List) string {
	var msgs []string
	for _, d := range diags {
		if d.Severity == diag.Error {
			msgs = append(msgs, d.Message)
		}
	}
	return strings.Join(msgs, "; ")
}

func distinct(ids []string) map[string]bool {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	return seen
}

// listWithheld replaces the text of a listing refusal when the query
// holds a secret: a provider may quote or escape the secret it echoes, so
// no match on its bytes can be trusted.
const listWithheld = "the provider refused the listing, and its text is withheld because the list query holds a secret"

// list reads the objects the provider lists for s, as an entry of ids
// would. Only new ids count against the object limit. It asks for one more
// id than the room left, plus one for each id the run already selected of
// this type and provider configuration, which the listing may repeat. A
// full answer then holds more new ids than the room, so a listing that
// would pass the limit stops the run before any read.
func (r *reader) list(ctx context.Context, s *Selection, at string) error {
	room := r.opts.MaxObjects - r.reads
	prefix := identity(s, "")
	known := 0
	for k := range r.seen {
		if strings.HasPrefix(k, prefix) {
			known++
		}
	}
	limit := room + 1 + known
	ids, diags, err := r.opts.Lister.List(ctx, s.Type, s.Provider, s.List, limit)
	if err != nil {
		return err
	}
	if diags.HasErrors() {
		reason := errorMessages(diags)
		if s.List.HasSecret() {
			reason = listWithheld
		}
		r.res.Problems = append(r.res.Problems, Problem{Kind: NotImported, From: at, Reason: reason})
		return nil
	}
	// A full answer that repeats an id holds fewer distinct ids than it
	// asked for, so it proves neither that the list ends there nor that
	// it passes the limit.
	if len(ids) >= limit && len(distinct(ids)) < len(ids) {
		r.res.Problems = append(r.res.Problems, Problem{Kind: NotImported, From: at, Reason: fmt.Sprintf("the provider repeated an object in a listing that reached the request limit of %d, so writ cannot tell whether it lists every object; narrow the query or raise --max-objects", limit)})
		return nil
	}
	var wants []want
	for j, id := range ids {
		origin := fmt.Sprintf("%s.list[%d]", at, j)
		if err := CheckID(id); err != nil {
			r.res.Problems = append(r.res.Problems, Problem{Kind: NotImported, From: origin, Reason: err.Error()})
			continue
		}
		wants = append(wants, want{sel: s, id: id, origin: origin})
	}
	return r.readEntry(ctx, wants)
}

// add keeps an object the provider read, unless the stack or an earlier
// read of the run holds it. A second id the user wrote for one object is
// an error, as in again.
func (r *reader) add(s *Selection, id, origin string, typed bool, f Found) error {
	if f.Schema == nil || f.State.Kind() != value.Object {
		r.res.Problems = append(r.res.Problems, Problem{Kind: NotImported, From: origin, Reason: "the provider returned no object"})
		return nil
	}
	// again already matched the import id, so another object of the run
	// is this one only when the provider read exactly its state.
	for _, n := range r.nodes {
		if n.schema.Wire != f.Schema.Wire || n.found.Source != f.Source || n.via != s.Provider || !value.Equal(n.state, f.State) {
			continue
		}
		if typed {
			return &DuplicateError{First: n.origin, Again: origin, Read: true}
		}
		r.passed = append(r.passed, passedState{owner: n, found: f})
		r.res.Problems = append(r.res.Problems, Problem{Kind: Skipped, From: origin, Reason: "names the same object as " + n.origin + ", which this run imports"})
		return nil
	}
	n := &bnode{
		node: &node{source: f.Source, tf: origin, schema: f.Schema, state: f.State, defaults: f.ProviderConfig, provider: configText(s.Provider)},
		typ:  s.Type,
		via:  s.Provider,
		id:   id, origin: origin, found: f,
	}
	r.nodes = append(r.nodes, n)
	return nil
}
