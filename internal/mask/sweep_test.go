package mask

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// TestEventSpellings sweeps spellings of a secret that a generator of
// encoded forms would miss. Each input is written out by hand.
func TestEventSpellings(t *testing.T) {
	tests := []struct {
		name, secret, line, want string
	}{
		{"lowercase URL escapes", "abc/def?ghi", `{"m":"q=abc%2fdef%3fghi&x=1"}`, `{"m":"q=(sensitive)&x=1"}`},
		{"mixed-case URL escapes", "abc/def?ghi", `{"m":"abc%2Fdef%3fghi"}`, `{"m":"(sensitive)"}`},
		{"query form with spaces", "two words here", `{"m":"a=two+words+here"}`, `{"m":"a=(sensitive)"}`},
		{"mixed-case hex", "canary-value", `{"m":"63616E6172792D76616c7565"}`, `{"m":"(sensitive)"}`},
		{"hex at an odd offset in a run", "canary-value", `{"m":"a63616e6172792d76616c7565"}`, `{"m":"a(sensitive)"}`},
		{"JSON escape inside a string", "canary-value", `{"m":"say \\u0063anary-value now"}`, `{"m":"say (sensitive) now"}`},
		{"uppercase JSON escape", "canary-value", `{"m":"canary\\u002Dvalue"}`, `{"m":"(sensitive)"}`},
		{"base64 at an offset", "canary-value", `{"m":"Basic dXNlcjpjYW5hcnktdmFsdWU="}`, `{"m":"Basic dXNlcj(sensitive)="}`},
		{"base64 at an offset in a run", "canary-value", `{"m":"xyzdXNlcjpjYW5hcnktdmFsdWU="}`, `{"m":"xyzdXNlcj(sensitive)="}`},
		{"base64 of escaped JSON", "wc+pw/7f3a&9c2e41", `{"m":"` + base64.StdEncoding.EncodeToString([]byte(`{"v":"wc+pw/7f3a`+"\\"+`u00269c2e41"}`)) + `"}`, `{"m":"eyJ2Ijoi(sensitive)J9"}`},
		{"URL-escaped JSON escape", "canary-value", `{"m":"%5Cu0063anary-value"}`, `{"m":"(sensitive)"}`},
		{"map key", "canary-value", `{"m":{"canary-value":1,"k-canary-value":2}}`, `{"m":{"(sensitive)":1,"k-(sensitive)":2}}`},
		{"number", "739201", `{"message":739201}`, `{"message":"(sensitive)"}`},
		{"number holding the secret", "739201", `{"message":17392011}`, `{"message":"(sensitive)"}`},
		{"short secret as a whole number", "42", `{"a":42,"b":420,"c":"42"}`, `{"a":"(sensitive)","b":420,"c":"(sensitive)"}`},
		{"short secret that spells a bool", "true", `{"a":true,"b":false,"c":"true"}`, `{"a":true,"b":false,"c":"(sensitive)"}`},
		{"short secret in hex, whole", "1234", `{"a":"31323334","b":"x31323334"}`, `{"a":"(sensitive)","b":"x31323334"}`},
		{"short secret URL-escaped, whole", "a/b", `{"a":"a%2fb","b":"a%2Fb","c":"xa%2fb"}`, `{"a":"(sensitive)","b":"(sensitive)","c":"xa%2fb"}`},
		{"short secret as hex of JSON escapes, whole", "a/b", `{"a":"5c75303036315c75303032665c7530303632"}`, `{"a":"(sensitive)"}`},
		{"short secret URL-escaped twice, whole", "a/b", `{"a":"a%252fb","c":"xa%252fb"}`, `{"a":"(sensitive)","c":"xa%252fb"}`},
		{"short secret JSON-escaped, whole", "a/b", `{"a":"a\\u002fb","b":"a\\/b","c":"xa\\u002fb"}`, `{"a":"(sensitive)","b":"(sensitive)","c":"xa\\u002fb"}`},
		{"short secret in base64, whole", "ab", `{"a":"YWI=","b":"YWI","c":"eGFi"}`, `{"a":"(sensitive)","b":"(sensitive)","c":"eGFi"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(New([]string{tt.secret}).Event([]byte(tt.line))); got != tt.want {
				t.Errorf("Event(%s) = %s, want %s", tt.line, got, tt.want)
			}
		})
	}
}

// syntheticPlan writes a plan of n resources. Each has a marked password,
// an unmarked copy of the previous resource's password inside a longer
// string, a policy document of about a kilobyte, and a few plain
// attributes.
func syntheticPlan(n int) []byte {
	policy := strings.Repeat(`{\"Effect\":\"Allow\",\"Action\":[\"s3:GetObject\",\"s3:ListBucket\"],\"Resource\":\"arn:x:s3:::bucket-name/*\"},`, 10)
	var b strings.Builder
	b.WriteString(`{"format_version":"1.2","terraform_version":"1.13.9","resource_changes":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"address":"x_thing.r%[1]d","mode":"managed","type":"x_thing","name":"r%[1]d","provider_name":"registry.opentofu.org/x/x","change":{"actions":["create"],"before":null,`+
			`"after":{"name":"thing-%[1]d","password":"%[3]x-k3j4h5g6","note":"previous is %[4]x-k3j4h5g6, keep it","tags":{"team":"core","env":"prod","index":"%[1]d"},"size":%[1]d,"enabled":true,"description":"a resource that the benchmark plan holds many copies of","policy":"[`+policy+`{}]"},`+
			`"after_unknown":{"id":true},"before_sensitive":false,"after_sensitive":{"password":true,"tags":{}}}}`, i, max(i-1, 0), pw(i), pw(max(i-1, 0)))
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// pw spreads the passwords' first bytes over the hex digits, as random
// secrets spread them over their alphabet.
func pw(i int) uint64 { return uint64(i+1) * 0x9e3779b97f4a7c15 }

func BenchmarkPlan(b *testing.B) {
	for _, n := range []int{100, 400, 1600} {
		raw := syntheticPlan(n)
		b.Run(fmt.Sprintf("resources=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := New(nil).Plan(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestPlanRefusesTooManySecrets checks that a run whose secrets would make
// the matcher too large stores no plan and no event. Secrets shorter than
// minSubstring count toward the limit as well.
func TestPlanRefusesTooManySecrets(t *testing.T) {
	for _, size := range []int{1024, minSubstring - 1} {
		t.Run(fmt.Sprintf("%d-byte secrets", size), func(t *testing.T) {
			var vals []string
			for i := 0; len(vals)*size <= MaxSecretBytes; i++ {
				n := strconv.FormatInt(int64(i), 36)
				vals = append(vals, strings.Repeat("0", size-len(n))+n)
			}
			raw := `{"format_version":"1.2","variables":{"big":{"value":["` + strings.Join(vals, `","`) + `"]}},` +
				`"configuration":{"root_module":{"variables":{"big":{"sensitive":true}}}}}`
			m := New(nil)
			if p, err := m.Plan([]byte(raw)); err == nil {
				t.Fatalf("masked a plan with %d bytes of secrets: %d variables", len(vals)*size, len(p.Variables))
			}
			if got := string(m.Event([]byte(`{"m":"` + vals[0] + `"}`))); got != Sensitive {
				t.Errorf("event = %.40s..., want %s", got, Sensitive)
			}
		})
	}
}

// TestPlanRefusesCostlyString checks that a string that costs more than the
// sweep allows fails the plan and blanks the event, rather than passing
// through unswept.
func TestPlanRefusesCostlyString(t *testing.T) {
	long := strings.Repeat("0123456789abcdef", maxStringWork/16/16)
	raw := `{"format_version":"1.2","variables":{"pw":{"value":"canary-value"}},` +
		`"configuration":{"root_module":{"variables":{"pw":{"sensitive":true}}}},` +
		`"resource_changes":[{"address":"x.y","mode":"managed","type":"x","name":"y","provider_name":"p","change":{` +
		`"actions":["create"],"before":null,"after":{"content":"canary-value ` + long + `"}}}]}`
	m := New(nil)
	if _, err := m.Plan([]byte(raw)); err == nil {
		t.Fatal("masked a plan with a string that costs more than maxStringWork")
	}
	if got := string(m.Event([]byte(`{"m":"canary-value ` + long + `"}`))); got != Sensitive {
		t.Errorf("event = %.40s..., want %s", got, Sensitive)
	}
	short := long[:len(long)/32]
	if got, want := string(m.Event([]byte(`{"m":"canary-value `+short+`"}`))), `{"m":"(sensitive) `+short+`"}`; got != want {
		t.Errorf("a string within the limit: event = %.40s..., want %.40s...", got, want)
	}
}

// TestEventReplacementFormsSecret checks that a string in which replacing
// one secret spells another is replaced whole.
func TestEventReplacementFormsSecret(t *testing.T) {
	m := New([]string{"canary-one", "xx" + Sensitive + "yy"})
	if got := string(m.Event([]byte(`{"m":"xxcanary-oneyy"}`))); got != `{"m":"(sensitive)"}` {
		t.Errorf("event = %s, want the string replaced whole", got)
	}
}
