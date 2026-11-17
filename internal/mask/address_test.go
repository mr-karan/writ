package mask

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// TestSweepAddress checks that only the index keys of an address are
// swept, each by its value as HCL decodes it, that a swept key is written
// ["(sensitive)"], and that an address OpenTofu could not parse is swept
// whole.
func TestSweepAddress(t *testing.T) {
	tests := []struct {
		name, in string
		secrets  []string
		want     string
	}{
		{"string keys", `module.m["canary-key-01"].random_id.i["ab"]`, []string{"canary-key-01", "ab"}, `module.m["(sensitive)"].random_id.i["(sensitive)"]`},
		{"number key", `random_id.x[123456]`, []string{"123456"}, `random_id.x["(sensitive)"]`},
		{"short number key", `random_id.x[12]`, []string{"123456"}, `random_id.x[12]`},
		{"escaped quote and bracket", `random_id.x["k\"]x-secret"]`, []string{`k"]x-secret`}, `random_id.x["(sensitive)"]`},
		{"short escaped quote", `module.a["a\"b"].x.y`, []string{`a"b`}, `module.a["(sensitive)"].x.y`},
		{"escaped backslash", `x.y["a\\b"]`, []string{`a\b`}, `x.y["(sensitive)"]`},
		{"secret inside a longer key", `x.y["pre-canary-key-01-post"]`, []string{"canary-key-01"}, `x.y["(sensitive)"]`},
		{"nested data", `module.a["ab"].module.b[0].data.local_file.x["canary-key"]`, []string{"ab"}, `module.a["(sensitive)"].module.b[0].data.local_file.x["canary-key"]`},
		{"unicode module name", `module.cért.random_id.i`, []string{"random_id"}, `module.cért.random_id.i`},
		{"unicode resource name", `terraform_data.café`, []string{"terraform_data"}, `terraform_data.café`},
		{"short template key", `x.y["a$${b}"]`, []string{"a${b}"}, `x.y["(sensitive)"]`},
		{"long template key", `x.y["ab$${cd}"]`, []string{"ab${cd}"}, `x.y["(sensitive)"]`},
		{"directive key", `x.y["a%%{b}"]`, []string{"a%{b}"}, `x.y["(sensitive)"]`},
		{"long unicode escape", `x.y["\U000e0001"]`, []string{"\U000e0001"}, `x.y["(sensitive)"]`},
		{"short unicode escape", `x.y["\u00a0"]`, []string{"\u00a0"}, `x.y["(sensitive)"]`},
		{"escaped key that holds no secret", `x.y["q\"b\\s$${x}%%{y}\U000e0002"]`, []string{"canary-key-01"}, `x.y["q\"b\\s$${x}%%{y}\U000e0002"]`},
		{"unterminated key", `random_id.x["unterminated`, []string{"random_id"}, `(sensitive).x["unterminated`},
		{"key that is not a literal", `random_id.x[k]`, []string{"random_id"}, `(sensitive).x[k]`},
		{"template key", `random_id.x["${k}"]`, []string{"random_id"}, `(sensitive).x["${k}"]`},
		{"fractional key", `random_id.x[1.5]`, []string{"random_id"}, `(sensitive).x[1.5]`},
		{"splat", `random_id.x[*]`, []string{"random_id"}, `(sensitive).x[*]`},
		{"space", `random_id x`, []string{"random_id"}, `(sensitive) x`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sweepAddress(t, tt.in, tt.secrets...); got != tt.want {
				t.Errorf("address(%s) with secrets %q = %s; want %s", tt.in, tt.secrets, got, tt.want)
			}
		})
	}
}

func sweepAddress(t *testing.T, address string, secrets ...string) string {
	t.Helper()
	set := map[string]struct{}{}
	for _, s := range secrets {
		set[s] = struct{}{}
	}
	sw, err := newSweeper(set)
	if err != nil {
		t.Fatal(err)
	}
	s := &scan{sweeper: sw}
	got := s.address(address)
	if s.err != nil {
		t.Fatal(s.err)
	}
	return got
}

// TestPlanAddresses masks a plan OpenTofu wrote, testdata/address, whose
// sensitive variables set three for_each keys through nonsensitive(), one
// of them spells the type of terraform_data.café, and whose other keys
// need HCL's escapes. A key that holds a secret is swept in the address
// and the index; every other address keeps OpenTofu's text.
func TestPlanAddresses(t *testing.T) {
	var raw struct {
		ResourceChanges []struct {
			Address       string `json:"address"`
			ModuleAddress string `json:"module_address"`
			Type          string `json:"type"`
			Index         any    `json:"index"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(fixture(t, "address-plan.json"), &raw); err != nil {
		t.Fatal(err)
	}
	p := maskPlan(t, "address-plan.json")
	if len(p.ResourceChanges) != len(raw.ResourceChanges) || len(raw.ResourceChanges) != 11 {
		t.Fatalf("%d resource changes, want the fixture's %d", len(p.ResourceChanges), len(raw.ResourceChanges))
	}
	for i, w := range raw.ResourceChanges {
		got := p.ResourceChanges[i]
		wantAddress, wantIndex := w.Address, fmt.Sprint(w.Index)
		if w.ModuleAddress == "module.cert" {
			wantAddress, wantIndex = `module.cert.terraform_data.item["(sensitive)"]`, Sensitive
		}
		if got.Address != wantAddress || fmt.Sprint(got.Index) != wantIndex || got.ModuleAddress != w.ModuleAddress || got.Type != w.Type {
			t.Errorf("resource change %d is %s index %v module %q type %q; want %s index %s, as OpenTofu wrote %s", i,
				got.Address, got.Index, got.ModuleAddress, got.Type, wantAddress, wantIndex, w.Address)
		}
	}
	doc, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`ab$${cd}`, `a$${b}`, `\\U000e0001`, `ab${cd}`, `a${b}`, "\U000e0001"} {
		if strings.Contains(string(doc), key) {
			t.Errorf("the masked plan holds %s", key)
		}
	}
	if !strings.Contains(string(doc), `"address":"terraform_data.café"`) {
		t.Errorf("the masked plan lost terraform_data.café: %s", doc)
	}
}

// TestPlanSweepsEveryAddress checks that every field that holds an
// address, and the index its key mirrors, sweeps a key that holds a
// secret only once HCL decodes it.
func TestPlanSweepsEveryAddress(t *testing.T) {
	const key = `[\"ab$${cd}\"]`
	raw := `{"format_version":"1.2","variables":{"pw":{"value":"ab${cd}"}},` +
		`"configuration":{"root_module":{"variables":{"pw":{"sensitive":true}}}},` +
		`"resource_changes":[{"address":"module.m` + key + `.x.y` + key + `","previous_address":"module.m` + key + `.x.old` + key + `",` +
		`"module_address":"module.m` + key + `","mode":"managed","type":"x","name":"y","index":"ab${cd}","provider_name":"p",` +
		`"change":{"actions":["create"],"before":null,"after":null}}],` +
		`"relevant_attributes":[{"resource":"module.m` + key + `.x.y` + key + `","attribute":["name"]}]}`
	p, err := New(nil).Plan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	rc, ra := p.ResourceChanges[0], p.RelevantAttributes[0]
	const m = `module.m["(sensitive)"]`
	if rc.Address != m+`.x.y["(sensitive)"]` || rc.PreviousAddress != m+`.x.old["(sensitive)"]` || rc.ModuleAddress != m ||
		rc.Index != Sensitive || ra.Resource != m+`.x.y["(sensitive)"]` {
		t.Errorf("address %s, previous %s, module %s, index %v, relevant %s; want each key swept",
			rc.Address, rc.PreviousAddress, rc.ModuleAddress, rc.Index, ra.Resource)
	}
}

// TestAddressRendersAsOpenTofu writes addresses from many names and keys as
// OpenTofu writes them and checks that the sweep returns each one as it
// was, byte for byte, and with a secret key swept and the rest as it was.
func TestAddressRendersAsOpenTofu(t *testing.T) {
	names := []string{"x", "random_id", "terraform_data", "café", "módulo", "日本", "a-b", "_u", "ünï_2", "Ωmega"}
	runes := []rune{'a', 'Z', '0', ' ', '"', '\\', '$', '%', '{', '}', '[', ']', '.', '\n', '\r', '\t', '\x00', '\x7f',
		'\u00a0', '\u200b', '\u2028', '\ufeff', '\U000e0001', 'é', '日', '😀', '\ufffd'}
	rng := rand.New(rand.NewPCG(1, 2))
	name := func() string { return names[rng.IntN(len(names))] }
	key := func() string {
		var b strings.Builder
		for range rng.IntN(9) {
			b.WriteRune(runes[rng.IntN(len(runes))])
		}
		return b.String()
	}
	for i := range 3000 {
		// Each segment is a name, then an optional key: an int, a string,
		// or the secret, which one segment carries.
		type segment struct{ name, key string }
		var segs []segment
		for range rng.IntN(3) {
			segs = append(segs, segment{name: "module." + name()})
		}
		last := name() + "." + name()
		if rng.IntN(4) == 0 {
			last = "data." + last
		}
		segs = append(segs, segment{name: last})
		for j := range segs {
			switch rng.IntN(3) {
			case 0:
				segs[j].key = "[" + strconv.Itoa(rng.IntN(1_000_000)) + "]"
			case 1:
				segs[j].key = tofuKey(key())
			}
		}
		secret := fmt.Sprintf("sk-%08x%s", rng.Uint32(), key())
		at := rng.IntN(len(segs))
		segs[at].key = tofuKey(secret)
		var text, want []string
		for j, s := range segs {
			text = append(text, s.name+s.key)
			if j == at {
				want = append(want, s.name+`["`+Sensitive+`"]`)
			} else {
				want = append(want, s.name+s.key)
			}
		}
		address := strings.Join(text, ".")
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if got := sweepAddress(t, address, "unrelated-canary-value"); got != address {
				t.Errorf("address(%s) = %s, want it as OpenTofu wrote it", address, got)
			}
			if got, want := sweepAddress(t, address, secret), strings.Join(want, "."); got != want {
				t.Errorf("address(%s) with secret %q = %s, want %s", address, secret, got, want)
			}
		})
	}
}

// tofuKey writes a string instance key as OpenTofu 1.13.9 does,
// following StringKey.String in its internal/addrs/instance_key.go: an HCL
// quoted string with \n, \r, \t, \", and \\ escaped, $ and % doubled before {, and
// a rune that is not printable written \uXXXX or \UXXXXXXXX.
func tofuKey(k string) string {
	var b strings.Builder
	b.WriteString(`["`)
	for i, r := range k {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case (r == '$' || r == '%') && strings.HasPrefix(k[i+1:], "{"):
			b.WriteRune(r)
			b.WriteRune(r)
		case !unicode.IsPrint(r) && r > 0xffff:
			fmt.Fprintf(&b, `\U%08x`, r)
		case !unicode.IsPrint(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(`"]`)
	return b.String()
}
