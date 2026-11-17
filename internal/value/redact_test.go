package value_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/value"
)

func TestRedactor(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\nMIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQg\n-----END PRIVATE KEY-----\n"
	r := value.Redactor{}.With(
		value.ObjectVal(map[string]value.Value{"private_key_pem": value.StringVal(pem).Secret(), "public": value.StringVal("not-secret")}),
		value.ListVal([]value.Value{value.StringVal("hunter2-token")}).Secret(),
	)
	err := errors.New("bad key: " + pem + " line MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQg; token hunter2-token; public not-secret")
	got := r.Error(err).Error()
	for _, leak := range []string{"MIGHAgEAMBMGByqGSM49", "hunter2-token"} {
		if strings.Contains(got, leak) {
			t.Errorf("redacted error still holds %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "not-secret") || !strings.Contains(got, "bad key") {
		t.Errorf("redaction removed text that is not secret: %s", got)
	}
	if r.Error(nil) != nil {
		t.Error("redacting nil gave an error")
	}
}

func TestDeniedName(t *testing.T) {
	for name, want := range map[string]bool{
		"DB_Password": true, "api-token": true, "nomad_secret_id": true, "private_key": true, "user_data": true,
		"image": false, "replicas": false, "tokenless": true, "": false,
	} {
		if got := value.DeniedName(name); got != want {
			t.Errorf("DeniedName(%q) = %v, want %v", name, got, want)
		}
	}
}
