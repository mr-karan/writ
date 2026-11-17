package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// Bounds on what a bulk import reads before it starts a provider.
const (
	// MaxSelectionBytes bounds a selection file.
	MaxSelectionBytes = 4 << 20
	// MaxIDBytes bounds one import id.
	MaxIDBytes = provider.MaxImportIDBytes
	// MaxTemplateBytes bounds an id template. Its literal text alone may
	// not pass MaxIDBytes, so a longer template is a mistake.
	MaxTemplateBytes = MaxIDBytes
	// ExpansionsPerObject bounds the id templates a run expands: at most
	// this many times its object limit, counting the expansions skipped
	// for a missing value.
	ExpansionsPerObject = 10
)

// Selection picks objects of one type for a bulk import, in exactly one of
// four ways: IDs, Values with ID, From with ID, or List.
type Selection struct {
	Type types.Name
	// Provider is the provider configuration to read through, zero for the
	// use block's.
	Provider addr.Addr
	IDs      []string
	// Values are records whose fields ID reads.
	Values []map[string]value.Value
	// From names a type an earlier selection read. ID reads each object of
	// it as parent and, with Over, each element of the parent's Over
	// attribute as item.
	From types.Name
	Over value.Path
	ID   *Template
	// List is the query a provider lists objects by, or null.
	List value.Value
}

// selectionFile is the JSON form of a selection file.
type selectionFile struct {
	Objects []selectionEntry `json:"objects"`
}

type selectionEntry struct {
	Type     string           `json:"type"`
	Provider string           `json:"provider"`
	IDs      []string         `json:"ids"`
	Values   []map[string]any `json:"values"`
	From     string           `json:"from"`
	Over     string           `json:"over"`
	ID       string           `json:"id"`
	List     map[string]any   `json:"list"`
}

// ParseSelectionsOf reads a selection file whose types typeOf reads, such
// as an OpenTofu stack's random_uuid. Every problem is an error that names
// the entry, before any provider starts.
func ParseSelectionsOf(r io.Reader, typeOf func(string) (types.Name, error)) ([]Selection, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxSelectionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSelectionBytes {
		return nil, fmt.Errorf("the selection is larger than %d bytes", MaxSelectionBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var f selectionFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("the selection is not valid JSON of the form {\"objects\": [...]}: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the selection holds more than one JSON value")
	}
	if len(f.Objects) == 0 {
		return nil, errors.New("the selection lists no objects")
	}
	out := make([]Selection, 0, len(f.Objects))
	seen := map[types.Name]bool{}
	for i, e := range f.Objects {
		s, err := e.parse(seen, typeOf)
		if err != nil {
			return nil, fmt.Errorf("objects[%d]: %w", i, err)
		}
		seen[s.Type] = true
		out = append(out, s)
	}
	return out, nil
}

func (e selectionEntry) parse(earlier map[types.Name]bool, typeOf func(string) (types.Name, error)) (Selection, error) {
	var s Selection
	var err error
	if s.Type, err = typeOf(e.Type); err != nil {
		return s, err
	}
	if e.Provider != "" {
		if s.Provider, err = addr.Parse(e.Provider); err != nil {
			return s, fmt.Errorf("provider %q: %w", e.Provider, err)
		}
	}
	ways := 0
	for _, set := range []bool{e.IDs != nil, e.Values != nil, e.From != "", e.List != nil} {
		if set {
			ways++
		}
	}
	if ways != 1 {
		return s, errors.New("give exactly one of ids, values, from, or list")
	}
	if (e.ID != "") != (e.Values != nil || e.From != "") {
		return s, errors.New("id goes with values or from, and they need it")
	}
	if e.Over != "" && e.From == "" {
		return s, errors.New("over goes with from")
	}
	switch {
	case e.IDs != nil:
		if len(e.IDs) == 0 {
			return s, errors.New("ids is empty")
		}
		for _, id := range e.IDs {
			if err := CheckID(id); err != nil {
				return s, err
			}
		}
		s.IDs = e.IDs
	case e.List != nil:
		q, err := jsonValue(e.List)
		if err != nil {
			return s, fmt.Errorf("list: %w", err)
		}
		s.List = q
	default:
		if s.ID, err = ParseTemplate(e.ID); err != nil {
			return s, fmt.Errorf("id: %w", err)
		}
	}
	if e.Values != nil {
		if len(e.Values) == 0 {
			return s, errors.New("values is empty")
		}
		for j, rec := range e.Values {
			fields := map[string]value.Value{}
			for k, raw := range rec {
				v, err := jsonScalar(raw)
				if err != nil {
					return s, fmt.Errorf("values[%d].%s: %w", j, k, err)
				}
				fields[k] = v
			}
			s.Values = append(s.Values, fields)
		}
		if err := s.ID.only(); err != nil {
			return s, fmt.Errorf("id: %w", err)
		}
	}
	if e.From != "" {
		if s.From, err = typeOf(e.From); err != nil {
			return s, fmt.Errorf("from: %w", err)
		}
		if !earlier[s.From] {
			return s, fmt.Errorf("from names %s, which no earlier entry selects", s.From)
		}
		roots := []string{"parent"}
		if e.Over != "" {
			for _, part := range strings.Split(e.Over, ".") {
				if !addr.IsIdent(part) {
					return s, fmt.Errorf("over %q: want an attribute path, such as route", e.Over)
				}
				s.Over = s.Over.Attr(part)
			}
			roots = append(roots, "item")
		}
		if err := s.ID.only(roots...); err != nil {
			return s, fmt.Errorf("id: %w", err)
		}
	}
	return s, nil
}

// CheckID refuses an import id that no provider takes: an empty one, or one
// longer than MaxIDBytes. It never prints the id, which may be a secret.
func CheckID(id string) error {
	switch {
	case id == "":
		return errors.New("an import id is empty")
	case len(id) > MaxIDBytes:
		return fmt.Errorf("an import id is %d bytes long, more than the limit of %d", len(id), MaxIDBytes)
	}
	return nil
}

func jsonScalar(raw any) (value.Value, error) {
	switch v := raw.(type) {
	case string:
		return value.StringVal(v), nil
	case json.Number:
		return jsonNumber(v)
	case bool:
		return value.BoolVal(v), nil
	}
	return value.Value{}, errors.New("want a string, a number, or a boolean")
}

func jsonNumber(n json.Number) (value.Value, error) {
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return value.IntVal(i), nil
	}
	f, err := n.Float64()
	if err != nil {
		return value.Value{}, err
	}
	return value.FloatVal(f), nil
}

// jsonValue converts decoded JSON, numbers as json.Number, to a value.
func jsonValue(raw any) (value.Value, error) {
	switch v := raw.(type) {
	case nil:
		return value.NullVal(), nil
	case []any:
		elems := make([]value.Value, len(v))
		for i, e := range v {
			ev, err := jsonValue(e)
			if err != nil {
				return value.Value{}, err
			}
			elems[i] = ev
		}
		return value.ListVal(elems), nil
	case map[string]any:
		fields := make(map[string]value.Value, len(v))
		for k, e := range v {
			ev, err := jsonValue(e)
			if err != nil {
				return value.Value{}, err
			}
			fields[k] = ev
		}
		return value.ObjectVal(fields), nil
	}
	return jsonScalar(raw)
}

// Template builds an import id from values: literal text with placeholders
// such as {b} or {parent.id}. {{ and }} write one brace.
type Template struct {
	parts []templatePart
}

type templatePart struct {
	text string
	path []string // a placeholder when set
}

// ParseTemplate reads an id template.
func ParseTemplate(s string) (*Template, error) {
	if len(s) > MaxTemplateBytes {
		return nil, fmt.Errorf("the template is %d bytes long, more than the limit of %d", len(s), MaxTemplateBytes)
	}
	t := &Template{}
	var lit strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '{' && i+1 < len(s) && s[i+1] == '{', c == '}' && i+1 < len(s) && s[i+1] == '}':
			lit.WriteByte(c)
			i++
		case c == '}':
			return nil, errors.New("a } with no {; write }} for a brace")
		case c == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, errors.New("a { with no }; write {{ for a brace")
			}
			name := s[i+1 : i+end]
			path := strings.Split(name, ".")
			for _, p := range path {
				if !addr.IsIdent(p) {
					return nil, fmt.Errorf("placeholder {%s}: want a name or a dotted path, such as {parent.id}", name)
				}
			}
			if lit.Len() > 0 {
				t.parts = append(t.parts, templatePart{text: lit.String()})
				lit.Reset()
			}
			t.parts = append(t.parts, templatePart{path: path})
			i += end
		default:
			lit.WriteByte(c)
		}
	}
	if lit.Len() > 0 {
		t.parts = append(t.parts, templatePart{text: lit.String()})
	}
	if len(t.parts) == 0 {
		return nil, errors.New("the template is empty")
	}
	return t, nil
}

// only checks that every placeholder starts with one of roots. With no
// roots, a placeholder is one field name.
func (t *Template) only(roots ...string) error {
	for _, p := range t.parts {
		switch {
		case p.path == nil:
		case len(roots) == 0 && len(p.path) != 1:
			return fmt.Errorf("placeholder {%s}: a values entry has fields, not paths", strings.Join(p.path, "."))
		case len(roots) > 0 && (!slices.Contains(roots, p.path[0]) || len(p.path) < 2):
			return fmt.Errorf("placeholder {%s}: want {%s.ATTR}", strings.Join(p.path, "."), strings.Join(roots, ".ATTR} or {"))
		}
	}
	return nil
}

// errSkip means a placeholder found no value, so this id is skipped.
type errSkip struct{ placeholder string }

func (e *errSkip) Error() string {
	return fmt.Sprintf("{%s} has no value", e.placeholder)
}

// errSecretID means a placeholder found a secret.
var errSecretID = errors.New("a placeholder reads a secret, and an import id is not secret")

// errLongID means an expansion passed MaxIDBytes. It stops there, so a
// long template of long values never builds a long string.
var errLongID = fmt.Errorf("the template builds an import id longer than the limit of %d bytes", MaxIDBytes)

// Expand fills the template from vars, a record. A placeholder whose value
// is null or missing gives an *errSkip; one whose value is a secret gives
// errSecretID; an id longer than MaxIDBytes gives errLongID.
func (t *Template) Expand(vars value.Value) (string, error) {
	var b strings.Builder
	write := func(s string) error {
		if b.Len()+len(s) > MaxIDBytes {
			return errLongID
		}
		b.WriteString(s)
		return nil
	}
	for _, p := range t.parts {
		if p.path == nil {
			if err := write(p.text); err != nil {
				return "", err
			}
			continue
		}
		v := vars
		for _, step := range p.path {
			if v.IsSecret() {
				return "", errSecretID
			}
			if v.Kind() != value.Object {
				v = value.NullVal()
				break
			}
			v = v.Get(step)
		}
		if v.IsSecret() || v.HasSecret() {
			return "", errSecretID
		}
		var piece string
		switch v.Kind() {
		case value.String:
			piece = v.AsString()
		case value.Int:
			piece = strconv.FormatInt(v.AsInt(), 10)
		case value.Float:
			piece = value.FormatFloat(v.AsFloat())
		case value.Bool:
			piece = strconv.FormatBool(v.AsBool())
		case value.Null:
			return "", &errSkip{placeholder: strings.Join(p.path, ".")}
		default:
			return "", fmt.Errorf("{%s} is a %s, and an id is built from strings and numbers", strings.Join(p.path, "."), v.Kind())
		}
		if err := write(piece); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}
