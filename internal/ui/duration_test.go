package ui

import "testing"

func TestShortDuration(t *testing.T) {
	for in, want := range map[string]string{"24h0m0s": "24h", "12h0m0s": "12h", "1m0s": "1m", "1h30m0s": "1h30m", "1m30s": "1m30s", "45s": "45s", "0s": "0s", "soon": "soon"} {
		if got := shortDuration(in); got != want {
			t.Errorf("shortDuration(%q) = %q, want %q", in, got, want)
		}
	}
}
