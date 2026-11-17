package importer

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"testing"
)

// TestCompareKeysIsATotalOrder checks compareKeys on random sets of keys
// that mix numbers, numbers with leading zeros or signs, and other text:
// it is antisymmetric and transitive, and sorting gives one order for
// every permutation of the input.
func TestCompareKeysIsATotalOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(20, 2))
	const alphabet = "0129a-+"
	key := func() string {
		b := make([]byte, rng.IntN(4))
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		return string(b)
	}
	sign := func(x int) int { return cmp.Compare(x, 0) }
	for round := range 300 {
		keys := []string{"2", "10", "1a", "01", "1"}
		for range rng.IntN(12) {
			keys = append(keys, key())
		}
		for _, a := range keys {
			if compareKeys(a, a) != 0 {
				t.Fatalf("compareKeys(%q, %q) != 0", a, a)
			}
			for _, b := range keys {
				if sign(compareKeys(a, b)) != -sign(compareKeys(b, a)) {
					t.Fatalf("compareKeys is not antisymmetric on %q and %q", a, b)
				}
				for _, c := range keys {
					if compareKeys(a, b) < 0 && compareKeys(b, c) < 0 && compareKeys(a, c) >= 0 {
						t.Fatalf("compareKeys is not transitive: %q < %q < %q but not %q < %q", a, b, c, a, c)
					}
				}
			}
		}
		want := slices.Clone(keys)
		slices.SortFunc(want, compareKeys)
		for range 5 {
			got := slices.Clone(keys)
			rng.Shuffle(len(got), func(i, j int) { got[i], got[j] = got[j], got[i] })
			slices.SortFunc(got, compareKeys)
			if !slices.Equal(got, want) {
				t.Fatalf("round %d: sorting %q gave %q and %q", round, keys, want, got)
			}
		}
	}
	got := []string{"1a", "10", "2", "b", "01", "1"}
	slices.SortFunc(got, compareKeys)
	if want := []string{"01", "1", "2", "10", "1a", "b"}; !slices.Equal(got, want) {
		t.Errorf("sorted %q, want %q", got, want)
	}
}
