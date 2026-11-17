// Package mask produces every document that leaves an OpenTofu run: the
// masked plan document, the rows of the state index, and the run's event
// streams with secrets swept out. One Mask serves one run and learns its
// secrets from the environment the run was given and from each document it
// masks.
//
// The mask is a filter on output, not a property of the program. Three
// cases stay open:
//
//   - A secret in a variable that nobody declared sensitive, written to an
//     attribute that the provider does not mark.
//   - A secret that the code transforms and then passes to nonsensitive(),
//     such as nonsensitive(sha256(var.NAME)).
//   - A provider that returns a secret in an attribute it does not mark,
//     known only after apply.
//
// The sweep protects a bool, and a number shorter than minNumber
// characters, only where a mark covers it. Sweeping such a value would
// blank every equal value in the document.
//
// Two limits fail closed: the run then stores no document. A run may
// collect at most 4 MiB of secrets, which bounds the matcher the sweep
// builds. One string may cost at most 32 MiB of decoding and matching,
// which bounds the work of undoing nested encodings.
package mask

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// Sensitive is the text that replaces every value the mask removes.
const Sensitive = "(sensitive)"

// Mask removes one run's secrets from what the run outputs.
type Mask struct {
	secrets map[string]struct{}
	sweep   *sweeper
}

// New returns a Mask that knows the secrets writ serve put into the run's
// environment.
func New(env []string) *Mask {
	m := &Mask{secrets: map[string]struct{}{}}
	for _, s := range env {
		m.add(s)
	}
	return m
}

// Secrets returns every secret the Mask knows, sorted: those it was made
// with and those it has learned.
func (m *Mask) Secrets() []string {
	return slices.Sorted(maps.Keys(m.secrets))
}

func (m *Mask) add(s string) {
	if s == "" {
		return
	}
	if _, ok := m.secrets[s]; !ok {
		m.secrets[s] = struct{}{}
		m.sweep = nil
	}
}

// collect adds every string leaf, every number of minNumber or more
// characters, and every map key of v. A number is added as the text
// OpenTofu writes for it, which is also what tostring() gives. A bool, or
// a shorter number, is masked only where a mark covers it.
func (m *Mask) collect(v any) {
	switch v := v.(type) {
	case string:
		m.add(v)
	case json.Number:
		if len(v) >= minNumber {
			m.add(v.String())
		}
	case map[string]any:
		for k, x := range v {
			m.add(k)
			m.collect(x)
		}
	case []any:
		for _, x := range v {
			m.collect(x)
		}
	}
}

// scan returns a sweep of one document with the secrets the Mask has
// learned so far.
func (m *Mask) scan() (*scan, error) {
	if m.sweep == nil {
		sw, err := newSweeper(m.secrets)
		if err != nil {
			return nil, err
		}
		m.sweep = sw
	}
	return &scan{sweeper: m.sweep}, nil
}

// Event sweeps one line of a `tofu plan -json` or `tofu apply -json` stream
// with the secrets the Mask has learned so far, so a run masks its plan
// before its events. Values under a secret name are masked first. A line
// that is not a JSON object is swept as text. A line the mask cannot sweep
// becomes Sensitive.
func (m *Mask) Event(line []byte) []byte {
	s, err := m.scan()
	if err != nil {
		return []byte(Sensitive)
	}
	var v map[string]any
	if err := decode(line, &v); err != nil {
		return []byte(s.str(string(line)))
	}
	swept := s.tree(byName(v))
	if s.err != nil {
		return []byte(Sensitive)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(swept); err != nil {
		return []byte(Sensitive)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// Sweep replaces every secret the Mask has learned in the strings, map
// keys, and JSON values that ptr, a non-nil pointer, reaches, as Plan
// sweeps its document. Text built from a masked document can still hold a
// secret that a raw document gave it, such as an attribute name. On an
// error *ptr must not leave the run.
func (m *Mask) Sweep(ptr any) error {
	s, err := m.scan()
	if err != nil {
		return err
	}
	s.walk(reflect.ValueOf(ptr).Elem())
	return s.err
}

// redact replaces every value of v under a true mark with Sensitive and
// collects what it removes. A mark is true, false, absent, or a tree of the
// value's shape; any other mark is an error. A mark may name a key the
// value lacks only where unknown, the value's after_unknown tree, marks
// that key unknown.
func (m *Mask) redact(v, mark, unknown any) (any, error) {
	switch mk := mark.(type) {
	case nil:
		return v, nil
	case bool:
		if !mk || v == nil {
			return v, nil
		}
		m.collect(v)
		return Sensitive, nil
	case map[string]any:
		if v == nil {
			return nil, checkMark(mk)
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("an object mark on a value that is not an object")
		}
		un, _ := unknown.(map[string]any)
		for k, sub := range mk {
			if _, ok := obj[k]; ok {
				continue
			}
			if un[k] != true {
				return nil, errors.New("a mark on a key the value does not hold")
			}
			if err := checkMark(sub); err != nil {
				return nil, err
			}
		}
		for k, x := range obj {
			r, err := m.redact(x, mk[k], un[k])
			if err != nil {
				return nil, err
			}
			obj[k] = r
		}
		return obj, nil
	case []any:
		if v == nil {
			return nil, checkMark(mk)
		}
		list, ok := v.([]any)
		if !ok {
			return nil, errors.New("a list mark on a value that is not a list")
		}
		if len(mk) > len(list) {
			return nil, errors.New("a list mark longer than its value")
		}
		un, _ := unknown.([]any)
		for i, x := range list {
			r, err := m.redact(x, at(mk, i), at(un, i))
			if err != nil {
				return nil, err
			}
			list[i] = r
		}
		return list, nil
	default:
		return nil, fmt.Errorf("a mark of type %T", mark)
	}
}

// checkMark checks a mark tree that has no value to fit.
func checkMark(mark any) error {
	switch mk := mark.(type) {
	case nil, bool:
		return nil
	case map[string]any:
		for _, sub := range mk {
			if err := checkMark(sub); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, sub := range mk {
			if err := checkMark(sub); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("a mark of type %T", mark)
	}
}

func at(list []any, i int) any {
	if i < len(list) {
		return list[i]
	}
	return nil
}

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		// encoding/json names the field path of a type error, and a path
		// can hold a map key, such as an output's name, that is a secret
		// the mask has not learned yet. So the error quotes nothing.
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return fmt.Errorf("the document is not valid JSON at byte %d", syn.Offset)
		}
		return errors.New("the document does not have the shape of tofu show -json output")
	}
	var extra json.RawMessage
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("trailing data after the JSON document")
	}
	return nil
}

// checkFormat refuses a document whose format_version major the mask was
// not tested with.
func checkFormat(v string) error {
	major, _, _ := strings.Cut(v, ".")
	if major != "1" {
		// The text is not quoted: it is read before the mask learns the
		// document's secrets.
		return errors.New("format_version: the mask is tested with major version 1 only")
	}
	return nil
}
