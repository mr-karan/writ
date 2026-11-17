package tfstate

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// A sealed saved plan opens only under its own key, for its own plan id,
// environment, and stack, and a sealed state never opens as a saved plan,
// nor a saved plan as a state.
func TestSavedPlanSeal(t *testing.T) {
	key, err := NewKey(bytes.Repeat([]byte{1}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewKey(bytes.Repeat([]byte{2}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	saved := []byte("a saved plan with canary-7e1f")
	sealed := key.SealPlan(saved, "id1", "prod", "app")
	if bytes.Contains(sealed, []byte("canary-7e1f")) {
		t.Fatal("the sealed plan holds its plain text")
	}
	if got, err := key.OpenPlan(sealed, "id1", "prod", "app"); err != nil || !bytes.Equal(got, saved) {
		t.Fatalf("open: %q, %v", got, err)
	}
	for name, open := range map[string]func() ([]byte, error){
		"another key":   func() ([]byte, error) { return other.OpenPlan(sealed, "id1", "prod", "app") },
		"another plan":  func() ([]byte, error) { return key.OpenPlan(sealed, "id2", "prod", "app") },
		"another env":   func() ([]byte, error) { return key.OpenPlan(sealed, "id1", "dev", "app") },
		"another stack": func() ([]byte, error) { return key.OpenPlan(sealed, "id1", "prod", "net") },
		"as a state":    func() ([]byte, error) { return key.OpenState(sealed, "prod", "app", 1) },
		"a state":       func() ([]byte, error) { return key.OpenPlan(key.seal(saved, "prod", "app", 1), "id1", "prod", "app") },
		"too short":     func() ([]byte, error) { return key.OpenPlan(sealed[:4], "id1", "prod", "app") },
	} {
		if got, err := open(); err == nil {
			t.Errorf("%s opened: %q", name, got)
		}
	}
}

// Sealed index secrets open, each byte as it was, only under their own
// key, for their own version of their own stack@env, and never as a
// state or a saved plan.
func TestIndexSecretsSeal(t *testing.T) {
	key, err := NewKey(bytes.Repeat([]byte{1}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewKey(bytes.Repeat([]byte{2}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{"canary-7e1f", "with\x00nul", string([]byte{0xff, 0xfe}), strings.Repeat("x", 300)}
	sealed := key.SealIndexSecrets(secrets, "prod", "app", 3)
	if bytes.Contains(sealed, []byte("canary-7e1f")) {
		t.Fatal("the sealed secrets hold their plain text")
	}
	if got, err := key.OpenIndexSecrets(sealed, "prod", "app", 3); err != nil || !slices.Equal(got, secrets) {
		t.Fatalf("open: %q, %v", got, err)
	}
	if got, err := key.OpenIndexSecrets(key.SealIndexSecrets(nil, "prod", "app", 3), "prod", "app", 3); err != nil || len(got) != 0 {
		t.Fatalf("open of none: %q, %v", got, err)
	}
	for name, open := range map[string]func() (any, error){
		"another key":     func() (any, error) { return other.OpenIndexSecrets(sealed, "prod", "app", 3) },
		"another version": func() (any, error) { return key.OpenIndexSecrets(sealed, "prod", "app", 2) },
		"another env":     func() (any, error) { return key.OpenIndexSecrets(sealed, "dev", "app", 3) },
		"another stack":   func() (any, error) { return key.OpenIndexSecrets(sealed, "prod", "net", 3) },
		"as a state":      func() (any, error) { return key.OpenState(sealed, "prod", "app", 3) },
		"as a saved plan": func() (any, error) { return key.OpenPlan(sealed, "id1", "prod", "app") },
		"a state": func() (any, error) {
			return key.OpenIndexSecrets(key.seal([]byte("x"), "prod", "app", 3), "prod", "app", 3)
		},
		"too short": func() (any, error) { return key.OpenIndexSecrets(sealed[:4], "prod", "app", 3) },
	} {
		if got, err := open(); err == nil {
			t.Errorf("%s opened: %q", name, got)
		}
	}
}
