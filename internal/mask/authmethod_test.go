package mask

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The auth-method fixtures are a state and an update plan shaped like
// nomad_acl_auth_method of hashicorp/nomad 2.1.0: the provider marks
// config.oidc_client_secret, and token_locality, max_token_ttl, and
// token_name_format are settings of the tokens the method issues.
const authMethod = "nomad_acl_auth_method.sso"

// adoptThenPlan masks the auth-method plan as writ serve does after
// writ state adopt: the state's mask learns the secret set, and the plan's
// mask starts from it.
func adoptThenPlan(t *testing.T, state, plan []byte) (secrets []string, p *Plan) {
	t.Helper()
	adopt := New(nil)
	if _, err := adopt.Index(state, only([]string{"id"})); err != nil {
		t.Fatal(err)
	}
	p, err := New(adopt.Secrets()).Plan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return adopt.Secrets(), p
}

// withClientSecret returns the auth-method fixture named with every
// occurrence of the fixture's client secret replaced by secret.
func withClientSecret(t *testing.T, name, secret string) []byte {
	t.Helper()
	q, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.ReplaceAll(string(fixture(t, name)), `"fixture-client-secret-1"`, string(q)))
}

// TestPlanShowsTokenSettings checks that the settings of the tokens an auth
// method issues show in the plan: the secret set holds only what the state
// marks, and a name that describes a token, not one that holds it, is not
// a secret name.
func TestPlanShowsTokenSettings(t *testing.T) {
	secrets, p := adoptThenPlan(t, fixture(t, "auth-method-state.json"), fixture(t, "auth-method-plan.json"))
	if want := []string{"fixture-client-secret-1"}; !slices.Equal(secrets, want) {
		t.Errorf("secret set = %q, want %q", secrets, want)
	}
	c := change(t, p, authMethod)
	for _, tt := range []struct {
		name          string
		before, after string
	}{
		{"token_locality", "global", "global"},
		{"max_token_ttl", "8h", "12h"},
		{"token_name_format", "${auth_method_type}-${auth_method_name}", "${auth_method_type}-${auth_method_name}"},
	} {
		if got := attr(c.Before, tt.name); got != tt.before {
			t.Errorf("before %s = %v, want %q", tt.name, got, tt.before)
		}
		if got := attr(c.After, tt.name); got != tt.after {
			t.Errorf("after %s = %v, want %q", tt.name, got, tt.after)
		}
	}
	for _, v := range []any{c.Before, c.After} {
		cfg, _ := attr(v, "config").([]any)
		if len(cfg) != 1 || attr(cfg[0], "oidc_client_secret") != Sensitive {
			t.Errorf("config = %v, want oidc_client_secret %s", cfg, Sensitive)
		}
	}
	assertNoLeak(t, encode(t, p), "fixture-client-secret-1")
}

// TestPlanMasksTokenSettingSecrets checks the other direction: a client
// secret that equals a token setting, a common word, or a short value is
// still masked at its own path and wherever the sweep finds it, and an
// attribute whose name holds a token is still masked by its name.
func TestPlanMasksTokenSettingSecrets(t *testing.T) {
	for _, secret := range []string{"global", "8h", "${auth_method_type}-${auth_method_name}", "email"} {
		t.Run(secret, func(t *testing.T) {
			secrets, p := adoptThenPlan(t,
				withClientSecret(t, "auth-method-state.json", secret),
				withClientSecret(t, "auth-method-plan.json", secret))
			if !slices.Contains(secrets, secret) {
				t.Errorf("secret set = %q, want it to hold %q", secrets, secret)
			}
			doc := encode(t, p)
			if strings.Contains(doc, `"`+secret+`"`) {
				t.Errorf("%q shows as a whole value: %s", secret, doc)
			}
			if len(secret) >= minSubstring {
				assertNoLeak(t, doc, secret)
			}
		})
	}

	raw := strings.ReplaceAll(string(fixture(t, "auth-method-plan.json")),
		`"token_locality": "global",`,
		`"token_locality": "global", "api_token": "plain-but-named", "bootstrap_token_ttl_secret": "also-named", "tokenless": "named-too",`)
	p, err := New(nil).Plan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	after := change(t, p, authMethod).After
	for _, name := range []string{"api_token", "bootstrap_token_ttl_secret", "tokenless"} {
		if got := attr(after, name); got != Sensitive {
			t.Errorf("after %s = %v, want %s", name, got, Sensitive)
		}
	}
	if got := attr(after, "token_locality"); got != "global" {
		t.Errorf("after token_locality = %v, want global", got)
	}
}
