package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// The generated code sets a resource's config, not its state. Terraform
// state holds values no configuration wrote: computed outputs, values the
// provider filled in for attributes the configuration left unset, and
// placeholder blocks. Code that sets them fails the provider's validation,
// so import leaves out every value it can show was not configured, and only
// where leaving it out keeps the plan the same.

// configure sets n.config: the state without computed-only attributes,
// deprecated attributes the provider computes, nested block elements that
// leave a required attribute null, and values the provider would derive the
// same way from null. It returns a problem for each
// deprecated attribute it had to keep and each write-only attribute the
// configuration set, and whether n.config lacks a write-only value.
//
// A write-only attribute is null in every state. The configuration set one
// when the provider requires it, or when its version attribute, which
// providers pair with a write-only attribute to say when its value changed,
// is set.
func configure(n *node, peers ...*node) (notes []Problem, writeOnly bool) {
	note := func(format string, args ...any) {
		notes = append(notes, Problem{Kind: Gap, From: n.tf, Reason: fmt.Sprintf(format, args...)})
	}
	for _, nb := range n.schema.Block.Blocks {
		owner, inline := nb.ImportOwner, n.state.Get(nb.Name)
		if owner == nil || isNull(inline) || inline.Len() == 0 {
			continue
		}
		items := []value.Value{inline}
		if !nb.IsRecord() {
			items = nil
			for i := range inline.Len() {
				items = append(items, inline.Index(i))
			}
		}
		missing := slices.ContainsFunc(items, func(item value.Value) bool {
			return !slices.ContainsFunc(peers, func(peer *node) bool {
				if peer.schema.Wire != owner.Wire || peer.source != n.source || peer.provider != n.provider {
					return false
				}
				parent := n.state.Get(owner.ParentField)
				if isNull(parent) || !value.Equal(parent, peer.state.Get(owner.OwnerField)) {
					return false
				}
				return owner.InlineKey == "" || !isNull(item.Get(owner.InlineKey)) && value.Equal(item.Get(owner.InlineKey), peer.state.Get(owner.OwnerKey))
			})
		})
		if missing {
			note("%s is deprecated and left out of the code; the import run lacks a matching %s object for at least one setting, so import it separately to manage that setting", nb.Name, owner.Wire)
		}
	}
	n.config = project(n.schema.Block, n.state, nil, func(v value.Value, a schema.Attribute, p value.Path) bool {
		fv := v.Get(a.Name)
		switch {
		case a.WriteOnly && (a.Required || !isNull(v.Get(a.Name+"_version"))):
			writeOnly = true
			note("%s is write-only, so its value is not in the state; set it in the code before you plan", n.pathText(p))
		case !a.Required && !a.Optional:
			return false
		case n.plan != nil && n.schema.LegacySDK && a.Optional && !a.Computed && fv.Kind() == value.String && fv.AsString() == "":
			// The legacy SDK reads "" and unset alike for an attribute it
			// neither computes nor defaults. The schema does not say which
			// attributes have a default, so prove checks with a plan. A
			// computed one keeps its "", because a null config would hand
			// the value to the provider.
			n.emptied = append(n.emptied, emptied{path: p, secret: fv.IsSecret()})
			return false
		case len(p) == 1 && a.Computed && a.Name == "tags_all" && sameTags(fv, v.Get("tags")):
			// The provider computes tags_all from tags and its default
			// tags, and writing it would pin the merged tags.
			return false
		case len(p) == 1 && a.Computed && fv.Kind() == value.String && n.defaults.Kind() == value.Object && value.Equal(fv, n.defaults.Get(a.Name)):
			// A computed attribute that repeats the provider
			// configuration's attribute of the same name, as AWS's region
			// does, takes its value from it.
			return false
		case a.Deprecated && isNull(fv):
		case a.Deprecated && a.Computed && !a.Required:
			// Null config keeps the prior value of a computed attribute.
			return false
		case a.Deprecated:
			note("%s is deprecated, and the provider warns at every plan; it stays in the code because leaving it out would change the plan", n.pathText(p))
		}
		return true
	})
	if n.plan != nil && n.schema.LegacySDK {
		// The rule for "" above, inside the records of a top-level
		// attribute, such as a route of an aws_route_table, whose fields
		// are optional only.
		for _, set := range nestedSets(n.schema.Block) {
			for _, f := range zeroFields(set, n.config.Get(set.name)) {
				if v := fieldOf(n.config, f); v.Kind() == value.String && !set.computed(f.name) {
					n.config = nullField(n.config, f)
					n.emptied = append(n.emptied, emptied{path: f.path, secret: v.IsSecret()})
				}
			}
		}
	}
	return notes, writeOnly
}

// emptied is an optional "" that configure left out of the code, which
// prove keeps only when a plan shows that leaving it out changes nothing.
type emptied struct {
	path   value.Path
	secret bool
}

// Planner returns what the provider plans for config against the object
// as it was read, with no refresh, as PlanResourceChange does.
type Planner func(ctx context.Context, config value.Value) (Plan, error)

// Plan is a provider's plan of one object: the planned value, the paths
// whose change replaces the object, and the private data the provider
// keeps with it. It holds no resource identity: a Planner sends the
// object's identity as the prior one, and the provider client fails a
// plan that changes it.
type Plan struct {
	Value           value.Value
	RequiresReplace []value.Path
	Private         []byte
}

// prove decides whether the "" values in n.emptied, which configure left
// out of n.config, stay out. A field with a default that the provider
// fills in only when the config lacks it, such as SDKv2's Default, plans
// "" to that default, and one such change can suppress or cause another.
// So prove plans the object twice, with no refresh: once as n.config, and
// once with every "" kept, which is the state's own values. Leaving them
// out changes nothing only when both planned objects are wholly known, so
// no value waits for the apply, and equal, every attribute, computed ones,
// list order, and set elements included, and the two plans replace the
// object on the same paths and keep the same private data. Then they stay
// out with no note. Otherwise the code keeps every "", with one note that
// says why, and is validated again. A plan that fails keeps every "" too. It costs two provider calls,
// made at once, for each object that left out a "".
func prove(ctx context.Context, ask Validator, n *node, ss sources, writeOnly bool) ([]Problem, error) {
	if n.pruningStopped {
		n.config = restore(n.config, n.emptied)
		n.emptied = nil
		return nil, nil
	}
	if len(n.emptied) == 0 {
		return nil, nil
	}
	kept := restore(n.config, n.emptied)
	n.emptied = nil
	if writeOnly {
		// A config without its write-only value cannot be planned.
		n.config = kept
		return nil, nil
	}
	// The two plans are independent, so they run at once.
	var with Plan
	var errWith error
	done := make(chan struct{})
	go func() {
		defer close(done)
		with, errWith = n.plan(ctx, kept)
	}()
	without, errWithout := n.plan(ctx, n.config)
	<-done
	var why string
	switch {
	case errWithout != nil || errWith != nil:
	case !without.Value.IsWhollyKnown() || !with.Value.IsWhollyKnown():
		names := slices.Concat(unknownAttrs(without.Value), unknownAttrs(with.Value))
		slices.Sort(names)
		why = "a plan leaves " + strings.Join(slices.Compact(names), ", ") + " unknown until the apply, so it cannot prove that leaving them out changes nothing"
	case !value.Equal(without.Value, with.Value):
		why = "a plan without them differs from a plan with them at " + strings.Join(differences(without.Value, with.Value), ", ") + ", as a default the provider fills in for an unset field does"
	case !samePaths(without.RequiresReplace, with.RequiresReplace):
		var ps []string
		for _, p := range pathDiff(without.RequiresReplace, with.RequiresReplace) {
			ps = append(ps, n.pathText(p))
		}
		why = "a plan without them replaces the object on other paths than a plan with them: " + strings.Join(ps, ", ")
	case !bytes.Equal(without.Private, with.Private):
		why = "the provider keeps other private data with a plan without them than with a plan with them"
	default:
		return nil, nil
	}
	var notes []Problem
	if why != "" {
		notes = append(notes, Problem{Kind: Gap, From: n.tf, Reason: n.redactor().Text(`the code keeps every "" this object holds, because ` + why)})
	}
	n.config = kept
	more, err := validate(ctx, ask, n, ss)
	if err != nil {
		return nil, err
	}
	return append(notes, more...), nil
}

// unknownAttrs names the top-level attributes of a planned object that
// hold an unknown value.
func unknownAttrs(v value.Value) []string {
	if v.Kind() != value.Object {
		return []string{"the whole object"}
	}
	var out []string
	for _, k := range v.Keys() {
		if !v.Get(k).IsWhollyKnown() {
			out = append(out, k)
		}
	}
	return out
}

// samePaths reports whether a and b hold the same paths, in any order.
func samePaths(a, b []value.Path) bool { return len(pathDiff(a, b)) == 0 }

// pathDiff lists the paths that only one of a and b holds, in a stable
// order.
func pathDiff(a, b []value.Path) []value.Path {
	count := map[string]int{}
	byText := map[string]value.Path{}
	for _, p := range a {
		count[p.String()] |= 1
		byText[p.String()] = p
	}
	for _, p := range b {
		count[p.String()] |= 2
		byText[p.String()] = p
	}
	var keys []string
	for k, c := range count {
		if c != 3 {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := make([]value.Path, len(keys))
	for i, k := range keys {
		out[i] = byText[k]
	}
	return out
}

// differences names the top-level attributes where two planned objects
// differ.
func differences(a, b value.Value) []string {
	if a.Kind() != value.Object || b.Kind() != value.Object {
		return []string{"the whole object"}
	}
	keys := slices.Concat(a.Keys(), b.Keys())
	slices.Sort(keys)
	var out []string
	for _, k := range slices.Compact(keys) {
		if !value.Equal(a.Get(k), b.Get(k)) {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		// The objects differ only in a mark, such as a secret one.
		return []string{"the whole object"}
	}
	return out
}

// restore returns config with "" put back at each path in es.
func restore(config value.Value, es []emptied) value.Value {
	for _, e := range es {
		v := value.StringVal("")
		if e.secret {
			v = v.Secret()
		}
		config = setPath(config, e.path, v)
	}
	return config
}

// setPath returns v with the value at p replaced by nv. A step that v does
// not have leaves v as it is.
func setPath(v value.Value, p value.Path, nv value.Value) value.Value {
	if len(p) == 0 {
		return nv
	}
	if name, ok := p[0].Name(); ok {
		if v.Kind() != value.Object {
			return v
		}
		return withField(v, name, setPath(v.Get(name), p[1:], nv))
	}
	if v.Kind() != value.List {
		return v
	}
	elems := make([]value.Value, v.Len())
	for i := range v.Len() {
		elems[i] = v.Index(i)
		if p[0] == value.IndexStep(int64(i)) {
			elems[i] = setPath(elems[i], p[1:], nv)
		}
	}
	out := value.ListVal(elems)
	if v.IsSecret() {
		out = out.Secret()
	}
	return out
}

// keepFunc reports whether the attribute a, at path p in the block value v,
// keeps its value.
type keepFunc func(v value.Value, a schema.Attribute, p value.Path) bool

// project returns the block value v with the attributes keep rejects set to
// null and the nested block elements that leave a required attribute null
// removed. Paths follow the generated code: a record block adds no index.
func project(b schema.Block, v value.Value, p value.Path, keep keepFunc) value.Value {
	if v.Kind() != value.Object {
		return v
	}
	fields := make(map[string]value.Value, v.Len())
	for _, k := range v.Keys() {
		fields[k] = v.Get(k)
	}
	for _, a := range b.Attributes {
		if !keep(v, a, p.Attr(a.Name)) {
			fields[a.Name] = value.NullVal()
		}
	}
	for _, nb := range b.Blocks {
		bv, bp := v.Get(nb.Name), p.Attr(nb.Name)
		element := func(ev value.Value, ep value.Path) (value.Value, bool) {
			if nb.ImportOwner != nil || placeholder(nb.Block, ev) {
				return value.Value{}, false
			}
			return project(nb.Block, ev, ep, keep), true
		}
		switch {
		case bv.Kind() == value.Null:
		case nb.IsRecord():
			ev, ok := element(bv, bp)
			if !ok {
				ev = value.NullVal()
			}
			fields[nb.Name] = ev
		case nb.Nesting == schema.NestMap:
			out := map[string]value.Value{}
			for _, k := range bv.Keys() {
				if ev, ok := element(bv.Get(k), bp.Key(k)); ok {
					out[k] = ev
				}
			}
			fields[nb.Name] = value.ObjectVal(out)
		default:
			var out []value.Value
			for i := range bv.Len() {
				if ev, ok := element(bv.Index(i), bp.Index(int64(len(out)))); ok {
					out = append(out, ev)
				}
			}
			fields[nb.Name] = value.ListVal(out)
		}
	}
	out := value.ObjectVal(fields)
	if v.IsSecret() {
		out = out.Secret()
	}
	return out
}

// placeholder reports whether a block element leaves a required attribute
// null. No configuration can write one, because the provider requires the
// attribute, so the provider filled the element in itself.
func placeholder(b schema.Block, v value.Value) bool {
	return slices.ContainsFunc(b.Attributes, func(a schema.Attribute) bool {
		return a.Required && isNull(v.Get(a.Name))
	})
}

func disabledBlock(nb schema.NestedBlock, v value.Value) bool {
	if nb.ImportDisabled == nil || nb.MinItems > 0 {
		return false
	}
	fv := v.Get(nb.ImportDisabled.Field)
	switch fv.Kind() {
	case value.String:
		return fv.AsString() == nb.ImportDisabled.Value
	case value.Bool:
		return fmt.Sprint(fv.AsBool()) == nb.ImportDisabled.Value
	}
	return false
}

func isNull(v value.Value) bool { return v.Kind() == value.Null }

// sameTags reports whether tagsAll holds only the tags in tags.
func sameTags(tagsAll, tags value.Value) bool {
	empty := func(v value.Value) bool { return isNull(v) || v.Kind() == value.Object && v.Len() == 0 }
	return empty(tagsAll) && empty(tags) || value.Equal(tagsAll, tags)
}

// isZero reports whether v is the zero value of its type: the empty string,
// zero, false, or an empty collection. The legacy provider SDK cannot store
// null for those types, so it writes the zero value for an attribute the
// configuration left unset.
func isZero(v value.Value) bool {
	switch v.Kind() {
	case value.String:
		return v.AsString() == ""
	case value.Int:
		return v.AsInt() == 0
	case value.Float:
		return v.AsFloat() == 0
	case value.Bool:
		return !v.AsBool()
	case value.List, value.Object:
		return v.Len() == 0
	}
	return false
}

// Validator asks a provider whether it accepts config for the resource type
// wire, as ValidateResourceConfig does. provider.Client.Validate is one.
type Validator func(ctx context.Context, wire string, config value.Value) ([]provider.Diag, error)

// validate asks the provider whether it accepts n.config and, while it
// does not, leaves out values that its errors name and that no configuration
// could have held. Terraform validated the configuration that wrote the
// state with the same provider version, so a value the provider rejects did
// not come from that configuration: the configuration left the attribute
// null, and Terraform planned that null against the state with no changes.
//
// Each round tries, in order:
//
//  1. A rejected attribute that is optional and holds its type's zero value,
//     which is what the legacy SDK stores for an unset attribute, is left out.
//  2. A provider names the whole set when it rejects a value inside a set
//     element, because a set element has no index. Inside the elements of a
//     rejected nested block or attribute of records, import leaves out one
//     optional zero value at a time, "" before false and 0, and keeps each
//     one the provider does not stop rejecting. A false or 0 the provider
//     accepts stays, because the code enforces it.
//  3. One optional computed attribute is left out: a null config keeps the
//     prior value of a computed attribute, so the plan stays the same, and
//     leaving out one of two conflicting attributes keeps the other in the
//     code. Import keeps an attribute whose value names another imported
//     resource, because the configuration most likely referred to it, and
//     otherwise leaves out the first in schema order.
//
// Every value left out gets a note with its path, so the code never loses a
// value silently. Errors left over become a problem.
var errPruneLimit = errors.New("import validation exceeded 128 provider calls")

func validate(ctx context.Context, ask Validator, n *node, ss sources) (notes []Problem, resultErr error) {
	if n.pruningStopped {
		return nil, nil
	}
	defer func() {
		if errors.Is(resultErr, errPruneLimit) {
			n.pruningStopped = true
			n.config = restore(n.config, n.emptied)
			n.emptied = nil
			notes = append(notes, Problem{Kind: Gap, From: n.tf, Reason: fmt.Sprintf("%s (%s): stopped pruning after 128 provider calls; the code restores every unproved empty string and keeps the remaining fields", n.tf, n.schema.Wire)})
			resultErr = nil
		}
	}()
	// Providers echo inputs in diagnostics, and the state holds secrets.
	red := n.redactor()
	calls := 0
	errorsOf := func(config value.Value) ([]provider.Diag, error) {
		if calls >= 128 {
			return nil, errPruneLimit
		}
		calls++
		diags, err := ask(ctx, n.schema.Wire, config)
		if err != nil {
			return nil, n.subject().Error(err, red)
		}
		return slices.DeleteFunc(diags, func(d provider.Diag) bool { return !d.Error }), nil
	}
	// held holds each message that withholds a provider's text. It is
	// Writ's own text, so it skips the redactor, and a note that holds one
	// says where to read the provider's text.
	held := map[string]bool{}
	hint := func(msgs []string) string {
		if slices.ContainsFunc(msgs, func(m string) bool { return held[m] }) {
			return "; " + provider.WithheldHint
		}
		return ""
	}
	note := func(paths []value.Path, format string, why []string) {
		var ps []string
		for _, p := range paths {
			ps = append(ps, n.pathText(p))
		}
		notes = append(notes, Problem{Kind: Gap, From: n.tf, Reason: fmt.Sprintf(format, red.Text(strings.Join(ps, ", ")), strings.Join(why, "; ")) + hint(why)})
	}
	for {
		errs, err := errorsOf(n.config)
		if err != nil {
			return notes, err
		}
		if len(errs) == 0 {
			return notes, nil
		}
		var rejected []value.Path
		var msgs []string
		for _, d := range errs {
			rejected = append(rejected, d.Path)
			shown := n.subject().Show(d)
			if shown.Withheld {
				held[shown.Text] = true
			} else {
				shown.Text = red.Text(shown.Text)
			}
			msgs = append(msgs, shown.Text)
		}
		why := func(paths []value.Path) []string {
			var out []string
			for i, r := range rejected {
				if slices.ContainsFunc(paths, func(p value.Path) bool { return r.HasPrefix(p) || p.HasPrefix(r) }) && !slices.Contains(out, msgs[i]) {
					out = append(out, msgs[i])
				}
			}
			return out
		}
		linked := func(a schema.Attribute, v value.Value) bool {
			return v.Kind() == value.String && len(ss.candidates(n, a.Type, v.AsString())) > 0
		}
		zero, computed := rejectedAttrs(n.schema.Block, n.config, rejected, linked)
		if len(zero) > 0 {
			n.config = nullPaths(n.schema.Block, n.config, zero)
			note(zero, "the code leaves out %s, because the provider rejects the empty or zero value each one held, so no configuration set it (%s)", why(zero))
			continue
		}
		cleared, inner, err := clearNested(n.schema.Block, n.config, rejected, errorsOf)
		if err != nil {
			return notes, err
		}
		if len(inner) > 0 {
			n.config = cleared
			note(inner, "the code leaves out %s, because the provider rejects each of these empty or zero values and accepts null (%s)", why(inner))
			continue
		}
		if len(computed) > 0 {
			n.config = nullPaths(n.schema.Block, n.config, computed[:1])
			note(computed[:1], "%s is left out of the code, because the provider rejects its value (%s); the provider keeps the value the object has, so the plan stays the same; write it in the code if the code should own it", why(computed[:1]))
			continue
		}
		return append(notes, Problem{Kind: Gap, From: n.tf, Reason: "the provider rejects the generated code: " + strings.Join(msgs, "; ") + hint(msgs)}), nil
	}
}

// pathText prints p, a path into n's object, as an import note prints it:
// each step past a secret prints as [(secret)], so credentials["k"] prints
// as credentials[(secret)].
func (n *node) pathText(p value.Path) string { return n.subject().PathText(p) }

// subject is n's object as the rule for provider text sees it.
func (n *node) subject() provider.Subject {
	return provider.Subject{Block: &n.schema.Block, Values: []value.Value{n.config, n.state}}
}

// redactor removes from text every secret string of n's object, and every
// key of a secret map in it, because Writ's own text may name a path or a
// value. A short key may take ordinary words with it, which is the price of
// never printing one.
func (n *node) redactor() value.Redactor {
	return value.Redactor{}.With(n.state, value.ListVal(secretKeys(n.state)))
}

// secretKeys returns each key of a secret map in v, marked secret.
func secretKeys(v value.Value) []value.Value {
	var keys []value.Value
	var walk func(v value.Value, secret bool)
	walk = func(v value.Value, secret bool) {
		secret = secret || v.IsSecret()
		switch v.Kind() {
		case value.Object:
			for _, k := range v.Keys() {
				if secret && k != "" {
					keys = append(keys, value.StringVal(k).Secret())
				}
				walk(v.Get(k), secret)
			}
		case value.List:
			for i := range v.Len() {
				walk(v.Index(i), secret)
			}
		}
	}
	walk(v, false)
	return keys
}

// rejectedAttrs returns the rejected optional attributes that hold a zero
// value, and the rejected optional computed attributes that do not, those
// that name another imported resource last. linked reports whether an
// attribute's value names another imported resource.
func rejectedAttrs(b schema.Block, config value.Value, rejected []value.Path, linked func(schema.Attribute, value.Value) bool) (zero, computed []value.Path) {
	var plain, computedLinked []value.Path
	project(b, config, nil, func(v value.Value, a schema.Attribute, p value.Path) bool {
		fv := v.Get(a.Name)
		switch {
		case a.Required || isNull(fv) || !slices.ContainsFunc(rejected, func(r value.Path) bool { return slices.Equal(r, p) }):
		case isZero(fv):
			zero = append(zero, p)
		case a.Computed && linked(a, fv):
			computedLinked = append(computedLinked, p)
		case a.Computed:
			plain = append(plain, p)
		}
		return true
	})
	return zero, append(plain, computedLinked...)
}

// nullPaths returns the block value config with the attributes at paths set
// to null.
func nullPaths(b schema.Block, config value.Value, paths []value.Path) value.Value {
	return project(b, config, nil, func(_ value.Value, _ schema.Attribute, p value.Path) bool {
		return !slices.ContainsFunc(paths, func(d value.Path) bool { return slices.Equal(d, p) })
	})
}

// zeroField is one field of one element of a top-level nested block or
// attribute of records.
type zeroField struct {
	path  value.Path
	set   string // the block or attribute
	index int    // the list element, or -1
	key   string // the map element, when index is -1 and record is false
	// record is set when the set is one record, not a collection.
	record bool
	name   string
}

// clearNested leaves out optional zero values inside the elements of each
// top-level nested block or attribute of records that rejected names by
// its own path, one at a time: "" first, then false, 0, and empty
// collections. It keeps a value left out only when errorsOf then reports
// fewer errors under that path, and stops at none. It returns the new
// config and the paths it left out.
func clearNested(b schema.Block, config value.Value, rejected []value.Path, errorsOf func(value.Value) ([]provider.Diag, error)) (value.Value, []value.Path, error) {
	var cleared []value.Path
	for _, set := range nestedSets(b) {
		sp := value.Path{}.Attr(set.name)
		if !slices.ContainsFunc(rejected, func(r value.Path) bool { return slices.Equal(r, sp) }) {
			continue
		}
		under := func(c value.Value) (int, error) {
			errs, err := errorsOf(c)
			return len(slices.DeleteFunc(errs, func(d provider.Diag) bool { return !d.Path.HasPrefix(sp) })), err
		}
		left, err := under(config)
		if err != nil {
			return config, nil, err
		}
		fields := zeroFields(set, config.Get(set.name))
		for _, f := range fields {
			if left == 0 {
				break
			}
			trial := nullField(config, f)
			n, err := under(trial)
			if err != nil {
				return config, nil, err
			}
			if n < left {
				config, left = trial, n
				cleared = append(cleared, f.path)
			}
		}
	}
	return config, cleared, nil
}

// nestedSet is a top-level nested block or attribute of records, with
// which of its fields are optional and which computed.
type nestedSet struct {
	name     string
	shape    types.Kind // types.Record, types.List, or types.Map
	optional func(string) bool
	computed func(string) bool
}

func nestedSets(b schema.Block) []nestedSet {
	var out []nestedSet
	for _, a := range b.Attributes {
		record, shape := a.Type, a.Type.Kind()
		if shape == types.List || shape == types.Map {
			record = record.Elem()
		}
		if record.Kind() != types.Record {
			continue
		}
		out = append(out, nestedSet{name: a.Name, shape: shape, optional: func(f string) bool {
			rf, ok := record.Field(f)
			return ok && rf.Optional
		}, computed: func(f string) bool {
			rf, ok := record.Field(f)
			return ok && rf.Computed
		}})
	}
	for _, nb := range b.Blocks {
		shape := types.List
		switch {
		case nb.IsRecord():
			shape = types.Record
		case nb.Nesting == schema.NestMap:
			shape = types.Map
		}
		attr := func(f string) (schema.Attribute, bool) {
			i := slices.IndexFunc(nb.Block.Attributes, func(a schema.Attribute) bool { return a.Name == f })
			if i < 0 {
				return schema.Attribute{}, false
			}
			return nb.Block.Attributes[i], true
		}
		out = append(out, nestedSet{name: nb.Name, shape: shape, optional: func(f string) bool {
			a, ok := attr(f)
			return ok && a.Optional
		}, computed: func(f string) bool {
			a, ok := attr(f)
			return ok && a.Computed
		}})
	}
	return out
}

// zeroFields lists the optional fields that hold a zero value in the
// elements of v, the value of set: each "" first, then the rest.
func zeroFields(set nestedSet, v value.Value) []zeroField {
	var strs, rest []zeroField
	add := func(e value.Value, f zeroField) {
		if e.Kind() != value.Object {
			return
		}
		for _, k := range e.Keys() {
			fv := e.Get(k)
			if !set.optional(k) || isNull(fv) || !isZero(fv) {
				continue
			}
			g := f
			g.name, g.path = k, f.path.Attr(k)
			if fv.Kind() == value.String {
				strs = append(strs, g)
			} else {
				rest = append(rest, g)
			}
		}
	}
	sp := value.Path{}.Attr(set.name)
	switch {
	case isNull(v):
	case set.shape == types.Record:
		add(v, zeroField{path: sp, set: set.name, index: -1, record: true})
	case set.shape == types.List && v.Kind() == value.List:
		for i := range v.Len() {
			add(v.Index(i), zeroField{path: sp.Index(int64(i)), set: set.name, index: i})
		}
	case set.shape == types.Map && v.Kind() == value.Object:
		for _, k := range v.Keys() {
			add(v.Get(k), zeroField{path: sp.Key(k), set: set.name, index: -1, key: k})
		}
	}
	return append(strs, rest...)
}

// nullField returns config with the field f names set to null.
func nullField(config value.Value, f zeroField) value.Value {
	sv := config.Get(f.set)
	switch {
	case f.record:
		sv = withField(sv, f.name, value.NullVal())
	case f.index >= 0:
		elems := make([]value.Value, sv.Len())
		for i := range sv.Len() {
			elems[i] = sv.Index(i)
		}
		elems[f.index] = withField(elems[f.index], f.name, value.NullVal())
		out := value.ListVal(elems)
		if sv.IsSecret() {
			out = out.Secret()
		}
		sv = out
	default:
		sv = withField(sv, f.key, withField(sv.Get(f.key), f.name, value.NullVal()))
	}
	return withField(config, f.set, sv)
}

// fieldOf returns the value of the field f names in config.
func fieldOf(config value.Value, f zeroField) value.Value {
	sv := config.Get(f.set)
	switch {
	case f.record:
	case f.index >= 0:
		sv = sv.Index(f.index)
	default:
		sv = sv.Get(f.key)
	}
	return sv.Get(f.name)
}

// withField returns the object v with its field k set to nv.
func withField(v value.Value, k string, nv value.Value) value.Value {
	fields := make(map[string]value.Value, v.Len())
	for _, key := range v.Keys() {
		fields[key] = v.Get(key)
	}
	fields[k] = nv
	out := value.ObjectVal(fields)
	if v.IsSecret() {
		out = out.Secret()
	}
	return out
}
