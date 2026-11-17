package importer

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/tfcode"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// secretIDReason refuses an object whose import id is one of its own
// secrets in OpenTofu code: an import block's id may not be sensitive, and
// tofu plan prints it.
const secretIDReason = "the import id is one of the object's own secrets; OpenTofu refuses a sensitive import id and prints the id of every import in the plan, so writ import never writes it; leave this object out"

// hclShape adds what OpenTofu code cannot vary between the members of one
// resource with for_each: a nested block takes no expression, so every
// member's blocks are the same.
func hclShape(n *bnode) string {
	var b strings.Builder
	for _, nb := range n.schema.Block.Blocks {
		if v := n.config.Get(nb.Name); v.Kind() != value.Null {
			b.WriteString("\x00" + nb.Name + "=" + jsonText(v))
		}
	}
	return b.String()
}

// hclToken is one token of the punctuation an expression needs.
func hclToken(t hclsyntax.TokenType, text string) *hclwrite.Token {
	return &hclwrite.Token{Type: t, Bytes: []byte(text)}
}

// traversal is root.attr.attr... with each step an identifier, or a
// quoted key when it starts with [.
func traversal(root string, steps ...string) hclwrite.Tokens {
	out := hclwrite.TokensForIdentifier(root)
	for _, s := range steps {
		if key, ok := strings.CutPrefix(s, "["); ok {
			out = append(out, hclToken(hclsyntax.TokenOBrack, "["))
			out = append(out, tfcode.String(key)...)
			out = append(out, hclToken(hclsyntax.TokenCBrack, "]"))
			continue
		}
		out = append(out, hclToken(hclsyntax.TokenDot, "."))
		out = append(out, hclwrite.TokensForIdentifier(s)...)
	}
	return out
}

// indexBy is expr[index], and expr[index].attr when attr is set, where
// index is an expression.
func indexBy(expr, index hclwrite.Tokens, attr string) hclwrite.Tokens {
	out := append(slices.Clone(expr), hclToken(hclsyntax.TokenOBrack, "["))
	out = append(out, index...)
	out = append(out, hclToken(hclsyntax.TokenCBrack, "]"))
	if attr == "" {
		return out
	}
	out = append(out, hclToken(hclsyntax.TokenDot, "."))
	return append(out, hclwrite.TokensForIdentifier(attr)...)
}

// objectKey is an object's key: an identifier when it is one, else quoted.
func objectKey(k string) hclwrite.Tokens {
	if hclsyntax.ValidIdentifier(k) {
		return hclwrite.TokensForIdentifier(k)
	}
	return tfcode.String(k)
}

// hclAddr is the OpenTofu address of the object n.
func hclAddr(n *bnode) addr.Addr {
	a := addr.Root(n.schema.Wire).Field(n.group.let)
	if n.group.many() {
		a = a.Key(n.key)
	}
	return a
}

// hclType is the type constraint of a variable that holds a value of type
// t. A record becomes any, so a field the value leaves out is not an error.
func hclType(t types.Type) string {
	switch t.Kind() {
	case types.Bool:
		return "bool"
	case types.Int, types.Float:
		return "number"
	case types.List:
		return "list(" + hclType(t.Elem()) + ")"
	case types.Map:
		return "map(" + hclType(t.Elem()) + ")"
	case types.Record:
		return "any"
	}
	return "string"
}

func comment(body *hclwrite.Body, lines ...string) {
	for _, l := range lines {
		body.AppendUnstructuredTokens(hclwrite.Tokens{hclToken(hclsyntax.TokenComment, "# "+l+"\n")})
	}
}

// renderHCL writes every group as one resource block and one import block,
// with a variable for each secret that no object of the run holds.
func (b *bulkGen) renderHCL() []byte {
	taken := map[string]bool{}
	for _, name := range b.opts.Taken {
		taken[name] = true
	}
	b.gen = newGen(taken)
	for _, n := range b.nodes {
		n.tf = hclAddr(n).String()
	}
	resources := hclwrite.NewEmptyFile()
	body := resources.Body()
	for _, g := range b.order() {
		body.AppendNewline()
		if g.many() {
			comment(body, fmt.Sprintf("%d %s objects", len(g.members), g.members[0].schema.Wire))
		}
		b.resourceHCL(body, g)
		body.AppendNewline()
		b.importHCL(body, g)
	}
	out := hclwrite.NewEmptyFile()
	top := out.Body()
	comment(top,
		"writ import bulk wrote this file. Each resource declares objects that",
		"existed before, and each import block adopts them: tofu plan should",
		"show only imports.")
	for _, p := range b.gen.params {
		top.AppendNewline()
		comment(top, p.Attr+" of "+p.From+" is sensitive, so its value is a variable.")
		v := top.AppendNewBlock("variable", []string{p.Name}).Body()
		v.SetAttributeRaw("type", hclwrite.TokensForIdentifier(p.Type))
		v.SetAttributeRaw("sensitive", hclwrite.TokensForIdentifier("true"))
	}
	var buf bytes.Buffer
	buf.Write(out.Bytes())
	buf.Write(resources.Bytes())
	return hclwrite.Format(buf.Bytes())
}

// resourceHCL writes the resource block of a group.
func (b *bulkGen) resourceHCL(body *hclwrite.Body, g *bgroup) {
	first := g.members[0]
	rb := body.AppendNewBlock("resource", []string{first.schema.Wire, g.let}).Body()
	type varying struct {
		name string
		vals []hclwrite.Tokens
	}
	var vary []varying
	var setters []func()
	keys := make([]hclwrite.Tokens, len(g.members))
	for i, n := range g.members {
		keys[i] = tfcode.String(n.key)
	}
	taken := map[string]bool{}
	for _, f := range b.fields(first) {
		taken[f.name] = true
	}
	var ignored []string
	for _, f := range b.fields(first) {
		if f.initial {
			ignored = append(ignored, f.name)
			for _, n := range g.members {
				b.gen.initials = append(b.gen.initials, initialField{n: n.node, attr: f.name})
			}
		}
		if nb, ok := nestedBlock(first.schema.Block, f.name); ok {
			if g.many() {
				texts := make([][]byte, len(g.members))
				for i, n := range g.members {
					scratch := hclwrite.NewEmptyFile()
					b.blocksHCL(scratch.Body(), n, nb, n.config.Get(f.name), value.Path{}.Attr(f.name))
					texts[i] = scratch.Bytes()
				}
				if slices.ContainsFunc(texts, func(t []byte) bool { return !bytes.Equal(t, texts[0]) }) {
					panic("importer: members of one resource have different " + f.name + " blocks")
				}
			}
			setters = append(setters, func() { b.blocksHCL(rb, first, nb, first.config.Get(f.name), value.Path{}.Attr(f.name)) })
			continue
		}
		exprs := make([]hclwrite.Tokens, len(g.members))
		for i, n := range g.members {
			exprs[i] = b.attrHCL(n, f.name)
		}
		set := func(e hclwrite.Tokens) { setters = append(setters, func() { rb.SetAttributeRaw(f.name, e) }) }
		switch {
		case !slices.ContainsFunc(exprs, func(e hclwrite.Tokens) bool { return !bytes.Equal(e.Bytes(), exprs[0].Bytes()) }):
			set(exprs[0])
		case f.name == g.keyAttr:
			set(traversal("each", "key"))
		default:
			if target, field, refKeys, ok := b.keyedRefHCL(g, f.name, taken); ok {
				set(target)
				vary = append(vary, varying{name: field, vals: refKeys})
				continue
			}
			set(traversal("each", "value", f.name))
			vary = append(vary, varying{name: f.name, vals: exprs})
		}
	}
	if g.many() {
		if len(vary) == 0 {
			rb.SetAttributeRaw("for_each", hclwrite.TokensForFunctionCall("toset", hclwrite.TokensForTuple(keys)))
		} else {
			entries := make([]hclwrite.ObjectAttrTokens, len(g.members))
			for i := range g.members {
				fields := make([]hclwrite.ObjectAttrTokens, len(vary))
				for j, v := range vary {
					fields[j] = hclwrite.ObjectAttrTokens{Name: hclwrite.TokensForIdentifier(v.name), Value: v.vals[i]}
				}
				entries[i] = hclwrite.ObjectAttrTokens{Name: keys[i], Value: hclwrite.TokensForObject(fields)}
			}
			rb.SetAttributeRaw("for_each", hclwrite.TokensForObject(entries))
		}
	}
	if !first.via.IsZero() {
		root, alias, _ := strings.Cut(first.via.String(), ".")
		if alias == "" {
			rb.SetAttributeRaw("provider", hclwrite.TokensForIdentifier(root))
		} else {
			rb.SetAttributeRaw("provider", traversal(root, alias))
		}
	}
	for _, set := range setters {
		set()
	}
	if len(ignored) > 0 {
		names := make([]hclwrite.Tokens, len(ignored))
		for i, name := range ignored {
			names[i] = hclwrite.TokensForIdentifier(name)
		}
		rb.AppendNewBlock("lifecycle", nil).Body().SetAttributeRaw("ignore_changes", hclwrite.TokensForTuple(names))
	}
}

// importHCL writes the import block of a group: with for_each over the
// group's keys when it has several members.
func (b *bulkGen) importHCL(body *hclwrite.Body, g *bgroup) {
	first := g.members[0]
	ib := body.AppendNewBlock("import", nil).Body()
	if !g.many() {
		ib.SetAttributeRaw("to", traversal(first.schema.Wire, g.let))
		ib.SetAttributeRaw("id", tfcode.String(first.id))
		return
	}
	byKey := !slices.ContainsFunc(g.members, func(n *bnode) bool { return n.key != n.id })
	if byKey {
		keys := make([]hclwrite.Tokens, len(g.members))
		for i, n := range g.members {
			keys[i] = tfcode.String(n.key)
		}
		ib.SetAttributeRaw("for_each", hclwrite.TokensForFunctionCall("toset", hclwrite.TokensForTuple(keys)))
	} else {
		entries := make([]hclwrite.ObjectAttrTokens, len(g.members))
		for i, n := range g.members {
			entries[i] = hclwrite.ObjectAttrTokens{Name: tfcode.String(n.key), Value: tfcode.String(n.id)}
		}
		ib.SetAttributeRaw("for_each", hclwrite.TokensForObject(entries))
	}
	ib.SetAttributeRaw("to", indexBy(traversal(first.schema.Wire, g.let), traversal("each", "key"), ""))
	if byKey {
		ib.SetAttributeRaw("id", traversal("each", "key"))
	} else {
		ib.SetAttributeRaw("id", traversal("each", "value"))
	}
}

// keyedRefHCL is keyedRef in OpenTofu code: the body reads the other
// resource by the key that each member's map entry holds.
func (b *bulkGen) keyedRefHCL(g *bgroup, f string, taken map[string]bool) (hclwrite.Tokens, string, []hclwrite.Tokens, bool) {
	var target *bgroup
	var attr string
	keys := make([]hclwrite.Tokens, len(g.members))
	for i, n := range g.members {
		src := b.decided[n][value.Path{}.Attr(f).String()]
		if src == nil || src.run == nil || !src.run.group.many() || n.config.Get(f).Kind() != value.String {
			return nil, "", nil, false
		}
		if target == nil {
			target, attr = src.run.group, src.attr
		}
		if src.run.group != target || src.attr != attr {
			return nil, "", nil, false
		}
		keys[i] = tfcode.String(src.run.key)
	}
	field := target.let
	for i := 2; taken[field]; i++ {
		field = target.let + "_" + strconv.Itoa(i)
	}
	taken[field] = true
	expr := indexBy(traversal(target.members[0].schema.Wire, target.let), traversal("each", "value", field), attr)
	return expr, field, keys, true
}

func nestedBlock(b schema.Block, name string) (schema.NestedBlock, bool) {
	i := slices.IndexFunc(b.Blocks, func(nb schema.NestedBlock) bool { return nb.Name == name })
	if i < 0 {
		return schema.NestedBlock{}, false
	}
	return b.Blocks[i], true
}

// attrHCL is the expression of one top-level attribute of n's config.
func (b *bulkGen) attrHCL(n *bnode, name string) hclwrite.Tokens {
	i := slices.IndexFunc(n.schema.Block.Attributes, func(a schema.Attribute) bool { return a.Name == name })
	a := n.schema.Block.Attributes[i]
	return b.exprHCL(n, n.config.Get(name), a.Type, a.Sensitive, value.Path{}.Attr(name))
}

// blocksHCL writes the blocks of the nested block type nb that v holds.
func (b *bulkGen) blocksHCL(body *hclwrite.Body, n *bnode, nb schema.NestedBlock, v value.Value, p value.Path) {
	switch {
	case v.Kind() == value.Null:
	case nb.IsRecord():
		b.blockBodyHCL(body.AppendNewBlock(nb.Name, nil).Body(), n, nb.Block, v, p)
	case nb.Nesting == schema.NestMap:
		for _, k := range v.Keys() {
			b.blockBodyHCL(body.AppendNewBlock(nb.Name, []string{k}).Body(), n, nb.Block, v.Get(k), p.Key(k))
		}
	default:
		for i := range v.Len() {
			b.blockBodyHCL(body.AppendNewBlock(nb.Name, nil).Body(), n, nb.Block, v.Index(i), p.Index(int64(i)))
		}
	}
}

func (b *bulkGen) blockBodyHCL(body *hclwrite.Body, n *bnode, blk schema.Block, v value.Value, p value.Path) {
	for _, a := range blk.Attributes {
		if fv := v.Get(a.Name); fv.Kind() != value.Null {
			body.SetAttributeRaw(a.Name, b.exprHCL(n, fv, a.Type, a.Sensitive, p.Attr(a.Name)))
		}
	}
	for _, nb := range blk.Blocks {
		b.blocksHCL(body, n, nb, v.Get(nb.Name), p.Attr(nb.Name))
	}
}

// exprHCL writes one value, with the references decide chose, and a
// variable for each secret.
func (b *bulkGen) exprHCL(n *bnode, v value.Value, t types.Type, slotSecret bool, p value.Path) hclwrite.Tokens {
	slotSecret = slotSecret || t.IsSecret()
	param := func() hclwrite.Tokens { return traversal("var", b.gen.param(n.node, v, t, slotSecret, p)) }
	if secretContainer(v) {
		return param()
	}
	switch v.Kind() {
	case value.Null:
		return hclwrite.TokensForIdentifier("null")
	case value.String:
		if src := b.decided[n][p.String()]; src != nil {
			return b.refHCL(src)
		}
		if v.IsSecret() {
			return param()
		}
		return tfcode.Scalar(v)
	case value.Int, value.Float, value.Bool:
		if v.IsSecret() {
			return param()
		}
		return tfcode.Scalar(v)
	case value.List:
		elems := make([]hclwrite.Tokens, v.Len())
		for i := range elems {
			elems[i] = b.exprHCL(n, v.Index(i), t.Elem(), slotSecret, p.Index(int64(i)))
		}
		return hclwrite.TokensForTuple(elems)
	case value.Object:
		var fields []hclwrite.ObjectAttrTokens
		for _, k := range v.Keys() {
			fv := v.Get(k)
			ft := t.Elem()
			if t.Kind() == types.Record {
				f, _ := t.Field(k)
				ft = f.Type
				if fv.Kind() == value.Null && f.Optional {
					continue
				}
			}
			fields = append(fields, hclwrite.ObjectAttrTokens{Name: objectKey(k), Value: b.exprHCL(n, fv, ft, slotSecret, p.Key(k))})
		}
		return hclwrite.TokensForObject(fields)
	}
	panic(fmt.Sprintf("importer: cannot write a %s value", v.Kind()))
}

// refHCL reads src, an attribute of another object of the run.
func (b *bulkGen) refHCL(src *bsource) hclwrite.Tokens {
	g := src.run.group
	if g.many() {
		return traversal(src.run.schema.Wire, g.let, "["+src.run.key, src.attr)
	}
	return traversal(src.run.schema.Wire, g.let, src.attr)
}
