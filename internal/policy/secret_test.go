package policy_test

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/policy"
)

// canaries are the secret values of internal/mask/testdata/stack, whose
// two recorded plans the fixtures mask: both values of pw, the defaults
// of fallback, the value of label that the run's environment names a
// secret, owner under a secret name, and pin.
var canaries = []string{"wc+pw/7f3a&9c2e41", "wc+pw/b19d&e0c5a2", "wc-default-91ad5e", "wc-fallback-0e7b2d", "wc-label-5b8e0d", "wc-owner-44e1a7", "739201"}

func leaks(s string) []string {
	var found []string
	for _, c := range canaries {
		for _, form := range []string{c, base64.StdEncoding.EncodeToString([]byte(c)), hex.EncodeToString([]byte(c)), url.QueryEscape(c)} {
			if strings.Contains(s, form) {
				found = append(found, form)
			}
		}
	}
	return found
}

// TestPolicyCanaryScan gives a policy every value of the input: each
// attribute before and after of every change, every output, and every
// variable's name. The masked document holds no secret, so neither does
// any input, message, or decision.
func TestPolicyCanaryScan(t *testing.T) {
	e := compile(t, rules("dump", `warn contains sprintf("%s %s %s %v %v", [rc.address, k, json.marshal(rc.change.actions), object.get(before(rc), k, null), v]) if {
	some rc in input.resource_changes
	is_object(rc.change.after)
	some k, v in rc.change.after
}

before(rc) := b if {
	b := rc.change.before
	is_object(b)
} else := {}

warn contains sprintf("%s %v", [rc.address, rc.change.before]) if {
	some rc in input.resource_changes
	not is_object(rc.change.after)
}

warn contains sprintf("output %s %v %v", [name, c.before, c.after]) if {
	some name, c in input.output_changes
}

warn contains sprintf("variable %s", [name]) if {
	some name in input.variables
}

deny contains json.marshal([rc.address, rc.change.after_sensitive, rc.change.changed_sensitive_paths]) if {
	some rc in input.resource_changes
}
`))
	for _, doc := range []string{"masked-create.json", "masked-replace.json"} {
		t.Run(doc, func(t *testing.T) {
			if found := leaks(string(fixture(t, doc))); len(found) > 0 {
				t.Fatalf("the fixture holds %q", found)
			}
			for _, in := range []policy.Input{planInput(t, doc, "prod"), applyInput(t, doc, "prod", wednesday)} {
				b, _ := json.Marshal(in)
				d := decide(t, e, in)
				if len(messages(d, "warn")) < 20 || len(messages(d, "error")) != 0 {
					t.Fatalf("the dump gave %d messages: %+v", len(d.Results), d.Results)
				}
				out, _ := json.Marshal(d)
				if found := leaks(string(b) + string(out)); len(found) > 0 {
					t.Fatalf("the input or its decision holds %q", found)
				}
				if !strings.Contains(string(out), "(sensitive)") {
					t.Fatal("no message shows a masked value, so the dump read nothing under a mark")
				}
			}
		})
	}
}
