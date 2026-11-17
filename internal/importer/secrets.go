package importer

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// minKnownSecret is the length of the shortest secret of another owner
// that an import refuses an equal value for. A shorter value, such as 0,
// 443, or "us", appears by coincidence in almost every object, so matching
// it would refuse almost every import, and so would the text of a
// Secret<Bool>. The limit applies only to a comparison with the secret of
// another owner: an object's own secret is tracked at any length, so an
// import id equal to it is never printed.
const minKnownSecret = 4

// minContainedSecret is the length, in bytes, of the shortest secret of
// another owner that an import refuses a value for when the value contains
// it, and of the shortest value that an import refuses when a secret
// contains it. A short string is part of many longer ones by coincidence.
// Both lengths count the bytes of a text other than line breaks. A secret
// that is shorter without its line breaks, and has at least this many
// bytes with them, is refused inside a value that holds it as it is; see
// leaks.
const minContainedSecret = 8

// SecretText is the text an import compares a secret scalar by: a string
// as it is, and a number or a bool as Writ prints it, so the secret
// 12345678 matches both the number 12345678 and the string "12345678".
func SecretText(v value.Value) (string, bool) {
	switch v.Kind() {
	case value.String:
		return v.AsString(), true
	case value.Int:
		return strconv.FormatInt(v.AsInt(), 10), true
	case value.Float:
		return value.FormatFloat(v.AsFloat()), true
	case value.Bool:
		return strconv.FormatBool(v.AsBool()), true
	}
	return "", false
}

// matchable reports whether an import refuses a value equal to the secret
// text s of another owner; see minKnownSecret.
func matchable(s string) bool {
	return len(s) >= minKnownSecret && s != "true" && s != "false"
}

// Known is what an import knows to be secret before it reads an object.
// Writ never writes one of these where it is not secret, so every import
// command refuses an object that would write one in its code, in state that
// is not sealed, or in its output.
type Known struct {
	// Secrets are the secret scalars the stack's code holds: each leaf of
	// a value that is secret, and each key of a map in one. Each is marked
	// secret.
	Secrets []value.Value
}

// set collects every secret k holds, owned by no object of the run.
func (k Known) set() *secretSet {
	s := newSecretSet()
	for _, v := range k.Secrets {
		s.addValue(nil, v)
	}
	return s
}

// maxSecretBytes is the most bytes of distinct secrets, each of at least
// minContainedSecret bytes, that one import compares values against. The
// index of them takes about 5 bytes of memory for each byte of secret, so
// the limit keeps the index under about 320 MiB. An import that knows more
// is refused before it builds the index. The key text of the private keys
// in those secrets is never longer than the secrets, so its index is never
// larger.
const maxSecretBytes = 64 << 20

// secretSet holds every secret a run knows by its text, with the objects
// that keep it: each value in an attribute the provider schema marks
// sensitive, or marked secret, and each key of a map in one, of every object
// the run read and every row in state; each secret of Known.Secrets; and
// each secret output of another stack. See leaks for the values it refuses.
//
// Call ready after the last addState or addValue and before the first
// check. It builds the indexes and refuses a set over maxSecretBytes.
type secretSet struct {
	// byText lists, for each secret at any length, the objects that keep
	// it; nil stands for the stack, and for an object the stack already
	// manages.
	byText map[string][]*node
	// forms lists, for each form of at least minContainedSecret bytes,
	// the distinct secrets with that form. A secret with line breaks
	// whose form is shorter has at least minContainedSecret bytes as it
	// is, so it is listed by itself.
	forms map[string][]string
	// byPrefix lists each such form by its first minContainedSecret
	// bytes, to find it inside the form of a value, or inside the value.
	byPrefix map[string][]string
	// long lists each form of at least minContainedSecret bytes once,
	// and longBytes is the sum of their lengths.
	long      []string
	longBytes int
	// index finds a form inside a form of long. It is nil until ready or
	// a lookup builds it, and after keep adds a form to long.
	index *textIndex
	// keys lists each private key that a secret holds by its key text,
	// and keyBodies lists each key text once.
	keys      map[string]*privateKey
	keyBodies []string
	// keysOf lists the private keys each secret holds.
	keysOf map[string][]*privateKey
	// keyIndex finds a key text inside the key text of a private key. It
	// is nil until ready or a lookup builds it, and after keep adds a key.
	keyIndex *textIndex
	// certs are the PEM certificates that the values of the run and of
	// state hold, each once.
	certs    []*certificate
	certSeen map[string]bool
	// texts are every secret string, and every other matchable secret, for
	// the redactor.
	texts []string
}

func newSecretSet() *secretSet {
	return &secretSet{
		byText: map[string][]*node{}, forms: map[string][]string{},
		byPrefix: map[string][]string{}, keys: map[string]*privateKey{},
		keysOf: map[string][]*privateKey{}, certSeen: map[string]bool{},
	}
}

// form returns s without the bytes \r and \n.
func form(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func (s *secretSet) keep(owner *node, text string, isString bool) {
	if text == "" {
		return
	}
	if _, ok := s.byText[text]; !ok {
		f := form(text)
		switch {
		case len(f) >= minContainedSecret:
			if _, seen := s.forms[f]; !seen {
				s.byPrefix[f[:minContainedSecret]] = append(s.byPrefix[f[:minContainedSecret]], f)
				s.long = append(s.long, f)
				s.longBytes += len(f)
				s.index = nil
			}
			s.forms[f] = append(s.forms[f], text)
			s.keepKeys(text)
		case len(text) >= minContainedSecret:
			s.byPrefix[text[:minContainedSecret]] = append(s.byPrefix[text[:minContainedSecret]], text)
			s.forms[text] = []string{text}
		}
	}
	s.byText[text] = append(s.byText[text], owner)
	if isString || matchable(text) {
		s.texts = append(s.texts, text)
	}
}

// keepKeys keeps each PEM private key in the secret text whose key text
// has at least minKeyText characters.
func (s *secretSet) keepKeys(text string) {
	if !strings.Contains(text, "PRIVATE KEY-----") {
		return
	}
	for _, b := range pemBlocks(text, privateLabel) {
		if len(b.body) < minKeyText {
			continue
		}
		pk, ok := s.keys[b.body]
		if !ok {
			pk = &privateKey{label: b.label, body: b.body}
			s.keys[b.body] = pk
			s.keyBodies = append(s.keyBodies, b.body)
			s.keyIndex = nil
		}
		pk.secrets = append(pk.secrets, text)
		s.keysOf[text] = append(s.keysOf[text], pk)
	}
}

// keepCerts keeps each PEM certificate in x, a string of a value of the
// run or of state, secret or not. A certificate that vouches for a private
// key makes its parts of 8 to 15 bytes public; see publicPart.
func (s *secretSet) keepCerts(x string) {
	if !strings.Contains(x, "-----BEGIN CERTIFICATE-----") {
		return
	}
	for _, b := range pemBlocks(x, certificateLabel) {
		if s.certSeen[b.text] {
			continue
		}
		s.certSeen[b.text] = true
		s.certs = append(s.certs, &certificate{block: b})
	}
}

// ready builds the indexes. It refuses a set whose long secrets hold more
// than maxSecretBytes, with a *LimitError, before it allocates an index.
func (s *secretSet) ready() error {
	if s.longBytes > maxSecretBytes {
		return &LimitError{What: "secret bytes", Limit: maxSecretBytes, Got: s.longBytes}
	}
	s.secretIndex()
	s.privateKeyIndex()
	return nil
}

// secretIndex returns the index of the forms of the long secrets, and
// builds it first when it is nil.
func (s *secretSet) secretIndex() *textIndex {
	if s.index == nil {
		s.index = newTextIndex(s.long)
	}
	return s.index
}

// privateKeyIndex returns the index of the key texts of the private keys,
// and builds it first when it is nil.
func (s *secretSet) privateKeyIndex() *textIndex {
	if s.keyIndex == nil {
		s.keyIndex = newTextIndex(s.keyBodies)
	}
	return s.keyIndex
}

// redactor removes every secret the set holds from free text that leaves
// the run, such as a provider's refusal.
func (s *secretSet) redactor() value.Redactor {
	vals := make([]value.Value, len(s.texts))
	for i, x := range s.texts {
		vals[i] = value.StringVal(x).Secret()
	}
	return value.Redactor{}.With(vals...)
}

// addValue keeps each secret in v, a value whose type the set does not
// know: each scalar that is secret, and each key of an object that is, a
// record's field names among them.
func (s *secretSet) addValue(owner *node, v value.Value) {
	var walk func(v value.Value, secret bool)
	walk = func(v value.Value, secret bool) {
		secret = secret || v.IsSecret()
		switch v.Kind() {
		case value.List:
			for i := range v.Len() {
				walk(v.Index(i), secret)
			}
		case value.Object:
			for _, k := range v.Keys() {
				if secret {
					s.keep(owner, k, true)
				}
				walk(v.Get(k), secret)
			}
		case value.String:
			s.keepCerts(v.AsString())
			if secret {
				s.keep(owner, v.AsString(), true)
			}
		default:
			if x, ok := SecretText(v); ok && secret {
				s.keep(owner, x, false)
			}
		}
	}
	walk(v, false)
}

// addState keeps each secret in v, a value of blk: each scalar in a value
// that is secret or in an attribute blk marks sensitive, and each key of a
// map in one. Record fields and attributes are named by the schema, so
// they are not values and are not kept.
func (s *secretSet) addState(owner *node, blk schema.Block, v value.Value) {
	if v.IsSecret() {
		s.addValue(owner, v)
		return
	}
	if v.Kind() != value.Object {
		return
	}
	var walk func(v value.Value, t types.Type, secret bool)
	walk = func(v value.Value, t types.Type, secret bool) {
		secret = secret || v.IsSecret() || t.IsSecret()
		switch v.Kind() {
		case value.List:
			for i := range v.Len() {
				walk(v.Index(i), t.Elem(), secret)
			}
		case value.Object:
			for _, k := range v.Keys() {
				if t.Kind() == types.Record {
					f, _ := t.Field(k)
					walk(v.Get(k), f.Type, secret)
					continue
				}
				if secret {
					s.keep(owner, k, true)
				}
				walk(v.Get(k), t.Elem(), secret)
			}
		case value.String:
			s.keepCerts(v.AsString())
			if secret {
				s.keep(owner, v.AsString(), true)
			}
		default:
			if x, ok := SecretText(v); ok && secret {
				s.keep(owner, x, false)
			}
		}
	}
	for _, a := range blk.Attributes {
		walk(v.Get(a.Name), a.Type, a.Sensitive)
	}
	for _, nb := range blk.Blocks {
		bv := v.Get(nb.Name)
		switch {
		case bv.Kind() == value.Null:
		case nb.IsRecord():
			s.addState(owner, nb.Block, bv)
		case nb.Nesting == schema.NestMap:
			for _, k := range bv.Keys() {
				if bv.IsSecret() {
					s.keep(owner, k, true)
				}
				s.addState(owner, nb.Block, bv.Get(k))
			}
		default:
			for i := range bv.Len() {
				s.addState(owner, nb.Block, bv.Index(i))
			}
		}
	}
}

// owners reports whether x is a secret self keeps, at any length, and
// whether it is a matchable secret that any other owner keeps. A nil self
// is no owner.
func (s *secretSet) owners(x string, self *node) (own, other bool) {
	own, other = s.ownedBy(x, self)
	return own, other && matchable(x)
}

// ownedBy reports whether self keeps the secret sec, and whether another
// owner does. A nil self is no owner.
func (s *secretSet) ownedBy(sec string, self *node) (own, other bool) {
	for _, o := range s.byText[sec] {
		if self != nil && o == self {
			own = true
		} else {
			other = true
		}
	}
	return own, other
}

// holding calls yield with each secret of at least minContainedSecret
// bytes that x holds, until yield returns true: a secret whose form is
// inside the form of x, and a secret with line breaks whose form is
// shorter and that is inside x as it is. It reports whether yield returned
// true.
func (s *secretSet) holding(x string, yield func(secret string) bool) bool {
	scan := func(t string) bool {
		for i := 0; i+minContainedSecret <= len(t); i++ {
			for _, f := range s.byPrefix[t[i:i+minContainedSecret]] {
				if !strings.HasPrefix(t[i:], f) {
					continue
				}
				for _, sec := range s.forms[f] {
					if yield(sec) {
						return true
					}
				}
			}
		}
		return false
	}
	fx := form(x)
	return scan(fx) || fx != x && scan(x)
}

// around calls yield with each secret whose form holds fx, the form of a
// value, when fx has at least minContainedSecret bytes, until yield returns
// true. It can call yield more than once with one secret.
func (s *secretSet) around(fx string, yield func(secret string) bool) {
	if len(fx) < minContainedSecret {
		return
	}
	s.secretIndex().within(fx, func(f string) bool {
		for _, sec := range s.forms[f] {
			if yield(sec) {
				return true
			}
		}
		return false
	})
}

// contains reports whether x holds a secret of at least minContainedSecret
// bytes that self keeps, and one that another owner keeps; see holding. A
// nil self is no owner.
func (s *secretSet) contains(x string, self *node) (own, other bool) {
	s.holding(x, func(sec string) bool {
		o, t := s.ownedBy(sec, self)
		own, other = own || o, other || t
		return own && other
	})
	return own, other
}

// within reports whether the form of x, of at least minContainedSecret
// bytes, is part of the form of a secret that self keeps, and of one that
// another owner keeps. A nil self is no owner.
func (s *secretSet) within(x string, self *node) (own, other bool) {
	s.around(form(x), func(sec string) bool {
		o, t := s.ownedBy(sec, self)
		own, other = own || o, other || t
		return own && other
	})
	return own, other
}

// publicPart reports whether fx, the form of a value that is part of the
// secret sec, is also part of an encoding of the public key of a private
// key that sec holds; see publicForms. A part of fewer than minKeyText
// bytes is also public when it is part of a certificate that vouches for
// the key; see privateKey.vouches. Such a part can match the private key
// by coincidence, as an object identifier that both hold. A certificate
// makes no longer part public.
func (s *secretSet) publicPart(sec, fx string) bool {
	for _, k := range s.keysOf[sec] {
		k.derive(s.certs)
		if containsIn(k.public, fx) || len(fx) < minKeyText && containsIn(k.certTexts, fx) {
			return true
		}
	}
	return false
}

func containsIn(texts []string, x string) bool {
	for _, t := range texts {
		if strings.Contains(t, x) {
			return true
		}
	}
	return false
}

// keyLeaks reports whether the key text of x shares a run of the key text
// of a private key that an owner other than self keeps, when that run
// covers a byte of the key that is in no encoding of its public key; see
// privateKey.runPublic. It compares each run of minKeyText characters of
// the key text of x. A certificate makes no run public. A nil self is no
// owner.
func (s *secretSet) keyLeaks(x string, self *node) bool {
	if len(s.keyBodies) == 0 {
		return false
	}
	kx := keyText(x)
	if len(kx) < minKeyText {
		return false
	}
	ix := s.privateKeyIndex()
	leak := func(run string) bool {
		found := false
		ix.within(run, func(body string) bool {
			k := s.keys[body]
			k.derive(s.certs)
			if k.runPublic(run) {
				return false
			}
			for _, sec := range k.secrets {
				if _, t := s.ownedBy(sec, self); t {
					found = true
				}
			}
			return found
		})
		return found
	}
	for i := 0; i+minKeyText <= len(kx); i++ {
		if leak(kx[i : i+minKeyText]) {
			return true
		}
	}
	return false
}

// leaks reports whether x, written in the clear for self, would expose a
// secret of another owner. self is nil when every owner counts, as for the
// code of an object.
//
// x leaks when one of these is true:
//
//   - x equals a matchable secret.
//   - x holds a secret of at least minContainedSecret bytes: the form of
//     x holds its form, or x holds it as it is.
//   - The form of x has at least minContainedSecret bytes and is part of
//     the form of a secret. When the secret holds a private key, and the
//     form of x is also part of an encoding of the key's public key, or a
//     part of fewer than minKeyText bytes of a certificate that vouches
//     for the key, this rule does not refuse x; see publicPart.
//   - The key text of x shares a run with the key text of a private key
//     that a secret holds, and the run encodes a byte of the key that is
//     in no encoding of its public key; see keyLeaks.
//   - x holds a PEM certificate that holds bytes of a private number of a
//     private key that a secret holds; see certLeaks.
//
// The form of a text is the text without the bytes \r and \n. Equality
// compares texts as they are. The comparison is exact and case-sensitive,
// on bytes, PEM armor included. A text that is part of another, or that
// holds another, still is after both lose their line breaks, so the forms
// find each match that the texts as they are find, and also a part of a
// PEM body that spans a line break or that has CRLF line breaks.
//
// The public key of a private key, and a certificate for it, are the only
// public values that make a part of a secret public. The check derives the
// public key from the private key, so it needs no public value in state or
// in the run. A certificate counts only for a part of 8 to 15 bytes, when
// a value of state or of the run holds it, it parses, it holds that public
// key, and it holds no bytes of a private number of the key. A part of the
// private key that is in neither, and its armor lines, are refused.
func (s *secretSet) leaks(x string, self *node) bool {
	other := func(sec string) bool {
		_, t := s.ownedBy(sec, self)
		return t
	}
	if matchable(x) && other(x) {
		return true
	}
	if s.holding(x, other) {
		return true
	}
	fx := form(x)
	leak := false
	s.around(fx, func(sec string) bool {
		leak = other(sec) && !s.publicPart(sec, fx)
		return leak
	})
	return leak || s.keyLeaks(x, self) || s.certLeaks(x, self)
}

// certLeaks reports whether x holds a PEM certificate whose DER holds
// bytes of a private number of a private key that an owner other than
// self keeps; see privateKey.holdsSecret. It compares bytes, so it finds
// them at any offset in the certificate, also where the key text of the
// certificate and of the key differ. A nil self is no owner.
func (s *secretSet) certLeaks(x string, self *node) bool {
	if len(s.keyBodies) == 0 || !strings.Contains(x, "-----BEGIN CERTIFICATE-----") {
		return false
	}
	for _, b := range pemBlocks(x, certificateLabel) {
		der, err := base64.StdEncoding.DecodeString(b.body)
		if err != nil {
			continue
		}
		for _, body := range s.keyBodies {
			k := s.keys[body]
			k.derive(s.certs)
			if !k.holdsSecret(der) {
				continue
			}
			for _, sec := range k.secrets {
				if _, t := s.ownedBy(sec, self); t {
					return true
				}
			}
		}
	}
	return false
}

// idSecret reports whether the import id of self is a secret of its own:
// equal to one at any length, holding a long one, or long and part of one.
// Such an id is never printed.
func (s *secretSet) idSecret(self *node, id string) bool {
	own, _ := s.owners(id, self)
	inside, _ := s.contains(id, self)
	part, _ := s.within(id, self)
	return own || inside || part
}

// idLeaks reports whether the import id of self would expose a secret of
// another owner. An id that is a secret of self, as a password's is, is
// never printed, so it exposes no secret.
func (s *secretSet) idLeaks(self *node, id string) bool {
	return !s.idSecret(self, id) && s.leaks(id, self)
}

// inClear reports whether a value of blk that v writes in the clear, a
// scalar or a map key outside a secret, would expose a secret that an owner
// other than self keeps; see leaks. A nil self counts every owner.
// Attribute and field names come from the schema, so they are not values
// and are not checked.
func (s *secretSet) inClear(blk schema.Block, v value.Value, self *node) bool {
	found := false
	walkClear(blk, v, func(x string) {
		if !found && s.leaks(x, self) {
			found = true
		}
	})
	return found
}

// walkClear calls fn with the text of every scalar a block value holds
// outside a secret, and each key of a map or of a block nested as a map. It
// skips the names of attributes, blocks, and record fields.
func walkClear(blk schema.Block, v value.Value, fn func(x string)) {
	var walk func(v value.Value, t types.Type)
	walk = func(v value.Value, t types.Type) {
		if v.IsSecret() {
			return
		}
		switch v.Kind() {
		case value.List:
			for i := range v.Len() {
				walk(v.Index(i), t.Elem())
			}
		case value.Object:
			for _, k := range v.Keys() {
				if t.Kind() == types.Record {
					f, _ := t.Field(k)
					walk(v.Get(k), f.Type)
					continue
				}
				fn(k)
				walk(v.Get(k), t.Elem())
			}
		default:
			if x, ok := SecretText(v); ok {
				fn(x)
			}
		}
	}
	if v.IsSecret() || v.Kind() != value.Object {
		return
	}
	for _, a := range blk.Attributes {
		walk(v.Get(a.Name), a.Type)
	}
	for _, nb := range blk.Blocks {
		bv := v.Get(nb.Name)
		switch {
		case bv.IsSecret() || bv.Kind() == value.Null:
		case nb.IsRecord():
			walkClear(nb.Block, bv, fn)
		case nb.Nesting == schema.NestMap:
			for _, k := range bv.Keys() {
				fn(k)
				walkClear(nb.Block, bv.Get(k), fn)
			}
		default:
			for i := range bv.Len() {
				walkClear(nb.Block, bv.Index(i), fn)
			}
		}
	}
}
