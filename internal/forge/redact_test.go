package forge

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	const tok = "glpat-redact-0123456789"
	for _, tc := range []struct {
		name     string
		err      error
		want     string
		notFound bool
	}{
		{"no error", nil, "", false},
		{"no token", errors.New("read project acme/infra: timeout"), "read project acme/infra: timeout", false},
		{"the token twice", fmt.Errorf("%s and %s", tok, tok), "[REDACTED] and [REDACTED]", false},
		{"not found keeps its meaning", fmt.Errorf("read %s: %w", tok, ErrNotFound), "read [REDACTED]: not found", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redact(tc.err, tok)
			if got == nil {
				if tc.err != nil {
					t.Fatal("redact dropped the error")
				}
				return
			}
			if got.Error() != tc.want || errors.Is(got, ErrNotFound) != tc.notFound {
				t.Fatalf("got %q (not found %v)", got, errors.Is(got, ErrNotFound))
			}
			for e := got; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), tok) {
					t.Fatalf("an unwrapped error holds the token: %v", e)
				}
			}
		})
	}
}
