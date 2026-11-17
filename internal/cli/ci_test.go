package cli

import "testing"

func TestHCLRoundTripsRefusesChangedNames(t *testing.T) {
	for _, s := range []string{"release-é", "bad\xff"} {
		if hclRoundTrips(s) {
			t.Errorf("hclRoundTrips(%q) = true; HCL would read back a different string", s)
		}
	}
	for _, s := range []string{"main", "release%{prod}", "émoji-🙂"} {
		if !hclRoundTrips(s) {
			t.Errorf("hclRoundTrips(%q) = false", s)
		}
	}
}
