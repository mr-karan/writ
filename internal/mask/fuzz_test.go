package mask

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// FuzzPlan feeds the mask arbitrary JSON values with arbitrary marks, as the
// before, after, and output of one change, beside a sensitive variable
// holding a canary that the value may repeat unmarked. Two more changes
// carry an index key in their addresses, as OpenTofu writes them: an
// arbitrary key, and the canary as a module's key. Whenever the mask
// accepts the plan, no scalar under a true mark and no canary may survive
// in any string, key, number, or bool of the masked document, plain or
// encoded, nor in the value of an address's key; and each address is
// either as OpenTofu wrote it or has its key swept.
func FuzzPlan(f *testing.F) {
	for _, name := range []string{"plan.json", "update-plan.json"} {
		var p struct {
			ResourceChanges []struct {
				Change struct {
					After          json.RawMessage `json:"after"`
					AfterSensitive json.RawMessage `json:"after_sensitive"`
				} `json:"change"`
			} `json:"resource_changes"`
		}
		if err := json.Unmarshal(fixture(f, name), &p); err != nil {
			f.Fatal(err)
		}
		for _, rc := range p.ResourceChanges {
			f.Add([]byte(rc.Change.After), []byte(rc.Change.AfterSensitive), pwFirst, "k")
		}
	}
	f.Add([]byte(`{"a":"k-canary-k","b":{"canary-key":"x"},"c":["Y2FuYXJ5LXZhbHVl"]}`), []byte(`{"b":true}`), "canary-value", "")
	f.Add([]byte(`["1","2",{"t":"1"}]`), []byte(`[false,true]`), "1", "")
	f.Add([]byte(`{"n":739201,"b":true,"s":"x","w":"dXNlcjpjYW5hcnktdmFsdWU="}`), []byte(`{"n":true,"b":true}`), "canary-value", "")
	f.Add([]byte(`{"n":739201,"t":"pin 739201","u":17392011}`), []byte(`{}`), "739201", "")
	f.Add([]byte(`{"a":true,"b":true,"p":80,"q":80,"r":"80"}`), []byte(`{"a":true,"p":true}`), "canary-value", "")
	f.Add([]byte(`{"a":"x %5cu0063anary%2dvalue","b":"63616E6172792D76616c7565","c":{"db_password":"q"}}`), []byte(`{"c":false}`), "canary-value", "")

	f.Add([]byte(`{"a":"a%252fb"}`), []byte(`false`), "a/b", "")
	f.Add([]byte(`{"a":"5c75303036315c75303032665c7530303632"}`), []byte(`false`), "a/b", "")
	f.Add([]byte(`{}`), []byte(`{}`), "ab${cd}", "ab${cd}")
	f.Add([]byte(`{}`), []byte(`{}`), "a${b}", "a%{b}")
	f.Add([]byte(`{}`), []byte(`{}`), "\U000e0001", "q\"b\\s\U000e0002")
	f.Fuzz(func(t *testing.T, after, marks []byte, canary, key string) {
		var v, mark any
		if unmarshal(after, &v) != nil || unmarshal(marks, &mark) != nil {
			return
		}
		raw, err := json.Marshal(map[string]any{
			"format_version": "1.2",
			"variables":      map[string]any{"pw": map[string]any{"value": canary}},
			"configuration":  map[string]any{"root_module": map[string]any{"variables": map[string]any{"pw": map[string]any{"sensitive": true}}}},
			"resource_changes": []any{map[string]any{
				"address": "x.y", "mode": "managed", "type": "x", "name": "y", "provider_name": "p",
				"change": map[string]any{"actions": []any{"update"}, "before": v, "after": v, "before_sensitive": mark, "after_sensitive": mark},
			}, map[string]any{
				"address": "x.z" + tofuKey(key), "mode": "managed", "type": "x", "name": "z", "index": key, "provider_name": "p",
				"change": map[string]any{"actions": []any{"create"}, "before": nil, "after": nil},
			}, map[string]any{
				"address": "module.m" + tofuKey(canary) + ".x.w", "module_address": "module.m" + tofuKey(canary), "mode": "managed", "type": "x", "name": "w", "provider_name": "p",
				"change": map[string]any{"actions": []any{"create"}, "before": nil, "after": nil},
			}},
			"output_changes": map[string]any{"o": map[string]any{"actions": []any{"create"}, "after": v, "after_sensitive": mark}},
		})
		if err != nil {
			t.Fatal(err)
		}
		p, err := New(nil).Plan(raw)
		if err != nil {
			return
		}
		got := p.ResourceChanges[0].Change.After
		if markedSurvives(v, got, mark) {
			t.Fatalf("a value under a true mark survives: %s", encode(t, got))
		}
		secret := roundTrip(t, canary)
		rc, o := &p.ResourceChanges[0], p.OutputChanges["o"]
		if p.FormatVersion != "1.2" || !slices.Equal(p.Variables, []string{"pw"}) || rc.Address != "x.y" || rc.Mode != "managed" || rc.Type != "x" || rc.Name != "y" ||
			rc.ProviderName != "p" || !slices.Equal(rc.Change.Actions, []string{"update"}) || !slices.Equal(o.Actions, []string{"create"}) {
			t.Fatalf("the sweep changed the structure of the plan: %s", encode(t, p))
		}
		// An address keeps OpenTofu's text or has its key swept, and its
		// index is swept with its key. HCL decodes a key to UTF-8 in NFC,
		// as OpenTofu keeps it.
		swept := `["` + Sensitive + `"]`
		rk := roundTrip(t, key)
		k, c := cty.StringVal(rk).AsString(), cty.StringVal(secret).AsString()
		z, w := &p.ResourceChanges[1], &p.ResourceChanges[2]
		switch {
		case z.Address == "x.z"+tofuKey(k) && z.Index == rk:
			z.Address = k
		case z.Address == "x.z"+swept && z.Index == Sensitive:
			z.Address = ""
		default:
			t.Fatalf("address %s index %v, want x.z%s or its key swept", z.Address, z.Index, tofuKey(k))
		}
		switch {
		case w.Address == "module.m"+tofuKey(c)+".x.w" && w.ModuleAddress == "module.m"+tofuKey(c):
			w.Address, w.ModuleAddress = c, ""
		case w.Address == "module.m"+swept+".x.w" && w.ModuleAddress == "module.m"+swept:
			w.Address, w.ModuleAddress = "", ""
		default:
			t.Fatalf("address %s module %s, want module.m%s.x.w or its key swept", w.Address, w.ModuleAddress, tofuKey(c))
		}
		// The structure is text of the configuration, which no value sets,
		// so the leak check reads the rest, with each key that an address
		// kept as its value.
		o.Actions = nil
		p.FormatVersion, p.Variables, p.OutputChanges = "", nil, map[string]Change{"": o}
		for i := range p.ResourceChanges {
			r := &p.ResourceChanges[i]
			r.Mode, r.Type, r.Name, r.ProviderName, r.Change.Actions = "", "", "", "", nil
		}
		rc.Address = ""
		var out []string
		stringsOf(reflect.ValueOf(p), &out)
		for _, s := range append(underTrue(v, mark), secret) {
			if leak := leaked(out, s); leak != "" {
				t.Fatalf("%q survives in %q", s, leak)
			}
		}
	})
}

// roundTrip returns s as the plan holds it: json.Marshal writes invalid
// UTF-8 as U+FFFD.
func roundTrip(t *testing.T, s string) string {
	t.Helper()
	js, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out string
	if err := json.Unmarshal(js, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// markedSurvives reports whether a value that was not null under a true
// mark is anything but the placeholder in masked. A key the sweep renamed
// cannot be followed and is skipped.
func markedSurvives(orig, masked, mark any) bool {
	switch mk := mark.(type) {
	case bool:
		return mk && orig != nil && masked != Sensitive
	case map[string]any:
		o, _ := orig.(map[string]any)
		m, _ := masked.(map[string]any)
		for k, x := range o {
			if y, ok := m[k]; ok && markedSurvives(x, y, mk[k]) {
				return true
			}
		}
	case []any:
		o, _ := orig.([]any)
		m, _ := masked.([]any)
		for i := range min(len(o), len(m), len(mk)) {
			if markedSurvives(o[i], m[i], mk[i]) {
				return true
			}
		}
	}
	return false
}

func unmarshal(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

// underTrue lists, as text, every string, key, and number of minNumber or
// more characters of v under a true mark: what the sweep must remove
// elsewhere. A bool and a shorter number are protected only by marks, which
// markedSurvives checks.
func underTrue(v, mark any) []string {
	var out []string
	switch mk := mark.(type) {
	case bool:
		if mk {
			swept(v, &out)
		}
	case map[string]any:
		if obj, ok := v.(map[string]any); ok {
			for k, x := range obj {
				out = append(out, underTrue(x, mk[k])...)
			}
		}
	case []any:
		if list, ok := v.([]any); ok {
			for i := range min(len(list), len(mk)) {
				out = append(out, underTrue(list[i], mk[i])...)
			}
		}
	}
	return out
}

func swept(v any, out *[]string) {
	switch v := v.(type) {
	case string:
		*out = append(*out, v)
	case json.Number:
		if len(v) >= minNumber {
			*out = append(*out, v.String())
		}
	case map[string]any:
		for k, x := range v {
			*out = append(*out, k)
			swept(x, out)
		}
	case []any:
		for _, x := range v {
			swept(x, out)
		}
	}
}

// stringsOf lists, as text, every string, map key, and number reachable
// from v. It leaves out the names of struct fields, which are the
// document's own keys, and every bool: the sweep leaves bools to the marks,
// and the leaves of a mark tree are bools.
func stringsOf(v reflect.Value, out *[]string) {
	switch v.Kind() {
	case reflect.String:
		*out = append(*out, v.String())
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			stringsOf(v.Elem(), out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			stringsOf(v.Field(i), out)
		}
	case reflect.Slice:
		for i := range v.Len() {
			stringsOf(v.Index(i), out)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			stringsOf(it.Key(), out)
			stringsOf(it.Value(), out)
		}
	}
}

// leaked returns the output string that still carries s. A secret of six
// bytes or more must not appear in any text between placeholders: plain, in
// hex of either case, in a base64 run, or after undoing URL or JSON escapes.
// A shorter one must not be a whole value, plain or behind up to two of
// those encodings. The decoding here is written apart from the mask's.
func leaked(out []string, s string) string {
	if s == "" {
		return ""
	}
	for _, o := range out {
		if len(s) < minSubstring {
			if decodesTo(o, s) {
				return o
			}
			continue
		}
		for _, seg := range strings.Split(o, Sensitive) {
			if strings.Contains(seg, s) || strings.Contains(strings.ToLower(seg), hex.EncodeToString([]byte(s))) || recoverable(seg, s) ||
				strings.Contains(unescapeURL(seg), s) || strings.Contains(unescapeJSON(seg), s) {
				return o
			}
		}
	}
	return ""
}

// decodesTo reports whether o is s, or s behind up to two URL, JSON, hex,
// or base64 encodings.
func decodesTo(o, s string) bool {
	if o == s {
		return true
	}
	for _, d := range oneDecoding(o) {
		if d == s || slices.Contains(oneDecoding(d), s) {
			return true
		}
	}
	return false
}

func oneDecoding(o string) []string {
	out := []string{unescapeURL(o), unescapeJSON(o)}
	if b, err := hex.DecodeString(o); err == nil {
		out = append(out, string(b))
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(o); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

var urlEscape = regexp.MustCompile(`%[0-9a-fA-F]{2}`)

func unescapeURL(s string) string {
	return urlEscape.ReplaceAllStringFunc(s, func(e string) string {
		b, _ := hex.DecodeString(e[1:])
		return string(b)
	})
}

var jsonEscape = regexp.MustCompile(`\\u[dD][89abAB][0-9a-fA-F]{2}\\u[dD][c-fC-F][0-9a-fA-F]{2}|\\u[0-9a-fA-F]{4}|\\["\\/bfnrt]`)

// unescapeJSON undoes each escape on its own, as Go's decoder would inside
// a JSON string. A surrogate pair is one escape: \ud800\udc00 is U+10000.
func unescapeJSON(s string) string {
	return jsonEscape.ReplaceAllStringFunc(s, func(e string) string {
		var out string
		if json.Unmarshal([]byte(`"`+e+`"`), &out) != nil {
			return e
		}
		return out
	})
}
