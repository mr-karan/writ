package provider

import (
	"testing"

	"github.com/mr-karan/writ/internal/value"
)

func TestClearTransientStateKeepsMetadata(t *testing.T) {
	digest := value.Digest{1}
	before := value.ObjectVal(map[string]value.Value{"id": value.StringVal("job"), "plan_response": value.StringVal("transient")}).Secret().WithObservedDigests(map[string]value.Digest{"token": digest})
	after := ClearTransientState("nomad_job", before)
	if !after.IsSecret() || after.ObservedDigests()["token"] != digest {
		t.Fatal("clearing response lost state metadata")
	}
	if after.Get("plan_response").Kind() != value.Null {
		t.Fatal("response was retained")
	}
}
