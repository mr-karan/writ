package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/mask"
)

// A known secret that itself holds a quoted list, as a jsonencode string
// does, is masked whole: the key sweep must not split it first.
func TestStepDiagnosticsMasksBeforeTheKeySweep(t *testing.T) {
	const secret = `{"hosts":["db1"],"password":"hunter2-hunter2-hunter2"}`
	summary := "expected length of secret_string to be in the range (1 - 10), got " + secret
	ev, _ := json.Marshal(map[string]any{"type": "diagnostic", "diagnostic": map[string]any{"severity": "error", "summary": summary}})
	for _, sweep := range []bool{true, false} {
		got := stepDiagnostics(mask.New([]string{secret}), append(ev, '\n'), sweep)
		if len(got) != 1 || strings.Contains(got[0].Message, "hunter2") || strings.Contains(got[0].Message, "db1") {
			t.Fatalf("sweepKeys %v: %+v", sweep, got)
		}
	}
}

// The key sweep covers an address quoted once, as OpenTofu writes it, and
// one quoted again inside a summary, as %q writes it.
func TestStepDiagnosticsSweepsQuotedKeys(t *testing.T) {
	for _, summary := range []string{
		`Cannot delete terraform_data.keyed["past-key"]`,
		`Cannot delete terraform_data.keyed["past\"key"]`,
		`reading "terraform_data.keyed[\"past-key\"]": not found`,
		`reading "terraform_data.keyed[\"past\\\"key\"]": not found`,
		`reading "terraform_data.keyed[\"past\\\\key\"]": not found`,
		`reading "terraform_data.keyed[\"{\\\"h\\\":[\\\"past-key\\\"]}\"]": not found`,
	} {
		ev, _ := json.Marshal(map[string]any{"type": "diagnostic", "diagnostic": map[string]any{"severity": "error", "summary": summary}})
		got := stepDiagnostics(mask.New(nil), append(ev, '\n'), true)
		if len(got) != 1 || strings.Contains(got[0].Message, "past") {
			t.Fatalf("%s: %+v", summary, got)
		}
	}
}
