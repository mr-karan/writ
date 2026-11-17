package schema

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/types"
)

// refIndex resolves the resource type an attribute refers to. It knows every
// resource type the provider has, and the families and slot suffixes from the
// annotation table.
type refIndex struct {
	byWire   map[string]*entry
	families map[string]family // by member wire name, for members this provider has
	names    map[string]string
	suffixes map[string][]string
}

func newRefIndex(anns providerAnnotations, byWire map[string]*entry) refIndex {
	ri := refIndex{byWire: byWire, families: map[string]family{}, names: anns.Names, suffixes: anns.Suffixes}
	for _, f := range anns.Families {
		for _, w := range f.Wires {
			if _, ok := byWire[w]; ok {
				ri.families[w] = f
			}
		}
	}
	return ri
}

// kind is the reference kind of a resource type's computed id.
func (ri *refIndex) kind(wire string) types.RefKind {
	if f, ok := ri.families[wire]; ok {
		return f.Kind
	}
	return types.IdRef
}

// self is the type of a resource type's own computed id.
func (ri *refIndex) self(wire string) types.Type {
	return types.RefTo(ri.kind(wire), ri.byWire[wire].name)
}

// hasArn reports whether a resource type's computed arn is a reference to
// it: its id is its ARN, or its family says so.
func (ri *refIndex) hasArn(wire string) bool {
	f, ok := ri.families[wire]
	return ok && (f.Kind == types.ArnRef || f.Arn)
}

// target is the reference to a family: any member's id is accepted, because
// members share one namespace of IDs. ok is false when wire is in no family
// this provider has.
func (ri *refIndex) target(wire string) (types.Type, bool) {
	f, ok := ri.families[wire]
	if !ok {
		return types.Type{}, false
	}
	return types.RefTo(f.Kind, ri.members(f)...), true
}

func (ri *refIndex) members(f family) []types.Name {
	var names []types.Name
	for _, w := range f.Wires {
		if e, ok := ri.byWire[w]; ok {
			names = append(names, e.name)
		}
	}
	return names
}

// slotTarget is the reference of the given kind to the families of wires,
// for a slot that inference reads. A family is a target of an id slot when
// its ids are plain ids that its API takes alone, and of an ARN slot when its
// members' ARNs are typed. ok is false when no family qualifies.
func (ri *refIndex) slotTarget(kind types.RefKind, wires ...string) (types.Type, bool) {
	var names []types.Name
	for _, w := range wires {
		f, ok := ri.families[w]
		if !ok {
			continue
		}
		switch {
		case kind == types.IdRef && f.Kind == types.IdRef && !f.IdTakesArn,
			kind == types.ArnRef && (f.Kind == types.ArnRef || f.Arn):
			names = append(names, ri.members(f)...)
		}
	}
	if len(names) == 0 {
		return types.Type{}, false
	}
	return types.RefTo(kind, names...), true
}

// slotStem splits a slot name into its stem and the kind of reference its
// suffix asks for: subnet_id is an id of a subnet, and target_group_arns are
// ARNs of target groups when the slot is a list.
func slotStem(attr string, base types.Type) (string, types.RefKind, bool) {
	plural := ""
	if base.Kind() == types.List {
		plural = "s"
	}
	for _, s := range []struct {
		suffix string
		kind   types.RefKind
	}{{"_id", types.IdRef}, {"_arn", types.ArnRef}} {
		if stem, ok := strings.CutSuffix(attr, s.suffix+plural); ok && stem != "" {
			return stem, s.kind, true
		}
	}
	return "", 0, false
}

// local returns the most specific resource type named after stem in the
// service of owner: instance in aws_connect_queue is aws_connect_instance,
// not aws_instance.
func (ri *refIndex) local(owner, stem string) (string, bool) {
	segs := strings.Split(owner, "_")
	for k := len(segs); k >= 1; k-- {
		cand := strings.Join(segs[:k], "_") + "_" + stem
		if _, ok := ri.byWire[cand]; ok {
			return cand, true
		}
	}
	return "", false
}

// suffix returns the longest stem in the suffix table that stem is or ends
// with, and the members it refers to. An entry with no members stops a
// shorter one, so secondary_subnet_id is not a subnet.
func (ri *refIndex) suffix(stem string, kind types.RefKind) (string, []string, bool) {
	var best string
	var wires []string
	for k, ws := range ri.suffixes {
		s, kk, ok := slotStem(k, types.TString)
		if !ok || kk != kind || len(s) <= len(best) || stem != s && !strings.HasSuffix(stem, "_"+s) {
			continue
		}
		best, wires = s, ws
	}
	return best, wires, best != ""
}

// infer reads the resource type an attribute names, from its suffix: _id and
// _ids name ids, _arn and _arns name ARNs. It tries, in order:
//
//   - The stem as a type in the owner's service: subnet_id and subnet_ids
//     in owner refer to <provider>_subnet, and the most specific candidate
//     wins, so instance_id in aws_connect_queue means aws_connect_instance.
//     A candidate decides: when it is in no family that is a target of the
//     slot, or is the owner itself, the slot stays plain. The owner's own
//     <x>_id is its identifier rather than a reference, except in a data
//     source, whose own <x>_id is the id of the resource it reads.
//   - The suffix table, for a stem that names a role rather than a type:
//     execution_role_arn ends with role_arn. When the owner's service has a
//     type named after the table's stem that is not a target, such as
//     aws_dms_certificate for certificate_arn, the slot stays plain. An
//     entry that holds the candidate of the first step widens it, so
//     iam_role_arn and role_arn take the same types.
func (ri *refIndex) infer(owner, attr string, base types.Type, data bool) (types.Type, bool) {
	stem, kind, ok := slotStem(attr, base)
	if !ok {
		return types.Type{}, false
	}
	key, wires, matched := ri.suffix(stem, kind)
	if cand, ok := ri.local(owner, stem); ok {
		if cand == owner && !data {
			return types.Type{}, false
		}
		if !matched || !ri.inFamilies(wires, cand) {
			wires = []string{cand}
		}
		return ri.slotTarget(kind, wires...)
	}
	if !matched || len(wires) == 0 {
		return types.Type{}, false
	}
	if cand, ok := ri.local(owner, key); ok && !ri.inFamilies(wires, cand) {
		return types.Type{}, false
	}
	return ri.slotTarget(kind, wires...)
}

// inFamilies reports whether wire is in the family of any of wires.
func (ri *refIndex) inFamilies(wires []string, wire string) bool {
	return slices.ContainsFunc(wires, func(w string) bool {
		return slices.Contains(ri.families[w].Wires, wire)
	})
}

// refShape returns ref for a String attribute and [ref] for a list of
// strings. ok is false for any other base type.
func refShape(base, ref types.Type) (types.Type, bool) {
	switch {
	case base.Kind() == types.String:
		return ref, true
	case base.Kind() == types.List && base.Elem().Kind() == types.String:
		return types.ListOf(ref), true
	}
	return types.Type{}, false
}

// annotator refines the attributes of one resource type or data source.
type annotator struct {
	refs  *refIndex
	owner string
	ann   annotation
	// data is set for a data source. Its owner names a data source, whose id
	// is the id of the resource type of the same name, if there is one.
	data bool
}

func (a annotator) block(b Block, prefix string) (Block, error) {
	out := Block{Deprecated: b.Deprecated, DeprecationMessage: b.DeprecationMessage, Attributes: make([]Attribute, len(b.Attributes)), Blocks: make([]NestedBlock, len(b.Blocks))}
	for i, attr := range b.Attributes {
		path := prefix + attr.Name
		t, err := a.attrType(attr, path, prefix == "")
		if err != nil {
			return Block{}, err
		}
		attr.Type = t
		// SDKv2 marks every id optional and computed only for backward
		// compatibility. No config sets it, so Writ treats it as read-only.
		// A data source's id is often a lookup argument, so it stays settable.
		if prefix == "" && attr.Name == "id" && attr.Computed && !a.data {
			attr.Optional = false
		}
		attr.Filled = prefix == "" && attr.Computed && slices.Contains(a.ann.Filled, attr.Name)
		attr.Sensitive = attr.Sensitive || slices.Contains(a.ann.Secret, path)
		if attr.Sensitive {
			attr.Type = types.Secret(attr.Type)
		}
		out.Attributes[i] = attr
	}
	for i, nb := range b.Blocks {
		inner, err := a.block(nb.Block, prefix+nb.Name+".")
		if err != nil {
			return Block{}, err
		}
		nb.Block = inner
		if rule, ok := a.ann.ImportDisabled[prefix+nb.Name]; ok {
			nb.ImportDisabled = &rule
		}
		if owner, ok := a.ann.ImportOwners[prefix+nb.Name]; ok && owner.Computed && nb.Block.Deprecated && (nb.Block.DeprecationMessage == "" || strings.Contains(nb.Block.DeprecationMessage, owner.Wire)) {
			nb.ImportOwner = &owner
		}
		nb.single = nb.single || slices.Contains(a.ann.Single, prefix+nb.Name)
		out.Blocks[i] = nb
	}
	return out, nil
}

// attrType applies, in order: an explicit type from the table, an explicit
// opt-out, an explicit reference, the owner's own id and ARN, a reference by
// attribute name, and a reference inferred from the name.
func (a annotator) attrType(attr Attribute, path string, top bool) (types.Type, error) {
	if refined, ok := a.ann.Attrs[path]; ok {
		if m, ok := types.Assign(attr.Type, refined); !ok {
			return types.Type{}, fmt.Errorf("annotation for %s does not refine %s: %s", path, attr.Type, m.Message(path))
		}
		return refined, nil
	}
	if slices.Contains(a.ann.Plain, path) {
		return attr.Type, nil
	}
	if wire, ok := a.ann.Refs[path]; ok {
		ref, ok := a.refs.target(wire)
		if !ok {
			return attr.Type, nil
		}
		t, ok := refShape(attr.Type, ref)
		if !ok {
			return types.Type{}, fmt.Errorf("annotation for %s refers to %s, but %s is neither a string nor a list of strings", path, wire, attr.Type)
		}
		return t, nil
	}
	readOnly := attr.Computed && !attr.Optional && !attr.Required
	if e, ownsType := a.refs.byWire[a.owner]; top && ownsType && attr.Computed && attr.Type.Kind() == types.String {
		if attr.Name == "id" {
			return a.refs.self(a.owner), nil
		}
		if attr.Name == "arn" && readOnly && a.refs.hasArn(a.owner) {
			return types.RefTo(types.ArnRef, e.name), nil
		}
	}
	if wire, ok := a.refs.names[attr.Name]; ok {
		if ref, ok := a.refs.target(wire); ok {
			if t, ok := refShape(attr.Type, ref); ok {
				return t, nil
			}
		}
		return attr.Type, nil
	}
	if ref, ok := a.refs.infer(a.owner, attr.Name, attr.Type, a.data); ok {
		if t, ok := refShape(attr.Type, ref); ok {
			return t, nil
		}
	}
	return attr.Type, nil
}
