package policy_test

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/policy"
)

func TestPolicyDecisions(t *testing.T) {
	cases := []struct {
		name    string
		sets    []policy.Set
		outcome policy.Outcome
		results []policy.Result
	}{
		{
			name:    "a rule with no message passes",
			sets:    []policy.Set{rules("s", "deny contains \"no\" if false\n")},
			outcome: policy.Pass,
		},
		{
			name:    "a warning alone warns",
			sets:    []policy.Set{rules("s", "warn contains \"look\" if input.writ.environment == \"prod\"\n")},
			outcome: policy.Warn,
			results: []policy.Result{{Set: "s", Level: policy.Mandatory, Rule: "warn", Outcome: policy.Warn, Message: "look"}},
		},
		{
			name:    "an absent level is mandatory, and its deny denies",
			sets:    []policy.Set{set("s", "", "p.rego", "package main\n\ndeny contains \"no\" if true\n")},
			outcome: policy.Deny,
			results: []policy.Result{{Set: "s", Level: policy.Mandatory, Rule: "deny", Outcome: policy.Deny, Message: "no"}},
		},
		{
			name:    "an advisory deny only warns",
			sets:    []policy.Set{set("adv", policy.Advisory, "p.rego", "package main\n\ndeny contains \"no\" if true\n")},
			outcome: policy.Warn,
			results: []policy.Result{{Set: "adv", Level: policy.Advisory, Rule: "deny", Outcome: policy.Warn, Message: "no"}},
		},
		{
			name: "conftest's rule names and message objects",
			sets: []policy.Set{rules("s", `deny_tags contains "a" if true

violation contains {"msg": "b", "details": {}} if true

warn_size contains "c" if true

helper contains "not read" if true
`)},
			outcome: policy.Deny,
			results: []policy.Result{
				{Set: "s", Level: policy.Mandatory, Rule: "deny_tags", Outcome: policy.Deny, Message: "a"},
				{Set: "s", Level: policy.Mandatory, Rule: "violation", Outcome: policy.Deny, Message: "b"},
				{Set: "s", Level: policy.Mandatory, Rule: "warn_size", Outcome: policy.Warn, Message: "c"},
			},
		},
		{
			name:    "a rule of another package is not read",
			sets:    []policy.Set{set("s", "", "p.rego", "package main\n\nwarn contains \"w\" if false\n", "q.rego", "package other\n\ndeny contains \"no\" if true\n")},
			outcome: policy.Pass,
		},
		{
			name: "sets are judged apart, so one cannot change another's rules",
			sets: []policy.Set{
				rules("server", "allowed := {\"random_pet\"}\n\ndeny contains rc.address if {\n\tsome rc in input.resource_changes\n\tnot rc.type in allowed\n\trc.type == \"tls_private_key\"\n}\n"),
				rules("team", "allowed contains \"tls_private_key\"\n\nwarn contains \"team\" if true\n"),
			},
			outcome: policy.Deny,
			results: []policy.Result{
				{Set: "server", Level: policy.Mandatory, Rule: "deny", Outcome: policy.Deny, Message: "tls_private_key.k"},
				{Set: "team", Level: policy.Mandatory, Rule: "warn", Outcome: policy.Warn, Message: "team"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decide(t, compile(t, c.sets...), planInput(t, "masked-create.json", "prod"))
			if d.Format != policy.DecisionFormat || d.Point != policy.PlanPoint || !d.At.Equal(wednesday) || len(d.Input) != 64 {
				t.Fatalf("decision %+v", d)
			}
			if d.Outcome != c.outcome || !slices.Equal(d.Results, c.results) && len(c.results)+len(d.Results) > 0 {
				t.Fatalf("got %s %+v\nwant %s %+v", d.Outcome, d.Results, c.outcome, c.results)
			}
		})
	}
}

func TestPolicyMessageBounds(t *testing.T) {
	e := compile(t, rules("s", fmt.Sprintf(`warn contains sprintf("%%s %%d", [long, i]) if {
	some i in [x | some x, _ in split(%q, "")]
}

long := concat("", [c | some _ in split(%q, ""); c := "x"])
`, strings.Repeat("y", 150), strings.Repeat("z", 1500))))
	d := decide(t, e, planInput(t, "masked-create.json", "dev"))
	if len(d.Results) != 101 || d.Outcome != policy.Warn {
		t.Fatalf("want 100 messages and one that counts the rest, got %d", len(d.Results))
	}
	if m := d.Results[0].Message; !strings.HasPrefix(m, strings.Repeat("x", 1500)+" ") {
		t.Fatalf("Decide cut a long message, which the caller must mask whole: %d bytes", len(m))
	}
	if m := d.Results[100].Message; m != "and 50 more messages of set s, cut" {
		t.Fatalf("the last result is %q", m)
	}
	d.CutMessages("(sensitive)")
	if m := d.Results[0].Message; m != strings.Repeat("x", 1000)+" (cut)" {
		t.Fatalf("a long message was not cut: %d bytes", len(m))
	}
	if m := d.Results[100].Message; m != "and 50 more messages of set s, cut" {
		t.Fatalf("a short message changed: %q", m)
	}
}

// A cut that falls inside a mask marker cuts before the marker, so a
// message never ends in a piece of one.
func TestCutMessagesKeepsMarkersWhole(t *testing.T) {
	for _, tt := range []struct{ name, message, want string }{
		{"inside", strings.Repeat("x", 995) + "(sensitive)" + strings.Repeat("y", 100), strings.Repeat("x", 995) + " (cut)"},
		{"ends at the cut", strings.Repeat("x", 989) + "(sensitive)y", strings.Repeat("x", 989) + "(sensitive) (cut)"},
		{"starts at the cut", strings.Repeat("x", 1000) + "(sensitive)", strings.Repeat("x", 1000) + " (cut)"},
		{"one inside, one before", strings.Repeat("x", 500) + "(sensitive)" + strings.Repeat("x", 486) + "(sensitive)", strings.Repeat("x", 500) + "(sensitive)" + strings.Repeat("x", 486) + " (cut)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := policy.Decision{Results: []policy.Result{{Message: tt.message}}}
			d.CutMessages("(sensitive)")
			if got := d.Results[0].Message; got != tt.want {
				t.Errorf("cut to %q, want %q", got[max(0, len(got)-40):], tt.want[max(0, len(tt.want)-40):])
			}
		})
	}
}

// Only pass and warn allow. Any other outcome, which no decision makes,
// denies.
func TestOutcomeDenies(t *testing.T) {
	for o, want := range map[policy.Outcome]bool{policy.Pass: false, policy.Warn: false, policy.Deny: true, "": true, "(sensitive)": true, "PASS": true} {
		if got := o.Denies(); got != want {
			t.Errorf("%q denies %t, want %t", o, got, want)
		}
	}
}

func TestPolicyCompileRefusals(t *testing.T) {
	cases := []struct {
		name string
		sets []policy.Set
		want string
	}{
		{"no rule Writ reads", []policy.Set{rules("s", "allow if true\n")}, "policy set s: defines no deny, violation, or warn rule in package main"},
		{"rules in another package only", []policy.Set{set("s", "", "p.rego", "package policy\n\ndeny contains \"x\" if true\n")}, "defines no deny"},
		{"an exception", []policy.Set{rules("s", "deny contains \"x\" if true\n\nexception contains \"x\" if true\n")}, "p.rego:5:1: writ serve reads no exception rules"},
		{"print", []policy.Set{rules("s", "deny contains \"x\" if print(\"a\")\n")}, "p.rego:3:22: rego_type_error: undefined function print"},
		{"a syntax error", []policy.Set{rules("s", "deny contains if\n")}, "policy set s: p.rego:3:"},
		{"a key the masked document drops", []policy.Set{rules("s", "deny contains r.type if {\n\tsome r in input.planned_values.root_module.resources\n}\n")}, "undefined ref: input.planned_values"},
		{"a misspelled key of a change", []policy.Set{rules("s", "deny contains r.address if {\n\tsome r in input.resource_changes\n\tr.change.action == \"create\"\n}\n")}, "p.rego:5:2: rego_type_error: undefined ref: r.change.action"},
		{"a key input.writ lacks", []policy.Set{rules("s", "deny contains \"x\" if input.writ.env == \"prod\"\n")}, "undefined ref: input.writ.env"},
		{"two sets of one name", []policy.Set{rules("s", "deny contains \"x\" if false\n"), rules("s", "deny contains \"x\" if false\n")}, "two policy sets are named s"},
		{"a level Writ does not know", []policy.Set{set("s", "soft-mandatory", "p.rego", "package main\n\ndeny contains \"x\" if false\n")}, "has level \"soft-mandatory\"; use mandatory or advisory"},
		{"a set past 1 MiB", []policy.Set{rules("s", "deny contains \"x\" if false\n# "+strings.Repeat("x", 1<<20)+"\n")}, "more than the 1 MiB limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := policy.Compile(c.sets, policy.Options{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

func TestPolicyEvalErrors(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"a deny that is not a set", "deny := 1\n", "deny is 1; it must be a set of strings"},
		{"a message that is not a string", "deny contains 7 if true\n", "deny holds 7; each message must be a string"},
		{"an object without msg", "deny contains {\"text\": \"x\"} if true\n", "each message must be a string, or an object whose msg is a string"},
		{"a built-in error, which conftest reads as no message", "deny contains \"x\" if to_number(\"seven\") > 1\n", "to_number"},
		{"two values of one complete rule", "warn := {\"a\"} if true\n\nwarn := {\"b\"} if true\n", "conflict"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, level := range []policy.Level{policy.Mandatory, policy.Advisory} {
				d := decide(t, compile(t, set("s", level, "p.rego", "package main\n\n"+c.body)), planInput(t, "masked-create.json", "dev"))
				want := map[policy.Level]policy.Outcome{policy.Mandatory: policy.Deny, policy.Advisory: policy.Warn}[level]
				errs := messages(d, "error")
				if d.Outcome != want || len(errs) != 1 || !strings.Contains(errs[0], c.want) {
					t.Fatalf("%s: got %s %+v, want one error with %q and %s", level, d.Outcome, d.Results, c.want, want)
				}
			}
		})
	}
}

// TestPolicyPinRegressions holds the two rule forms that OPA v1.21.0 broke:
// a membership in a set that holds an input value never matched until
// v1.22.1, and `some x in []` failed to compile until v1.22.2. It holds
// too the substring whose start plus length overflows, which panicked
// before v1.22.3.
func TestPolicyPinRegressions(t *testing.T) {
	e := compile(t, rules("s", `deny contains "member" if input.writ.environment in {"qa", input.writ.stack}

deny contains "empty" if {
	some x in []
	x == 1
}

deny contains "none" if {
	not any_in_empty
}

any_in_empty if {
	some x in []
	x == 1
}

deny contains "substring" if substring("abc", 1, 9223372036854775807) == "bc"
`))
	d := decide(t, e, planInput(t, "masked-create.json", "network"))
	if got := strings.Join(messages(d, "deny"), ","); got != "member,none,substring" || len(messages(d, "error")) != 0 {
		t.Fatalf("got denies %q, results %+v, want member,none,substring", got, d.Results)
	}
}

func TestPolicyInput(t *testing.T) {
	w := policy.Writ{Decision: policy.PlanPoint, Now: wednesday, Environment: "prod", Stack: "network", Actor: policy.Actor{Person: "p", Display: "P", Kind: "person"}}
	in, err := policy.NewInput(fixture(t, "masked-create.json"), w)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	b, _ := json.Marshal(in)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, "masked-create.json"), &doc); err != nil {
		t.Fatal(err)
	}
	for k, v := range doc {
		if !jsonEqual(t, got[k], v) {
			t.Errorf("the input changed the document's key %s", k)
		}
	}
	want := `{"decision":"plan","now":"2026-11-11T05:00:00Z","environment":"prod","stack":"network","actor":{"person":"p","display":"P","kind":"person","teams":[]},"approvals":[]}`
	if string(got["writ"]) != want || len(got) != len(doc)+1 {
		t.Fatalf("input.writ is %s, want %s", got["writ"], want)
	}

	raw, err := os.ReadFile("../mask/testdata/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	refusals := []struct {
		name string
		doc  []byte
		w    policy.Writ
		want string
	}{
		{"the raw output of tofu show -json", raw, w, "so it is not a masked plan document"},
		{"a document with input.writ", []byte(`{"format_version":"1.2","resource_changes":[],"writ":{}}`), w, `the key "writ"`},
		{"a plan that errored", []byte(`{"format_version":"1.2","resource_changes":[],"errored":true}`), w, "the plan errored"},
		{"no resource_changes", []byte(`{"format_version":"1.2"}`), w, "no resource_changes"},
		{"approvals at the plan point", fixture(t, "masked-create.json"), policy.Writ{Decision: policy.PlanPoint, Now: wednesday, Environment: "prod", Stack: "s", Approvals: []policy.Approval{{Person: "a"}}}, "the plan point has no approvals"},
		{"no decision point", fixture(t, "masked-create.json"), policy.Writ{Now: wednesday, Environment: "prod", Stack: "s"}, "not plan or apply"},
		{"past 16 MiB", []byte(`{"format_version":"1.2","resource_changes":[],"checks":"` + strings.Repeat("x", 16<<20) + `"}`), w, "larger than the 16 MiB limit"},
	}
	for _, r := range refusals {
		t.Run(r.name, func(t *testing.T) {
			if _, err := policy.NewInput(r.doc, r.w); err == nil || !strings.Contains(err.Error(), r.want) {
				t.Fatalf("got %v, want an error with %q", err, r.want)
			}
		})
	}
}

// TestPolicyInputKeysAreTheMasks keeps the keys NewInput accepts equal to
// the masked plan document's.
func TestPolicyInputKeysAreTheMasks(t *testing.T) {
	typ := reflect.TypeFor[mask.Plan]()
	var keys []string
	for i := range typ.NumField() {
		keys = append(keys, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	doc := map[string]any{}
	for _, k := range keys {
		doc[k] = nil
	}
	doc["format_version"], doc["resource_changes"], doc["errored"] = "1.2", []any{}, false
	b, _ := json.Marshal(doc)
	w := policy.Writ{Decision: policy.PlanPoint, Now: wednesday, Environment: "e", Stack: "s"}
	if _, err := policy.NewInput(b, w); err != nil {
		t.Fatalf("a document with every key of mask.Plan: %v", err)
	}
}

// TestPolicyDecisionPoint judges one rule at both points: input.writ
// tells them apart, and under conftest it is absent.
func TestPolicyDecisionPoint(t *testing.T) {
	e := compile(t, rules("s", `deny contains sprintf("%s by %s with %d approvals", [input.writ.decision, input.writ.actor.display, count(input.writ.approvals)]) if true
`))
	if got := messages(decide(t, e, planInput(t, "masked-create.json", "prod")), "deny"); !slices.Equal(got, []string{"plan by Person A with 0 approvals"}) {
		t.Fatalf("plan point: %q", got)
	}
	if got := messages(decide(t, e, applyInput(t, "masked-create.json", "prod", wednesday)), "deny"); !slices.Equal(got, []string{"apply by Person A with 1 approvals"}) {
		t.Fatalf("apply point: %q", got)
	}
}

// TestPolicyExamples judges the bundled examples on the masked documents
// of two real OpenTofu plans: one that creates nine resources, and one
// that replaces the four local_file resources after a sensitive variable
// changed.
func TestPolicyExamples(t *testing.T) {
	files, err := policy.LoadDir("../../examples/policy")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["policy_test.rego"]; ok {
		t.Fatal("LoadDir read a test file")
	}
	e := compile(t, policy.Set{Name: "examples", Files: files})
	perm := func(addr string) string {
		return addr + ": file_permission is 0777; set 0600, so only its owner reads it"
	}
	locals := []string{"local_file.encoded", "local_file.json", "local_file.plain", "local_file.wrapped"}
	var create, replace []string
	for _, a := range locals {
		create = append(create, perm(a))
		// A replace creates the file again.
		replace = append(replace, a+" is replaced: OpenTofu deletes it and creates it again", perm(a))
	}
	cases := []struct {
		name    string
		in      policy.Input
		outcome policy.Outcome
		want    []string
	}{
		{"create", planInput(t, "masked-create.json", "prod"), policy.Warn, create},
		{"replace", planInput(t, "masked-replace.json", "prod"), policy.Warn, replace},
		{"replace, applied in the window", applyInput(t, "masked-replace.json", "prod", time.Date(2026, 11, 11, 4, 0, 0, 0, time.UTC)), policy.Deny,
			append(slices.Clone(replace), "no applies to prod from 09:00 to 16:00 Asia/Kolkata, Monday to Friday")},
		{"replace, applied after the window", applyInput(t, "masked-replace.json", "prod", time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC)), policy.Warn, replace},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decide(t, e, c.in)
			got := messages(d, "")
			slices.Sort(got)
			want := slices.Sorted(slices.Values(c.want))
			if d.Outcome != c.outcome || !slices.Equal(got, want) {
				t.Fatalf("got %s %q\nwant %s %q", d.Outcome, got, c.outcome, want)
			}
		})
	}
}

// TestPolicyWindowRule is the owner's rule, in examples/policy/window.rego.
func TestPolicyWindowRule(t *testing.T) {
	files, err := policy.LoadDir("../../examples/policy")
	if err != nil {
		t.Fatal(err)
	}
	e := compile(t, policy.Set{Name: "examples", Files: files})
	cases := []struct {
		name, env, now string
		deny           bool
	}{
		{"08:59:59 on a Wednesday", "prod", "2026-11-11T03:29:59Z", false},
		{"09:00:00 on a Wednesday", "prod", "2026-11-11T03:30:00Z", true},
		{"15:59:59 on a Wednesday", "prod", "2026-11-11T10:29:59Z", true},
		{"16:00:00 on a Wednesday", "prod", "2026-11-11T10:30:00Z", false},
		{"10:00 on a Saturday", "prod", "2026-11-14T04:30:00Z", false},
		{"Friday in UTC is Saturday in the zone", "prod", "2026-11-13T22:00:00Z", false},
		{"Sunday in UTC is Monday 09:30 in the zone", "prod", "2026-11-16T04:00:00Z", true},
		{"another env", "staging", "2026-11-11T05:00:00Z", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.now)
			if err != nil {
				t.Fatal(err)
			}
			d := decide(t, e, applyInput(t, "masked-create.json", c.env, now))
			denies := messages(d, "deny_window")
			if c.deny != (len(denies) == 1) || len(messages(d, "error")) != 0 {
				t.Fatalf("got %s %+v, want deny %v", d.Outcome, d.Results, c.deny)
			}
			if p := decide(t, e, planInput(t, "masked-create.json", c.env)); len(messages(p, "deny_window")) != 0 {
				t.Fatalf("the window denied at the plan point: %+v", p.Results)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
