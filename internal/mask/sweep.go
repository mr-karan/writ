package mask

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// minSubstring is the shortest secret the sweep replaces inside a longer
// string. A shorter one is replaced only where it is a whole value, so that a
// secret such as "1" does not blank the document.
const minSubstring = 6

// minNumber is the fewest characters a sensitive number's text must have
// for the sweep to learn it. It is minSubstring, so a learned number is
// also caught inside a longer string, as tostring() leaves it. Every port
// is five digits or fewer, and so are most counts and sizes in a plan.
const minNumber = minSubstring

// maxDepth bounds how many encodings the sweep undoes one inside another,
// such as a JSON escape inside base64.
const maxDepth = 2

// maxWholeEncoded is the longest text that can encode a secret shorter than
// minSubstring through maxDepth encodings. The widest encoding, a JSON \u
// escape, writes one byte as six, so each layer multiplies by six.
const maxWholeEncoded = 6 * 6 * (minSubstring - 1)

// MaxSecretBytes bounds the secrets one run may collect, and with them the
// matcher the sweep builds, at about 20 bytes a secret byte.
const MaxSecretBytes = 4 << 20

// maxStringWork bounds the bytes the sweep may decode and match for one
// string. Undoing up to maxDepth encodings costs at most about 30 bytes for
// each byte of the string, so a string of a megabyte or so passes.
const maxStringWork = 32 << 20

var (
	errTooManySecrets = fmt.Errorf("the run holds more than %d bytes of secrets, more than the mask sweeps", MaxSecretBytes)
	errTooMuchWork    = fmt.Errorf("a string costs more than %d bytes of decoding to sweep, more than the mask allows", maxStringWork)
)

// sweeper replaces known secrets in strings, as plain text and inside the
// encodings that scan decodes: URL escapes, JSON escapes, hex, and base64.
type sweeper struct {
	long  *automaton
	whole map[string]bool // secrets shorter than minSubstring, folded
}

func newSweeper(secrets map[string]struct{}) (*sweeper, error) {
	sw := &sweeper{whole: map[string]bool{}}
	var long []string
	total := 0
	for s := range secrets {
		total += len(s)
		if len(s) < minSubstring {
			sw.whole[foldString(s)] = true
			continue
		}
		long = append(long, s)
	}
	if total > MaxSecretBytes {
		return nil, errTooManySecrets
	}
	if len(long) > 0 {
		sw.long = newAutomaton(long)
	}
	return sw, nil
}

// scan sweeps one document. Once a string costs more than maxStringWork,
// every string becomes Sensitive and err is set, so the caller stores
// nothing.
type scan struct {
	*sweeper
	left int // bytes the current string may still cost
	err  error
	bufs [maxDepth]scratch
}

func (s *scan) charge(n int) bool {
	s.left -= n
	if s.left < 0 && s.err == nil {
		s.err = errTooMuchWork
	}
	return s.err == nil
}

// str returns x with every secret replaced. A replacement can join with its
// neighbours into a new match; a string that still matches after one round
// of replacements is replaced whole.
func (s *scan) str(x string) string {
	s.left = maxStringWork
	for round := range 2 {
		if s.err != nil || s.isWhole(x) {
			return Sensitive
		}
		spans := s.find(x)
		if s.err != nil {
			return Sensitive
		}
		if len(spans) == 0 {
			return x
		}
		if round == 1 {
			break
		}
		x = replace(x, spans)
	}
	return Sensitive
}

// isWhole reports whether x, or the whole of x decoded through up to
// maxDepth encodings, is a short secret.
func (s *scan) isWhole(x string) bool {
	if len(s.whole) == 0 || len(x) > maxWholeEncoded {
		return false
	}
	return s.wholeAt([]byte(x), 0)
}

func (s *scan) wholeAt(b []byte, depth int) bool {
	if s.whole[foldString(string(b))] {
		return true
	}
	if depth == maxDepth {
		return false
	}
	return slices.ContainsFunc(decodings(b), func(f []byte) bool { return s.wholeAt(f, depth+1) })
}

// decodings returns every whole-string decoding of b that the sweep
// knows: URL escapes, JSON escapes, hex, and base64.
func decodings(b []byte) [][]byte {
	var forms [][]byte
	if v, ok := decodePercent(b, &scratch{}); ok {
		forms = append(forms, v.text)
	}
	if v, ok := decodeJSON(b, &scratch{}); ok {
		forms = append(forms, v.text)
	}
	if h, err := hex.DecodeString(string(b)); err == nil {
		forms = append(forms, h)
	}
	b64 := strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimRight(string(b), "="))
	if d, err := base64.RawStdEncoding.DecodeString(b64); err == nil {
		forms = append(forms, d)
	}
	return forms
}

// find returns the spans of x that hold a long secret, plain or inside up
// to maxDepth encodings.
func (s *scan) find(x string) [][2]int {
	if s.long == nil || len(x) < minSubstring || !s.charge(len(x)) {
		return nil
	}
	spans := find(s.long, x, nil)
	s.decoded([]byte(x), nil, 0, &spans)
	return spans
}

// decoded matches each view that one decoding of text gives, where text is
// outer's text, or the string itself when outer is nil, and decodes each
// view again until maxDepth. Every view at one depth reuses that depth's
// scratch.
func (s *scan) decoded(text []byte, outer *view, depth int, spans *[][2]int) {
	buf := &s.bufs[depth]
	match := func(v view) bool {
		// A view too short to hold a long secret still decoded; isWhole
		// checks the short secrets.
		if len(v.text) < minSubstring {
			return true
		}
		if !s.charge(len(v.text)) {
			return false
		}
		v.outer = outer
		for _, sp := range find(s.long, v.text, nil) {
			lo, hi := v.span(sp[0], sp[1])
			*spans = append(*spans, [2]int{lo, hi})
		}
		if depth+1 < maxDepth {
			s.decoded(v.text, &v, depth+1, spans)
		}
		return s.err == nil
	}
	if !s.charge(4 * len(text)) {
		return
	}
	if v, ok := decodePercent(text, buf); ok && !match(v) {
		return
	}
	if v, ok := decodeJSON(text, buf); ok && !match(v) {
		return
	}
	for _, r := range runs(text, hexDigit, 2*minSubstring) {
		for off := range 2 {
			if r[1]-r[0]-off >= 2*minSubstring && !match(decodeHex(text[:r[1]], r[0]+off, buf)) {
				return
			}
		}
	}
	for _, r := range runs(text, base64Char, minBase64) {
		for off := range 4 {
			if r[1]-r[0]-off >= minBase64 && !match(decodeBase64(text[:r[1]], r[0]+off, buf)) {
				return
			}
		}
	}
}

func replace(x string, spans [][2]int) string {
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	var out strings.Builder
	at := 0
	for i := 0; i < len(spans); {
		start, end := spans[i][0], spans[i][1]
		for i++; i < len(spans) && spans[i][0] <= end; i++ {
			end = max(end, spans[i][1])
		}
		out.WriteString(x[at:start])
		out.WriteString(Sensitive)
		at = end
	}
	out.WriteString(x[at:])
	return out.String()
}

// view is a text decoded from outer's text, or from the string itself when
// outer is nil. Hex and base64 map their bytes back by arithmetic from
// start; the escapes keep, for each byte i, the span lo[i] to hi[i]-1 it
// came from.
type view struct {
	text   []byte
	outer  *view
	kind   viewKind
	start  int
	lo, hi []int32
}

type viewKind uint8

const (
	escapes viewKind = iota
	hexes
	base64s
)

// span returns the bytes of the string that decode to text[a:b].
func (v *view) span(a, b int) (int, int) {
	var lo, hi int
	switch v.kind {
	case hexes:
		lo, hi = v.start+2*a, v.start+2*b
	case base64s:
		lo, hi = v.start+4*(a/3)+a%3, v.start+4*((b-1)/3)+(b-1)%3+2
	default:
		lo, hi = int(v.lo[a]), int(v.hi[b-1])
	}
	if v.outer != nil {
		return v.outer.span(lo, hi)
	}
	return lo, hi
}

// scratch holds the buffers of the views at one depth.
type scratch struct {
	text   []byte
	lo, hi []int32
}

func (v *view) emit(c byte, lo, hi int) {
	v.text = append(v.text, c)
	v.lo = append(v.lo, int32(lo))
	v.hi = append(v.hi, int32(hi))
}

func escapeView(buf *scratch) view {
	return view{text: buf.text[:0], lo: buf.lo[:0], hi: buf.hi[:0]}
}

// keep returns v's buffers to buf, grown, for the next view at its depth.
func (buf *scratch) keep(v view) {
	buf.text, buf.lo, buf.hi = v.text, v.lo, v.hi
}

// decodePercent undoes %XX escapes in either case. It reports false when t
// holds no escape.
func decodePercent(t []byte, buf *scratch) (view, bool) {
	if !slices.Contains(t, '%') {
		return view{}, false
	}
	v := escapeView(buf)
	escaped := false
	for i := 0; i < len(t); i++ {
		if t[i] == '%' && i+2 < len(t) && isHex(t[i+1]) && isHex(t[i+2]) {
			v.emit(unhex(t[i+1])<<4|unhex(t[i+2]), i, i+3)
			i += 2
			escaped = true
			continue
		}
		v.emit(t[i], i, i+1)
	}
	buf.keep(v)
	return v, escaped
}

// decodeJSON undoes the escapes of a JSON string, with \u in either case
// and surrogate pairs. It reports false when t holds no escape.
func decodeJSON(t []byte, buf *scratch) (view, bool) {
	if !slices.Contains(t, '\\') {
		return view{}, false
	}
	v := escapeView(buf)
	escaped := false
	for i := 0; i < len(t); i++ {
		if t[i] != '\\' || i+1 >= len(t) {
			v.emit(t[i], i, i+1)
			continue
		}
		if c, ok := simpleEscape[t[i+1]]; ok {
			v.emit(c, i, i+2)
			i++
			escaped = true
			continue
		}
		r, n := unicodeEscape(t[i:])
		if n == 0 {
			v.emit(t[i], i, i+1)
			continue
		}
		var enc [utf8.UTFMax]byte
		for _, c := range enc[:utf8.EncodeRune(enc[:], r)] {
			v.emit(c, i, i+n)
		}
		i += n - 1
		escaped = true
	}
	buf.keep(v)
	return v, escaped
}

var simpleEscape = map[byte]byte{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}

// unicodeEscape reads a \uXXXX escape, or a surrogate pair of two, at the
// start of t and returns the rune and the bytes it took, or 0 bytes.
func unicodeEscape(t []byte) (rune, int) {
	r, ok := hex4(t)
	if !ok {
		return 0, 0
	}
	if utf16.IsSurrogate(r) {
		if r2, ok := hex4(t[6:]); ok {
			if p := utf16.DecodeRune(r, r2); p != utf8.RuneError {
				return p, 12
			}
		}
	}
	return r, 6
}

func hex4(t []byte) (rune, bool) {
	if len(t) < 6 || t[0] != '\\' || t[1] != 'u' {
		return 0, false
	}
	var r rune
	for _, c := range t[2:6] {
		if !isHex(c) {
			return 0, false
		}
		r = r<<4 | rune(unhex(c))
	}
	return r, true
}

// decodeHex decodes the hex digits, in either case, of t from start.
func decodeHex(t []byte, start int, buf *scratch) view {
	v := view{text: buf.text[:0], kind: hexes, start: start}
	for i := start; i+1 < len(t); i += 2 {
		v.text = append(v.text, unhex(t[i])<<4|unhex(t[i+1]))
	}
	buf.text = v.text
	return v
}

// minBase64 is the fewest base64 characters that encode minSubstring bytes.
const minBase64 = (4*minSubstring + 2) / 3

// decodeBase64 decodes the base64 characters of t from start, in the
// standard or the URL alphabet, with or without padding.
func decodeBase64(t []byte, start int, buf *scratch) view {
	v := view{text: buf.text[:0], kind: base64s, start: start}
	for g := start; g < len(t); g += 4 {
		n := min(4, len(t)-g)
		var bits uint32
		for k := range 4 {
			bits <<= 6
			if k < n {
				bits |= uint32(base64Value[t[g+k]])
			}
		}
		for j := range n - 1 {
			v.text = append(v.text, byte(bits>>(16-8*j)))
		}
	}
	buf.text = v.text
	return v
}

// runs returns the spans of t, at least min long, whose bytes all belong
// to the class.
func runs(t []byte, class uint8, min int) [][2]int {
	var out [][2]int
	for i := 0; i < len(t); {
		if classes[t[i]]&class == 0 {
			i++
			continue
		}
		j := i + 1
		for j < len(t) && classes[t[j]]&class != 0 {
			j++
		}
		if j-i >= min {
			out = append(out, [2]int{i, j})
		}
		i = j
	}
	return out
}

const (
	hexDigit uint8 = 1 << iota
	base64Char
)

var classes = func() (t [256]uint8) {
	for c := range 256 {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
			t[c] |= hexDigit
		}
		if base64Value[c] != 0xff {
			t[c] |= base64Char
		}
	}
	return t
}()

func isHex(c byte) bool { return classes[c]&hexDigit != 0 }

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

var base64Value = func() (t [256]byte) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := range t {
		t[i] = 0xff
	}
	for i := range len(alphabet) {
		t[alphabet[i]] = byte(i)
	}
	t['-'], t['_'] = 62, 63
	return t
}()

// tree sweeps every string, map key, and number of a decoded JSON value.
// A number whose text holds a secret becomes Sensitive. A bool carries one
// bit and is left to the marks.
func (s *scan) tree(v any) any {
	switch v := v.(type) {
	case string:
		return s.str(v)
	case json.Number:
		if s.str(string(v)) != string(v) {
			return Sensitive
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[s.str(k)] = s.tree(x)
		}
		return out
	case []any:
		for i, x := range v {
			v[i] = s.tree(x)
		}
		return v
	default:
		return v
	}
}

// walk sweeps every string, map key, and JSON value reachable from v, which
// must be settable, except where a struct field's mask tag says otherwise:
// see Plan. It reaches a field added to a document type without a change
// here.
func (s *scan) walk(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(s.str(v.String()))
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		v.Set(reflect.ValueOf(s.tree(v.Interface())))
	case reflect.Pointer:
		if !v.IsNil() {
			s.walk(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Field(i)
			switch tag := v.Type().Field(i).Tag.Get("mask"); tag {
			case "":
				s.walk(f)
			case "keep":
			case "keys":
				s.walkMap(f, false)
			case "address":
				f.SetString(s.address(f.String()))
			case "index":
				if !f.IsNil() {
					f.Set(reflect.ValueOf(s.index(f.Interface())))
				}
			case "paths":
				for _, path := range f.Interface().([][]any) {
					s.path(path)
				}
			default:
				panic("mask: unknown tag " + tag)
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			s.walk(v.Index(i))
		}
	case reflect.Map:
		s.walkMap(v, true)
	}
}

func (s *scan) walkMap(v reflect.Value, keys bool) {
	if v.IsNil() {
		return
	}
	out := reflect.MakeMapWithSize(v.Type(), v.Len())
	for it := v.MapRange(); it.Next(); {
		k := reflect.New(v.Type().Key()).Elem()
		k.Set(it.Key())
		if keys {
			s.walk(k)
		}
		e := reflect.New(v.Type().Elem()).Elem()
		e.Set(it.Value())
		s.walk(e)
		out.SetMapIndex(k, e)
	}
	v.Set(out)
}

// address sweeps the index keys of a resource or module address, such as
// "k" in module.m["k"].x.y[0], which a value can set, and keeps the names,
// which the configuration sets. It parses the address with HCL's traversal
// parser, as OpenTofu's addrs.ParseAbsResourceInstanceStr does, sweeps each
// key as the value it decodes to, and writes the address back as OpenTofu
// writes it, a swept key as ["(sensitive)"]. An address that does not parse
// is swept whole.
func (s *scan) address(x string) string {
	steps, diags := hclsyntax.ParseTraversalAbs([]byte(x), "", hcl.InitialPos)
	if diags.HasErrors() {
		return s.str(x)
	}
	var out strings.Builder
	for _, step := range steps {
		switch step := step.(type) {
		case hcl.TraverseRoot:
			out.WriteString(step.Name)
		case hcl.TraverseAttr:
			out.WriteString("." + step.Name)
		case hcl.TraverseIndex:
			key, text, ok := instanceKey(step.Key)
			if !ok {
				return s.str(x)
			}
			if s.str(key) != key {
				text = `["` + Sensitive + `"]`
			}
			out.WriteString(text)
		default:
			return s.str(x)
		}
	}
	return out.String()
}

// instanceKey returns the value of an index key as text, and the key as
// OpenTofu writes it in an address: a string quoted by hclwrite, whose
// escapes match OpenTofu's toHCLQuotedString, or an integer. It reports
// false for any other key, which OpenTofu does not write.
func instanceKey(v cty.Value) (string, string, bool) {
	switch v.Type() {
	case cty.String:
		return v.AsString(), "[" + string(hclwrite.TokensForValue(v).Bytes()) + "]", true
	case cty.Number:
		n, acc := v.AsBigFloat().Int64()
		if acc != big.Exact {
			return "", "", false
		}
		key := strconv.FormatInt(n, 10)
		return key, "[" + key + "]", true
	}
	return "", "", false
}

// index sweeps the index of a resource change, the key its address
// carries, as address sweeps that key: whole.
func (s *scan) index(v any) any {
	switch k := v.(type) {
	case string:
		if s.str(k) != k {
			return Sensitive
		}
	case json.Number:
		if s.str(k.String()) != k.String() {
			return Sensitive
		}
	default:
		return s.tree(v)
	}
	return v
}

// path sweeps one path of an attribute, such as a replace path, but its
// first step, which names an attribute of the resource's schema. A later
// string step can be a map key, which a value sets, and is swept.
func (s *scan) path(p []any) {
	for i, step := range p {
		if _, ok := step.(string); ok && i == 0 {
			continue
		}
		p[i] = s.tree(step)
	}
}
