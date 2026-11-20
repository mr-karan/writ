package server

import (
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/mr-karan/writ/internal/diag"
)

// parseHCL parses a file in HCL's native syntax. It evaluates nothing.
func parseHCL(file string, src []byte) (*hclsyntax.Body, diag.List) {
	f, hd := hclsyntax.ParseConfig(src, file, hcl.InitialPos)
	var diags diag.List
	for _, x := range hd {
		msg := x.Summary
		if x.Detail != "" {
			msg += ": " + x.Detail
		}
		msg = strings.TrimSuffix(msg, ".")
		if msg != "" {
			msg = strings.ToLower(msg[:1]) + msg[1:]
		}
		var span diag.Span
		if x.Subject != nil {
			span = spanOf(*x.Subject)
		}
		if x.Severity == hcl.DiagError {
			diags.Errorf(diag.Syntax, span, "%s", msg)
		} else {
			diags.Warnf(diag.Syntax, span, "%s", msg)
		}
	}
	if hd.HasErrors() {
		return nil, diags
	}
	return f.Body.(*hclsyntax.Body), diags
}

type decoder struct {
	dir   string
	src   []byte
	diags diag.List
}

func spanOf(r hcl.Range) diag.Span {
	return diag.Span{File: r.Filename,
		Start: diag.Pos{Line: r.Start.Line, Col: r.Start.Column, Offset: r.Start.Byte},
		End:   diag.Pos{Line: r.End.Line, Col: r.End.Column, Offset: r.End.Byte}}
}

// at is the span of a node, a block's type and labels, or a range.
func at(n any) diag.Span {
	switch x := n.(type) {
	case hcl.Range:
		return spanOf(x)
	case *hclsyntax.Block:
		return spanOf(x.DefRange())
	case hclsyntax.Node:
		return spanOf(x.Range())
	}
	return diag.Span{}
}

func (d *decoder) errorf(n any, format string, args ...any) {
	d.diags.Errorf(diag.Usage, at(n), format, args...)
}

func (d *decoder) warnf(n any, format string, args ...any) {
	d.diags.Warnf(diag.Usage, at(n), format, args...)
}

// field is one setting: an attribute's name and expression, or a block
// that appears once, or one key of an object and its value.
type field struct {
	key   hcl.Range
	value hclsyntax.Node
}

// block is the block a field of a body holds.
func (f field) block() *hclsyntax.Block { return f.value.(*hclsyntax.Block) }

// shape is what a body may hold: attributes, blocks that appear at most
// once, blocks that repeat, and blocks that repeat with one label, a name
// that no other block of the type has.
type shape struct {
	attrs, blocks, lists, named []string
	// removed names the settings Writ no longer has, each with why, so an
	// old config is refused with the setting named.
	removed map[string]string
}

// body reads b as s allows. Attributes and single blocks come back by
// name, and repeated blocks by type in the order written.
func (d *decoder) body(b *hclsyntax.Body, what string, s shape) (map[string]field, map[string][]*hclsyntax.Block) {
	one := map[string]field{}
	many := map[string][]*hclsyntax.Block{}
	unknown := func(r hcl.Range, name string) {
		if why, ok := s.removed[name]; ok {
			d.errorf(r, "%s: %s was removed: %s; delete it", what, name, why)
			return
		}
		all := slices.Sorted(slices.Values(slices.Concat(s.attrs, s.blocks, s.lists, s.named)))
		d.errorf(r, "%s has no field %s; it has %s", what, name, strings.Join(all, ", "))
	}
	for _, name := range slices.Sorted(maps.Keys(b.Attributes)) {
		a := b.Attributes[name]
		switch {
		case slices.Contains(s.attrs, name):
			one[name] = field{key: a.NameRange, value: a.Expr}
		case slices.Contains(s.named, name):
			d.errorf(a.NameRange, "%s is a block in %s: write %s \"NAME\" { ... }", name, what, name)
		case slices.Contains(s.blocks, name), slices.Contains(s.lists, name):
			d.errorf(a.NameRange, "%s is a block in %s: write %s { ... }", name, what, name)
		default:
			unknown(a.NameRange, name)
		}
	}
	names := map[string]bool{}
	for _, blk := range b.Blocks {
		t := blk.Type
		switch {
		case slices.Contains(s.blocks, t), slices.Contains(s.lists, t):
			if len(blk.Labels) != 0 {
				d.errorf(blk.LabelRanges[0], "%s takes no label: write %s { ... }", t, t)
				continue
			}
			if slices.Contains(s.lists, t) {
				many[t] = append(many[t], blk)
			} else if _, dup := one[t]; dup {
				d.errorf(blk.TypeRange, "%s sets %s twice", what, t)
			} else {
				one[t] = field{key: blk.TypeRange, value: blk}
			}
		case slices.Contains(s.named, t):
			if len(blk.Labels) != 1 {
				d.errorf(blk, "%s takes one label, its name: write %s \"NAME\" { ... }", t, t)
				continue
			}
			if names[t+" "+blk.Labels[0]] {
				d.errorf(blk.LabelRanges[0], "%s names %s %s twice", what, t, blk.Labels[0])
				continue
			}
			names[t+" "+blk.Labels[0]] = true
			many[t] = append(many[t], blk)
		case slices.Contains(s.attrs, t):
			d.errorf(blk.TypeRange, "%s is an attribute in %s: write %s = ...", t, what, t)
		default:
			unknown(blk.TypeRange, t)
		}
	}
	return one, many
}

// attrs reads a block that holds only the attributes names.
func (d *decoder) attrs(blk *hclsyntax.Block, what string, names ...string) map[string]field {
	one, _ := d.body(blk.Body, what, shape{attrs: names})
	return one
}

// object reads an object { NAME = VALUE, ... } with any keys.
func (d *decoder) object(e hclsyntax.Node, what string) map[string]field {
	o, ok := e.(*hclsyntax.ObjectConsExpr)
	if !ok {
		d.errorf(e, "%s must be an object { NAME = ... }", what)
		return nil
	}
	out := map[string]field{}
	for _, it := range o.Items {
		name := objectKey(it.KeyExpr)
		if name == "" {
			d.errorf(it.KeyExpr, "%s: a key must be a name or a plain string", what)
			continue
		}
		if _, dup := out[name]; dup {
			d.errorf(it.KeyExpr, "%s sets %s twice", what, name)
			continue
		}
		out[name] = field{key: it.KeyExpr.Range(), value: it.ValueExpr}
	}
	return out
}

func objectKey(e hclsyntax.Expression) string {
	k, ok := e.(*hclsyntax.ObjectConsKeyExpr)
	if !ok || k.ForceNonLiteral {
		return ""
	}
	if name := hcl.ExprAsKeyword(k.Wrapped); name != "" {
		return name
	}
	s, _ := literalString(k.Wrapped)
	return s
}

// literalString is the value of a quoted string with no interpolation.
func literalString(e hclsyntax.Node) (string, bool) {
	t, ok := e.(*hclsyntax.TemplateExpr)
	if !ok || !t.IsStringLiteral() {
		return "", false
	}
	v, diags := t.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() {
		return "", false
	}
	return v.AsString(), true
}

// literal is the text of a number, bool, or null literal as written, and
// "" for any other node.
func (d *decoder) literal(e hclsyntax.Node) string {
	l, ok := e.(*hclsyntax.LiteralValueExpr)
	if !ok {
		return ""
	}
	return string(d.src[l.SrcRange.Start.Byte:l.SrcRange.End.Byte])
}

// isEmptyString reports whether e is the string "".
func isEmptyString(e hclsyntax.Node) bool {
	s, ok := literalString(e)
	return ok && s == ""
}

func (d *decoder) str(e hclsyntax.Node, what string) string {
	s, ok := literalString(e)
	if !ok {
		d.errorf(e, "%s must be a plain string", what)
	}
	return s
}

func (d *decoder) list(e hclsyntax.Node, what string) []hclsyntax.Expression {
	l, ok := e.(*hclsyntax.TupleConsExpr)
	if !ok {
		d.errorf(e, "%s must be a list [ ... ]", what)
		return nil
	}
	return l.Exprs
}

func (d *decoder) strs(e hclsyntax.Node, what string) []string {
	var out []string
	for _, x := range d.list(e, what) {
		out = append(out, d.str(x, what+" element"))
	}
	return out
}

// wholeNumber is the value of a whole number literal.
func (d *decoder) wholeNumber(e hclsyntax.Node) (int64, bool) {
	n, err := strconv.ParseInt(d.literal(e), 10, 64)
	return n, err == nil
}

func (d *decoder) integer(e hclsyntax.Node, what string) int {
	n, ok := d.wholeNumber(e)
	if !ok || n <= 0 || n > 1<<20 {
		d.errorf(e, "%s must be a whole number from 1 to %d", what, 1<<20)
		return 0
	}
	return int(n)
}

func (d *decoder) duration(e hclsyntax.Node, what string) time.Duration {
	s := d.str(e, what)
	if s == "" {
		return 0
	}
	v, err := time.ParseDuration(s)
	if days, ok := strings.CutSuffix(s, "d"); ok && err != nil {
		var n int64
		if n, err = strconv.ParseInt(days, 10, 64); err == nil && n > 0 && n < 100000 {
			v = time.Duration(n) * 24 * time.Hour
		}
	}
	if err != nil || v <= 0 {
		d.errorf(e, "%s must be a duration such as \"24h\" or \"30d\", got %q", what, s)
		return 0
	}
	return v
}

func (d *decoder) size(e hclsyntax.Node, what string) int64 {
	s := d.str(e, what)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}} {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseInt(n, 10, 64)
			if err == nil && v > 0 && v <= (1<<31)/u.mult {
				return v * u.mult
			}
			break
		}
	}
	d.errorf(e, "%s must be a size such as \"16MiB\", up to 2GiB, got %q", what, s)
	return 0
}

// sizeWithin reads a size from lo to hi bytes, written in GiB, MiB, or
// KiB, such as "4GiB".
func (d *decoder) sizeWithin(e hclsyntax.Node, what string, lo, hi int64) int64 {
	s := d.str(e, what)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseInt(n, 10, 64)
			if err == nil && v > 0 && v <= hi/u.mult && v*u.mult >= lo {
				return v * u.mult
			}
			break
		}
	}
	d.errorf(e, "%s must be a size from %dMiB to %dGiB, such as \"4GiB\", got %q", what, lo>>20, hi>>30, s)
	return 0
}

// cpus reads a number of CPUs, such as 2 or 0.5, from minRunCPUs to
// maxRunCPUs.
func (d *decoder) cpus(e hclsyntax.Node, what string) float64 {
	v, err := strconv.ParseFloat(d.literal(e), 64)
	if err != nil || v < minRunCPUs || v > maxRunCPUs {
		d.errorf(e, "%s must be a number of CPUs from %v to %v, such as 2 or 0.5", what, minRunCPUs, maxRunCPUs)
		return 0
	}
	return v
}

// path reads a file or directory path. A relative one is relative to the
// config file's directory.
func (d *decoder) path(e hclsyntax.Node, what string) string {
	s := d.str(e, what)
	if s == "" {
		d.errorf(e, "%s must not be empty", what)
		return ""
	}
	if !filepath.IsAbs(s) {
		s = filepath.Join(d.dir, s)
	}
	return filepath.Clean(s)
}
