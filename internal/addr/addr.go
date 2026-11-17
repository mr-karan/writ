// Package addr defines resource addresses: the path from a top-level let to a
// resource, such as ca_key, site.key, or files["a"].
package addr

import (
	"fmt"
	"strings"
)

// Addr is a resource address in canonical form. It is comparable, so it works
// as a map key, and it can only be built through Root, Field, Key, or Parse,
// so every Addr in memory is well formed.
type Addr struct {
	s string
}

// Root starts an address at a top-level let name. It panics when let is not
// an identifier: let names come from the parser, so that is a bug in Writ.
func Root(let string) Addr {
	if !IsIdent(let) {
		panic(fmt.Sprintf("addr: let name %q is not an identifier", let))
	}
	return Addr{s: let}
}

// Field extends a with a record field step: site.key. A record key written as
// a string that is not an identifier, such as "my-key", cannot follow a dot,
// so it is written as a key step: site["my-key"].
func (a Addr) Field(name string) Addr {
	a.mustBeSet("Field")
	if !IsIdent(name) {
		return a.Key(name)
	}
	return Addr{s: a.s + "." + name}
}

// Key extends a with a map key step: files["a"]. The key is quoted with the
// same escapes as Writ strings.
func (a Addr) Key(k string) Addr {
	a.mustBeSet("Key")
	return Addr{s: a.s + "[" + Quote(k) + "]"}
}

func (a Addr) mustBeSet(method string) {
	if a.IsZero() {
		panic("addr: " + method + " on the zero address")
	}
}

// Parse reads the canonical form a user types on the command line. It accepts
// exactly the strings String prints, so Parse(s).String() == s whenever Parse
// succeeds, and rejects everything else.
func Parse(s string) (Addr, error) {
	if s == "" {
		return Addr{}, fmt.Errorf("empty address")
	}
	p := parser{src: s}
	a, err := p.parse()
	if err != nil {
		return Addr{}, fmt.Errorf("invalid address %q: %w", s, err)
	}
	if a.s != s {
		return Addr{}, fmt.Errorf("address %q is not in canonical form, write %s", s, a.s)
	}
	return a, nil
}

// String returns the canonical form. Parse(a.String()) == a.
func (a Addr) String() string { return a.s }

// IsZero reports whether a is the zero address, which names nothing.
func (a Addr) IsZero() bool { return a.s == "" }

// Let returns the top-level let name the address starts with.
func (a Addr) Let() string {
	if i := strings.IndexAny(a.s, ".["); i >= 0 {
		return a.s[:i]
	}
	return a.s
}

// Within reports whether a is p or lies under p on complete address steps:
// files["a"] is within files, filesystem is not.
func (a Addr) Within(p Addr) bool {
	rest, ok := strings.CutPrefix(a.s, p.s)
	return ok && (rest == "" || rest[0] == '.' || rest[0] == '[')
}

// IsIdent reports whether s matches [A-Za-z_][A-Za-z0-9_]*.
func IsIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isIdentStart(c) && (i == 0 || !isDigit(c)) {
			return false
		}
	}
	return true
}

func isIdentStart(c byte) bool { return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }
func isDigit(c byte) bool      { return '0' <= c && c <= '9' }

// ShellWord returns s as one word of a POSIX shell command line, for a
// command a message tells the reader to run. A word of letters, digits, and
// `_ . - / = : , @ + %` is returned as it is. Anything else goes in single
// quotes, since addresses hold brackets and double quotes.
func ShellWord(s string) string {
	plain := s != ""
	for i := 0; i < len(s) && plain; i++ {
		c := s[i]
		plain = isIdentStart(c) || isDigit(c) || strings.IndexByte("._-/=:,@+%", c) >= 0
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Quote renders s as a Writ string literal. It escapes ", \, newline, and
// tab, and escapes $ only before {, where it would start an interpolation.
func Quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '$':
			if i+1 < len(s) && s[i+1] == '{' {
				b.WriteByte('\\')
			}
			b.WriteByte('$')
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// parser reads ident ( "." ident | "[" string "]" )*. It is lenient about
// which escapes a key uses; Parse then insists the result prints back as the
// input, which rejects every non-canonical spelling with one rule.
type parser struct {
	src string
	pos int
}

func (p *parser) parse() (Addr, error) {
	steps, err := p.steps()
	if err != nil {
		return Addr{}, err
	}
	a := Root(steps[0].text)
	for _, st := range steps[1:] {
		if st.key {
			a = a.Key(st.text)
		} else {
			a = a.Field(st.text)
		}
	}
	return a, nil
}

// step is one step of an address: the let name, a field, or a key.
type step struct {
	key  bool
	text string
}

func (p *parser) steps() ([]step, error) {
	let := p.ident()
	if let == "" {
		return nil, fmt.Errorf("an address starts with a let name, at column %d", p.pos+1)
	}
	out := []step{{text: let}}
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '.':
			p.pos++
			name := p.ident()
			if name == "" {
				return nil, fmt.Errorf("expected a field name after \".\", at column %d", p.pos+1)
			}
			out = append(out, step{text: name})
		case '[':
			p.pos++
			k, err := p.key()
			if err != nil {
				return nil, err
			}
			out = append(out, step{key: true, text: k})
		default:
			return nil, fmt.Errorf("unexpected %q at column %d, expected \".\" or \"[\"", p.src[p.pos], p.pos+1)
		}
	}
	return out, nil
}

func (p *parser) ident() string {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if !isIdentStart(c) && (p.pos == start || !isDigit(c)) {
			break
		}
		p.pos++
	}
	return p.src[start:p.pos]
}

// key reads a quoted key and the closing bracket; p.pos is just past "[".
func (p *parser) key() (string, error) {
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", fmt.Errorf("a key must be a quoted string, at column %d", p.pos+1)
	}
	p.pos++
	var b strings.Builder
	for {
		if p.pos >= len(p.src) {
			return "", fmt.Errorf("unterminated key")
		}
		c := p.src[p.pos]
		p.pos++
		if c == '"' {
			break
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if p.pos >= len(p.src) {
			return "", fmt.Errorf("unterminated key")
		}
		switch e := p.src[p.pos]; e {
		case '"', '\\', '$':
			b.WriteByte(e)
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		default:
			return "", fmt.Errorf("unknown escape \\%c at column %d", e, p.pos)
		}
		p.pos++
	}
	if p.pos >= len(p.src) || p.src[p.pos] != ']' {
		return "", fmt.Errorf("expected \"]\" after the key, at column %d", p.pos+1)
	}
	p.pos++
	return b.String(), nil
}
