package mask

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The canaries are the values testdata/stack/*.tfvars give the stack.
const (
	pwFirst   = "wc+pw/7f3a&9c2e41"
	pwSecond  = "wc+pw/b19d&e0c5a2"
	defaultPw = "wc-default-91ad5e"
	unmarked  = "wc-unmarked-2c6f88"
	label     = "wc-label-5b8e0d"
	owner     = "wc-owner-44e1a7"
	pin       = "739201"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// forms are the encodings that the verify-mask scan looks for.
func forms(s string) []string {
	return []string{s, base64.StdEncoding.EncodeToString([]byte(s)), hex.EncodeToString([]byte(s)), url.QueryEscape(s)}
}

// encode writes v as JSON without escaping, so a secret appears as its bytes.
func encode(t testing.TB, v any) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// holds reports whether raw JSON text carries s, as OpenTofu escapes it.
func holds(raw []byte, s string) bool {
	js, _ := json.Marshal(s)
	return bytes.Contains(raw, []byte(s)) || bytes.Contains(raw, bytes.Trim(js, `"`))
}

func assertNoLeak(t *testing.T, doc string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		for _, f := range forms(s) {
			if strings.Contains(doc, f) {
				t.Errorf("%q leaks as %q", s, f)
			}
		}
		if recoverable(doc, s) {
			t.Errorf("%q can be decoded from a base64 run", s)
		}
	}
}

// recoverable reports whether a base64 run in text decodes, at some
// alignment and in either alphabet, to bytes that hold s.
func recoverable(text, s string) bool {
	if len(s) < minSubstring {
		return false
	}
	notBase64 := func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("+/-_", r))
	}
	for _, run := range strings.FieldsFunc(text, notBase64) {
		run = strings.NewReplacer("-", "+", "_", "/").Replace(run)
		for i := 0; i < 4 && i < len(run); i++ {
			c := run[i:]
			if len(c)%4 == 1 {
				c = c[:len(c)-1]
			}
			b, err := base64.RawStdEncoding.DecodeString(c)
			if err == nil && strings.Contains(string(b), s) {
				return true
			}
		}
	}
	return false
}

func maskPlan(t *testing.T, name string, env ...string) *Plan {
	t.Helper()
	p, err := New(env).Plan(fixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func change(t *testing.T, p *Plan, address string) Change {
	t.Helper()
	i := slices.IndexFunc(p.ResourceChanges, func(rc ResourceChange) bool { return rc.Address == address })
	if i < 0 {
		t.Fatalf("no change for %s", address)
	}
	return p.ResourceChanges[i].Change
}

func attr(v any, path ...string) any {
	for _, k := range path {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

// stateValue reads one attribute of one resource from the recorded state.
func stateValue(t *testing.T, address, name string) string {
	t.Helper()
	var st struct {
		Values struct {
			RootModule struct {
				Resources []struct {
					Address string         `json:"address"`
					Values  map[string]any `json:"values"`
				} `json:"resources"`
			} `json:"root_module"`
		} `json:"values"`
	}
	if err := json.Unmarshal(fixture(t, "state.json"), &st); err != nil {
		t.Fatal(err)
	}
	for _, r := range st.Values.RootModule.Resources {
		if r.Address == address {
			s, _ := r.Values[name].(string)
			return s
		}
	}
	t.Fatalf("no %s in state.json", address)
	return ""
}

// TestPlanLeaks covers every place the recorded plans carry a secret. Each
// case first checks that the raw plan holds the secret, so a re-recorded
// fixture that stops exercising a leak fails here instead of passing.
func TestPlanLeaks(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		env     []string
		secret  func(t *testing.T) string
	}{
		{"sensitive variable value and nonsensitive() copies", "plan.json", nil, func(*testing.T) string { return pwFirst }},
		{"sensitive number", "plan.json", nil, func(*testing.T) string { return pin }},
		{"variable nobody marked, used only through length()", "plan.json", nil, func(*testing.T) string { return unmarked }},
		{"overridden default of a sensitive variable", "plan.json", nil, func(*testing.T) string { return defaultPw }},
		{"for_each key from a variable the environment names secret", "plan.json", []string{label}, func(*testing.T) string { return label }},
		{"unmarked value under a secret name", "plan.json", nil, func(*testing.T) string { return owner }},
		{"new secret in an update", "update-plan.json", nil, func(*testing.T) string { return pwSecond }},
		{"previous secret in before", "update-plan.json", nil, func(*testing.T) string { return pwFirst }},
		{"provider-marked private key", "update-plan.json", nil, func(t *testing.T) string { return stateValue(t, "tls_private_key.k", "private_key_pem") }},
		{"provider-marked password", "update-plan.json", nil, func(t *testing.T) string { return stateValue(t, "random_password.db", "result") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := tt.secret(t)
			if !holds(fixture(t, tt.fixture), secret) {
				t.Fatalf("%s does not hold %q; record the fixtures again", tt.fixture, secret)
			}
			assertNoLeak(t, encode(t, maskPlan(t, tt.fixture, tt.env...)), secret)
		})
	}
}

func TestPlanKeepsVariableNamesOnly(t *testing.T) {
	p := maskPlan(t, "plan.json")
	want := []string{"fallback", "label", "owner", "pin", "pw", "unmarked"}
	if !slices.Equal(p.Variables, want) {
		t.Errorf("variables = %q, want %q", p.Variables, want)
	}
}

func TestPlanConfiguration(t *testing.T) {
	p := maskPlan(t, "plan.json")
	want := Configuration{Providers: []Provider{
		{Key: "local", Name: "local", FullName: "registry.opentofu.org/hashicorp/local", VersionConstraint: "2.9.1"},
		{Key: "random", Name: "random", FullName: "registry.opentofu.org/hashicorp/random", VersionConstraint: "3.9.1"},
		{Key: "tls", Name: "tls", FullName: "registry.opentofu.org/hashicorp/tls", VersionConstraint: "4.4.2"},
	}}
	if !reflect.DeepEqual(p.Configuration, want) {
		t.Errorf("configuration = %+v, want %+v", p.Configuration, want)
	}
}

func TestPlanKeepsOnlyNamedKeys(t *testing.T) {
	allowed := []string{"format_version", "terraform_version", "timestamp", "errored", "variables", "configuration",
		"resource_changes", "resource_drift", "output_changes", "checks", "relevant_attributes"}
	for _, name := range []string{"plan.json", "update-plan.json"} {
		var doc map[string]any
		if err := json.Unmarshal([]byte(encode(t, maskPlan(t, name))), &doc); err != nil {
			t.Fatal(err)
		}
		for k := range doc {
			if !slices.Contains(allowed, k) {
				t.Errorf("%s: the masked document keeps %s", name, k)
			}
		}
	}
}

// TestPlanMarks checks that a marked value is replaced whole, not swept
// piece by piece, and that a changed secret still shows as changed.
func TestPlanMarks(t *testing.T) {
	first := maskPlan(t, "plan.json")
	if got := attr(change(t, first, "local_file.encoded").After, "content"); got != Sensitive {
		t.Errorf("base64encode(var.pw) = %v, want %s", got, Sensitive)
	}
	if got := attr(change(t, first, "random_pet.named").After, "keepers", "fallback"); got != Sensitive {
		t.Errorf("keepers.fallback = %v, want %s", got, Sensitive)
	}

	update := maskPlan(t, "update-plan.json")
	enc := change(t, update, "local_file.encoded")
	if !reflect.DeepEqual(enc.ChangedSensitivePaths, [][]any{{"content"}}) {
		t.Errorf("local_file.encoded changed_sensitive_paths = %v, want [[content]]", enc.ChangedSensitivePaths)
	}
	key := change(t, update, "tls_private_key.k")
	if len(key.ChangedSensitivePaths) != 0 {
		t.Errorf("an unchanged key has changed_sensitive_paths %v", key.ChangedSensitivePaths)
	}
	for _, side := range []any{key.Before, key.After} {
		for _, a := range []string{"private_key_pem", "private_key_openssh", "private_key_pem_pkcs8"} {
			if got := attr(side, a); got != Sensitive {
				t.Errorf("%s = %v, want %s", a, got, Sensitive)
			}
		}
		if got := attr(side, "public_key_openssh"); got != stateValue(t, "tls_private_key.k", "public_key_openssh") {
			t.Errorf("public_key_openssh = %v, want the unmarked key", got)
		}
	}
}

func TestPlanSweep(t *testing.T) {
	p := maskPlan(t, "plan.json", label)
	if got := attr(change(t, p, "local_file.plain").After, "content"); got != Sensitive {
		t.Errorf("nonsensitive(var.pw) = %v, want %s", got, Sensitive)
	}
	if got := p.OutputChanges["pw_copy"].After; got != Sensitive {
		t.Errorf("output pw_copy = %v, want %s", got, Sensitive)
	}
	i := slices.IndexFunc(p.ResourceChanges, func(rc ResourceChange) bool { return rc.Type == "random_id" })
	if rc := p.ResourceChanges[i]; rc.Address != `random_id.keyed["(sensitive)"]` || rc.Index != Sensitive {
		t.Errorf("for_each address = %s index %v, want the key masked", rc.Address, rc.Index)
	}

	update := maskPlan(t, "update-plan.json")
	if got := attr(change(t, update, "local_file.plain").Before, "content"); got != Sensitive {
		t.Errorf("the previous nonsensitive(var.pw) = %v, want %s", got, Sensitive)
	}
}

// TestPlanKeepsStructure checks that the sweep leaves whole what the
// configuration names: a secret that spells a version, a type, a name, a
// mode, a provider, an action, an action reason, an attribute a replace
// path starts with, or the name of a variable or an output leaves it as
// OpenTofu wrote it. The index key of an address is a value and is swept,
// and so is a key inside a value.
func TestPlanKeepsStructure(t *testing.T) {
	raw := fixture(t, "update-plan.json")
	var want struct {
		FormatVersion    string                     `json:"format_version"`
		TerraformVersion string                     `json:"terraform_version"`
		ResourceChanges  []ResourceChange           `json:"resource_changes"`
		OutputChanges    map[string]json.RawMessage `json:"output_changes"`
		Variables        map[string]json.RawMessage `json:"variables"`
		Configuration    struct {
			ProviderConfig map[string]struct {
				Name     string `json:"name"`
				FullName string `json:"full_name"`
			} `json:"provider_config"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	p := maskPlan(t, "update-plan.json", "1.2", "1.13.9", "managed", "local_file", "encoded", "k", "local",
		"registry.opentofu.org/hashicorp/local", "delete", "create", "no-op", "replace_because_cannot_update",
		"content", "pw_copy", "fallback", "keyed", label)
	if p.FormatVersion != want.FormatVersion || p.TerraformVersion != want.TerraformVersion {
		t.Errorf("versions %q and %q", p.FormatVersion, p.TerraformVersion)
	}
	if len(p.ResourceChanges) != len(want.ResourceChanges) {
		t.Fatalf("%d resource changes, want %d", len(p.ResourceChanges), len(want.ResourceChanges))
	}
	for i, w := range want.ResourceChanges {
		got := p.ResourceChanges[i]
		if got.Address != strings.Replace(w.Address, label, Sensitive, 1) || got.Mode != w.Mode || got.Type != w.Type || got.Name != w.Name ||
			got.ProviderName != w.ProviderName || got.ActionReason != w.ActionReason ||
			!slices.Equal(got.Change.Actions, w.Change.Actions) || !reflect.DeepEqual(got.Change.ReplacePaths, w.Change.ReplacePaths) {
			t.Errorf("resource change %d is %+v, want the structure of %+v", i, got, w)
		}
	}
	if rc := p.ResourceChanges[slices.IndexFunc(p.ResourceChanges, func(rc ResourceChange) bool { return rc.Name == "keyed" })]; rc.Index != Sensitive {
		t.Errorf("the index of %s is %v, want it swept", rc.Address, rc.Index)
	}
	if after, _ := change(t, p, "local_file.encoded").After.(map[string]any); after["content"] != nil || after[Sensitive] == nil {
		t.Errorf("a key inside a value is not swept: %v", after)
	}
	for name := range want.OutputChanges {
		if _, ok := p.OutputChanges[name]; !ok {
			t.Errorf("output %s is renamed: %v", name, slices.Collect(maps.Keys(p.OutputChanges)))
		}
	}
	if v := slices.Sorted(maps.Keys(want.Variables)); !slices.Equal(p.Variables, v) {
		t.Errorf("variables %q, want %q", p.Variables, v)
	}
	for _, pr := range p.Configuration.Providers {
		if w := want.Configuration.ProviderConfig[pr.Key]; pr.Name != w.Name || pr.FullName != w.FullName {
			t.Errorf("provider %+v, want %+v", pr, w)
		}
	}
}

// TestPlanSweepsNumbers checks that nonsensitive() on a sensitive number
// does not carry it out, as a number or as its text.
func TestPlanSweepsNumbers(t *testing.T) {
	after := change(t, maskPlan(t, "plan.json"), "random_integer.pinned").After
	if got := attr(after, "min"); got != Sensitive {
		t.Errorf("min = nonsensitive(var.pin) = %v, want %s", got, Sensitive)
	}
	if got := attr(after, "keepers", "pin"); got != Sensitive {
		t.Errorf("keepers.pin = tostring(nonsensitive(var.pin)) = %v, want %s", got, Sensitive)
	}
	if got := attr(after, "max"); got != json.Number("739211") {
		t.Errorf("max = %v, want 739211 kept", got)
	}
}

// TestPlanSweepsLongNumbers checks that the mask learns a sensitive number
// of minNumber digits or more and sweeps it everywhere, as a whole value
// and inside a string.
func TestPlanSweepsLongNumbers(t *testing.T) {
	raw := `{"format_version":"1.2",
		"variables":{"n":{"value":739201}},
		"configuration":{"root_module":{"variables":{"n":{"sensitive":true}}}},
		"resource_changes":[{"address":"x.y","mode":"managed","type":"x","name":"y","provider_name":"p","change":{
			"actions":["create"],"before":null,
			"after":{"n":739201,"n_text":"pin 739201","m":17,"big":100000,"big_text":"size 100000","big_copy":100000},
			"after_sensitive":{"big":true}}}]}`
	p, err := New(nil).Plan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"n": Sensitive, "n_text": "pin " + Sensitive, "m": json.Number("17"), "big": Sensitive, "big_text": "size " + Sensitive, "big_copy": Sensitive}
	if got := p.ResourceChanges[0].Change.After; !reflect.DeepEqual(got, want) {
		t.Errorf("after = %v, want %v", got, want)
	}
	if got := p.ResourceChanges[0].Change.AfterSensitive; !reflect.DeepEqual(got, map[string]any{"big": true}) {
		t.Errorf("after_sensitive = %v, want the mark tree kept", got)
	}
}

// TestPlanSmallScalarsOnlyByMark checks that a sensitive bool, and a
// sensitive number shorter than minNumber, are masked where a mark covers
// them and nowhere else. Sweeping them would blank every equal value: a
// plan is full of true, false, 80, and 65535.
func TestPlanSmallScalarsOnlyByMark(t *testing.T) {
	raw := `{"format_version":"1.2",
		"variables":{"on":{"value":true},"port":{"value":80}},
		"configuration":{"root_module":{"variables":{"on":{"sensitive":true},"port":{"sensitive":true}}}},
		"resource_changes":[{"address":"x.y","mode":"managed","type":"x","name":"y","provider_name":"p","change":{
			"actions":["create"],"before":null,
			"after":{"flag":true,"other":true,"off":false,"word":"true","port":80,"http":80,"text":"80","url":"http://h:80/","max":65535,"max_copy":65535,"list":[true,80,65535]},
			"after_sensitive":{"flag":true,"port":true,"max":true}}}]}`
	m := New(nil)
	p, err := m.Plan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"flag": Sensitive, "other": true, "off": false, "word": "true",
		"port": Sensitive, "http": json.Number("80"), "text": "80", "url": "http://h:80/",
		"max": Sensitive, "max_copy": json.Number("65535"),
		"list": []any{true, json.Number("80"), json.Number("65535")},
	}
	if got := p.ResourceChanges[0].Change.After; !reflect.DeepEqual(got, want) {
		t.Errorf("after = %v, want %v", got, want)
	}
	if got, want := string(m.Event([]byte(`{"ok":true,"port":80,"text":"80"}`))), `{"ok":true,"port":80,"text":"80"}`; got != want {
		t.Errorf("event = %s, want %s", got, want)
	}
}

func TestPlanMasksByName(t *testing.T) {
	p := maskPlan(t, "plan.json")
	if got := attr(change(t, p, "random_pet.named").After, "keepers", "db_password"); got != Sensitive {
		t.Errorf("keepers.db_password = %v, want %s", got, Sensitive)
	}
	if got := attr(change(t, p, "random_pet.named").After, "keepers", "size"); got != "18" {
		t.Errorf("keepers.size = %v, want 18", got)
	}
	if got := attr(p.OutputChanges["settings"].After, "db_password"); got != Sensitive {
		t.Errorf("output settings.db_password = %v, want %s", got, Sensitive)
	}
	if got := attr(p.OutputChanges["settings"].After, "size"); got != json.Number("3") {
		t.Errorf("output settings.size = %v, want 3", got)
	}
}

// TestPlanMasksByNameBeforeSweep checks that a secret name is read before
// the sweep can rewrite it: a run whose environment holds the name itself
// still masks the value under it.
func TestPlanMasksByNameBeforeSweep(t *testing.T) {
	assertNoLeak(t, encode(t, maskPlan(t, "plan.json", "db_password")), owner)

	for _, tt := range []struct {
		name, raw, secret string
		env               []string
	}{
		{"output value", `{"format_version":"1.2","output_changes":{"o":{"actions":["create"],"after":{"db_password":"canary-value"}}}}`, "canary-value", nil},
		{"output named secret", `{"format_version":"1.2","output_changes":{"api_token":{"actions":["create"],"after":"canary-value"}}}`, "canary-value", nil},
		{"key that is an environment secret", `{"format_version":"1.2","resource_changes":[{"address":"x.y","mode":"managed","type":"x","name":"y","provider_name":"p","change":{"actions":["create"],"before":null,"after":{"password":"another-secret-value"}}}]}`, "another-secret-value", []string{"password"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(tt.env).Plan([]byte(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			assertNoLeak(t, encode(t, p), tt.secret)
		})
	}
}

// TestPlanShortSecret checks that a secret shorter than six bytes is
// replaced where it is a whole value and nowhere else.
func TestPlanShortSecret(t *testing.T) {
	p := maskPlan(t, "plan.json", "0777")
	after := change(t, p, "local_file.plain").After
	if got := attr(after, "file_permission"); got != Sensitive {
		t.Errorf("file_permission = %v, want %s", got, Sensitive)
	}
	if got := attr(after, "filename"); got != "./plain.txt" {
		t.Errorf("filename = %v, want it kept", got)
	}
}

// TestPlanFailsClosed edits the recorded plan into shapes the mask was not
// tested with. Each must be refused, with no secret in the error.
func TestPlanFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		edit func(plan map[string]any)
	}{
		{"format major version 2", func(p map[string]any) { p["format_version"] = "2.0" }},
		{"no format version", func(p map[string]any) { delete(p, "format_version") }},
		{"object mark on a string", func(p map[string]any) {
			setMark(p, "local_file.plain", map[string]any{"content": map[string]any{"x": true}})
		}},
		{"list mark on an object", func(p map[string]any) {
			setMark(p, "local_file.plain", []any{true})
		}},
		{"mark that is not a bool", func(p map[string]any) {
			setMark(p, "local_file.plain", map[string]any{"content": "yes"})
		}},
		{"mark on a key the value does not hold", func(p map[string]any) {
			setMark(p, "local_file.plain", map[string]any{"missing": true})
		}},
		{"invalid mark on a key the value does not hold", func(p map[string]any) {
			setMark(p, "local_file.plain", map[string]any{"missing": "invalid"})
		}},
		{"invalid mark on a null before", func(p map[string]any) {
			findChange(p, "local_file.plain")["before_sensitive"] = map[string]any{"missing": "invalid"}
		}},
		{"mark on an omitted key that after_unknown does not explain", func(p map[string]any) {
			rc := findChange(p, "random_password.db")
			rc["after_unknown"].(map[string]any)["result"] = false
		}},
		{"list mark longer than its value", func(p map[string]any) {
			rc := findChange(p, "local_file.plain")
			rc["after"].(map[string]any)["content"] = []any{pwFirst}
			rc["after_sensitive"] = map[string]any{"content": []any{false, true}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var plan map[string]any
			if err := json.Unmarshal(fixture(t, "plan.json"), &plan); err != nil {
				t.Fatal(err)
			}
			tt.edit(plan)
			p, err := New(nil).Plan([]byte(encode(t, plan)))
			if err == nil {
				t.Fatalf("masked a plan with %s: %+v", tt.name, p)
			}
			assertNoLeak(t, err.Error(), pwFirst)
		})
	}
}

func findChange(p map[string]any, address string) map[string]any {
	for _, rc := range p["resource_changes"].([]any) {
		rc := rc.(map[string]any)
		if rc["address"] == address {
			return rc["change"].(map[string]any)
		}
	}
	panic("no change for " + address)
}

func setMark(p map[string]any, address string, mark any) {
	findChange(p, address)["after_sensitive"] = mark
}

func TestPlanRejectsNonJSON(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"format_version":"1.2"} {}`, `{"format_version":"1.2"}}`, `{"format_version":"1.2"}]`, `{"format_version":"1.2"} x`} {
		if _, err := New(nil).Plan([]byte(raw)); err == nil {
			t.Errorf("masked %q", raw)
		}
	}
}

func lines(t *testing.T, name string) [][]byte {
	t.Helper()
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(fixture(t, name)))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		out = append(out, slices.Clone(sc.Bytes()))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestEvents sweeps the recorded event streams with what the plan taught the
// mask. The apply stream prints the nonsensitive() output's value and the
// for_each key; a line with no secret must come through unchanged.
func TestEvents(t *testing.T) {
	m := New([]string{label})
	if _, err := m.Plan(fixture(t, "plan.json")); err != nil {
		t.Fatal(err)
	}
	for stream, printed := range map[string][]string{
		"plan-events.jsonl":  {label},
		"apply-events.jsonl": {label, pwFirst},
	} {
		t.Run(stream, func(t *testing.T) {
			for _, s := range printed {
				if !holds(fixture(t, stream), s) {
					t.Fatalf("%s does not print %q; record the fixtures again", stream, s)
				}
			}
			var out strings.Builder
			for _, line := range lines(t, stream) {
				got := m.Event(line)
				out.Write(got)
				out.WriteByte('\n')
				if bytes.Contains(line, []byte("wc")) {
					continue
				}
				var a, b any
				if err := json.Unmarshal(line, &a); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(got, &b); err != nil {
					t.Fatalf("swept event is not JSON: %v: %s", err, got)
				}
				if !reflect.DeepEqual(a, b) {
					t.Errorf("an event with no secret changed:\n%s\n%s", line, got)
				}
			}
			assertNoLeak(t, out.String(), pwFirst, label, owner)
		})
	}

	var outputs struct {
		Outputs map[string]struct {
			Value any `json:"value"`
		} `json:"outputs"`
	}
	for _, line := range lines(t, "apply-events.jsonl") {
		if bytes.Contains(line, []byte(`"type":"outputs"`)) {
			if err := json.Unmarshal(m.Event(line), &outputs); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := outputs.Outputs["pw_copy"].Value; got != Sensitive {
		t.Errorf("outputs event pw_copy = %v, want %s", got, Sensitive)
	}
	if got := attr(outputs.Outputs["settings"].Value, "db_password"); got != Sensitive {
		t.Errorf("outputs event settings.db_password = %v, want %s", got, Sensitive)
	}

	if got := string(m.Event([]byte("panic: token " + pwFirst + " leaked"))); got != "panic: token "+Sensitive+" leaked" {
		t.Errorf("text line = %q", got)
	}
}

func TestIndex(t *testing.T) {
	m := New([]string{label})
	rows, err := m.Index(fixture(t, "state.json"), only([]string{"id", "filename", "content", "private_key_pem", "keepers", "min", "result"}))
	if err != nil {
		t.Fatal(err)
	}
	byAddr := map[string]map[string]string{}
	for _, r := range rows {
		byAddr[r.Address] = r.Attrs
	}
	want := map[string]map[string]string{
		"local_file.encoded":             {"id": stateValue(t, "local_file.encoded", "id"), "filename": "./encoded.txt"},
		"local_file.json":                {"id": stateValue(t, "local_file.json", "id"), "filename": "./json.txt", "content": `{"v":"` + Sensitive + `"}`},
		"random_integer.pinned":          {"id": stateValue(t, "random_integer.pinned", "id"), "min": Sensitive, "result": stateValue(t, "random_integer.pinned", "id")},
		"local_file.plain":               {"id": stateValue(t, "local_file.plain", "id"), "filename": "./plain.txt", "content": Sensitive},
		"local_file.wrapped":             {"id": stateValue(t, "local_file.wrapped", "id"), "filename": "./wrapped.txt"},
		`random_id.keyed["(sensitive)"]`: {"id": stateValue(t, `random_id.keyed["`+label+`"]`, "id")},
		"random_password.db":             {"id": "none"},
		"random_pet.named":               {"id": stateValue(t, "random_pet.named", "id")},
		"tls_private_key.k":              {"id": stateValue(t, "tls_private_key.k", "id")},
	}
	if wrapped := byAddr["local_file.wrapped"]["content"]; !strings.Contains(wrapped, Sensitive) {
		t.Errorf("local_file.wrapped content = %q, want the encoded secret swept", wrapped)
	}
	delete(byAddr["local_file.wrapped"], "content")
	if !reflect.DeepEqual(byAddr, want) {
		t.Errorf("rows = %v\nwant %v", byAddr, want)
	}
	if !slices.IsSortedFunc(rows, func(a, b Row) int { return strings.Compare(a.Address, b.Address) }) {
		t.Error("rows are not sorted by address")
	}
	assertNoLeak(t, encode(t, rows), pwFirst, label, pin, stateValue(t, "tls_private_key.k", "private_key_pem"))
}

func TestIndexSkipsSecretNames(t *testing.T) {
	raw := `{"format_version":"1.0","values":{"root_module":{"resources":[
		{"address":"x.a","mode":"managed","type":"x","provider_name":"p","values":{"id":"a","api_token":"plain-but-named"},"sensitive_values":{}},
		{"address":"data.x.b","mode":"data","type":"x","provider_name":"p","values":{"id":"b"},"sensitive_values":{}}]}}}`
	rows, err := New(nil).Index([]byte(raw), only([]string{"id", "api_token"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{Address: "x.a", Type: "x", Provider: "p", Attrs: map[string]string{"id": "a"}}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
}

// TestIndexAttrsByType checks that a row keeps the attributes named for
// its own resource type only.
func TestIndexAttrsByType(t *testing.T) {
	raw := `{"format_version":"1.0","values":{"root_module":{"resources":[
		{"address":"nomad_job.web","mode":"managed","type":"nomad_job","provider_name":"p","values":{"id":"web","namespace":"apps","modify_index":"42"},"sensitive_values":{}},
		{"address":"x.a","mode":"managed","type":"x","provider_name":"p","values":{"id":"a","namespace":"not-for-x"},"sensitive_values":{}}]}}}`
	attrs := func(typ string) []string {
		if typ == "nomad_job" {
			return []string{"id", "namespace", "modify_index"}
		}
		return []string{"id"}
	}
	rows, err := New(nil).Index([]byte(raw), attrs)
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{Address: "nomad_job.web", Type: "nomad_job", Provider: "p", Attrs: map[string]string{"id": "web", "namespace": "apps", "modify_index": "42"}},
		{Address: "x.a", Type: "x", Provider: "p", Attrs: map[string]string{"id": "a"}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
}

func TestIndexFailsClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown format": `{"format_version":"2.0","values":{"root_module":{}}}`,
		"mark mismatch":  `{"format_version":"1.0","values":{"root_module":{"resources":[{"address":"x.a","mode":"managed","type":"x","values":{"id":"a"},"sensitive_values":{"id":{"y":true}}}]}}}`,
	} {
		if _, err := New(nil).Index([]byte(raw), only([]string{"id"})); err == nil {
			t.Errorf("%s: indexed", name)
		}
	}
}

// TestShortSecretEncodedWhole checks that a secret shorter than minSubstring
// is replaced in a plan and an index row where a value is the secret behind
// one or two URL, JSON, hex, or base64 encodings, and kept where the decoded
// value only holds it.
func TestShortSecretEncodedWhole(t *testing.T) {
	const secret = "a/b"
	for _, tt := range []struct{ name, whole, part string }{
		{"URL escape", "a%2fb", "xa%2fb"},
		{"uppercase URL escape", "a%2Fb", "xa%2Fb"},
		{"JSON escape", `a\u002fb`, `xa\u002fb`},
		{"JSON solidus escape", `a\/b`, `xa\/b`},
		{"hex", "612f62", "78612f62"},
		{"base64", "YS9i", "eGEvYg"},
		{"URL escape twice", "a%252fb", "xa%252fb"},
		{"base64 of a URL escape", "YSUyZmI", "eGElMmZi"},
		{"hex of JSON escapes, past one layer's cap", "5c75303036315c75303032665c7530303632", "785c75303036315c75303032665c7530303632"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			vals := encode(t, map[string]any{"whole": tt.whole, "part": tt.part})
			want := map[string]any{"whole": Sensitive, "part": tt.part}

			raw := `{"format_version":"1.2","variables":{"pw":{"value":"` + secret + `"}},` +
				`"configuration":{"root_module":{"variables":{"pw":{"sensitive":true}}}},` +
				`"resource_changes":[{"address":"x.y","mode":"managed","type":"x","name":"y","provider_name":"p","change":{` +
				`"actions":["create"],"before":null,"after":` + vals + `}}]}`
			p, err := New(nil).Plan([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if got := p.ResourceChanges[0].Change.After; !reflect.DeepEqual(got, want) {
				t.Errorf("plan after = %v, want %v", got, want)
			}

			state := `{"format_version":"1.0","values":{"root_module":{"resources":[` +
				`{"address":"x.y","mode":"managed","type":"x","provider_name":"p","values":` + vals + `,"sensitive_values":{}}]}}}`
			rows, err := New([]string{secret}).Index([]byte(state), only([]string{"whole", "part"}))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := rows[0].Attrs, map[string]string{"whole": Sensitive, "part": tt.part}; !reflect.DeepEqual(got, want) {
				t.Errorf("index attrs = %v, want %v", got, want)
			}
		})
	}
}

// TestIndexWholeResourceMark checks that a true mark on a whole resource,
// sensitive_values: true, keeps every attribute out of its row, a bool and
// a short number included, in the root module and in a child module.
func TestIndexWholeResourceMark(t *testing.T) {
	raw := `{"format_version":"1.0","values":{"root_module":{
		"resources":[{"address":"x.y","mode":"managed","type":"x","provider_name":"p","values":{"port":80,"on":true,"name":"plain-name"},"sensitive_values":true}],
		"child_modules":[{"resources":[{"address":"module.m.x.z","mode":"managed","type":"x","provider_name":"p","values":{"port":81,"on":false},"sensitive_values":true}]}]}}}`
	rows, err := New(nil).Index([]byte(raw), only([]string{"port", "on", "name"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{Address: "module.m.x.z", Type: "x", Provider: "p", Attrs: map[string]string{}},
		{Address: "x.y", Type: "x", Provider: "p", Attrs: map[string]string{}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
}

// TestPlanSweepsMarkedKeys checks that a map key under a true mark is
// learned and swept where it appears unmarked.
func TestPlanSweepsMarkedKeys(t *testing.T) {
	raw := `{"format_version":"1.2","resource_changes":[{"address":"x.y","mode":"managed","type":"x","name":"y","provider_name":"p","change":{
		"actions":["create"],"before":null,
		"after":{"tags":{"canary-key-name":"v"},"copy":"see canary-key-name"},
		"after_sensitive":{"tags":true}}}]}`
	p, err := New(nil).Plan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"tags": Sensitive, "copy": "see " + Sensitive}
	if got := p.ResourceChanges[0].Change.After; !reflect.DeepEqual(got, want) {
		t.Errorf("after = %v, want %v", got, want)
	}
}

// only gives the same attributes for every resource type.
func only(attrs []string) func(string) []string {
	return func(string) []string { return attrs }
}

// A Mask that follows a run's credentials sweeps those handed to the run
// after the Mask was made, counts them toward the run's limit, and leaves
// them out of Secrets, which a state's secret set carries on.
func TestFollow(t *testing.T) {
	var live []string
	m := New([]string{label})
	m.Follow(func() []string { return slices.Clone(live) })
	if got := string(m.Event([]byte("first " + owner))); !strings.Contains(got, owner) {
		t.Fatalf("before it is served: %s", got)
	}
	live = append(live, owner)
	if got := string(m.Event([]byte("first " + owner))); got != "first "+Sensitive {
		t.Fatalf("after it is served: %s", got)
	}
	live = append(live, pwFirst)
	if got := string(m.Event([]byte(`{"text":"refreshed ` + pwFirst + ` and ` + owner + `"}`))); got != `{"text":"refreshed `+Sensitive+` and `+Sensitive+`"}` {
		t.Fatalf("after a refresh: %s", got)
	}
	if got := m.Secrets(); !slices.Equal(got, []string{label}) {
		t.Fatalf("Secrets() = %q, want only what the Mask was made with", got)
	}
	live = append(live, strings.Repeat("x", maxSecretBytes))
	if got := string(m.Event([]byte("anything"))); got != Sensitive {
		t.Fatalf("past the limit: %s", got)
	}
}
