package diag

import "testing"

func TestHasErrorsIgnoresWarnings(t *testing.T) {
	var l List
	l.Warnf(State, Span{}, "the state moved")
	if l.HasErrors() {
		t.Fatal("a warning alone must not count as an error")
	}
	l.Errorf(Usage, Span{}, "mismatch %d", 1)
	if !l.HasErrors() {
		t.Fatal("an error must count")
	}
	if l[1].Message != "mismatch 1" {
		t.Fatalf("Errorf formats its message, got %q", l[1].Message)
	}
}
