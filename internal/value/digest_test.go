package value_test

import (
	"bytes"
	"crypto/sha256"
	"math"
	"testing"

	"github.com/mr-karan/writ/internal/value"
)

var (
	keyA = []byte("0123456789abcdef0123456789abcdef")
	keyB = []byte("fedcba9876543210fedcba9876543210")
)

func mustCanonical(t *testing.T, v value.Value) []byte {
	t.Helper()
	b, err := value.Canonical(v)
	if err != nil {
		t.Fatalf("Canonical(%s): %v", v, err)
	}
	return b
}

func TestCanonicalRefusesValuesThatAreNotConcrete(t *testing.T) {
	tests := []struct {
		name    string
		v       value.Value
		wantErr string
	}{
		{"unknown", value.UnknownVal(false), "cannot encode an unknown value at the top level"},
		{"unknown in a list", list(num(1), value.UnknownVal(true)), "cannot encode an unknown value at [1]"},
		{"nested unknown", obj(map[string]value.Value{
			"subject": list(obj(map[string]value.Value{"common_name": value.UnknownVal(false)})),
		}), "cannot encode an unknown value at subject[0].common_name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := value.Canonical(tt.v)
			if err == nil {
				t.Fatalf("Canonical(%s) = %x, want an error", tt.v, b)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("error = %q, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestCanonicalDistinguishesValues(t *testing.T) {
	values := []struct {
		name string
		v    value.Value
	}{
		{"null", value.NullVal()},
		{"false", value.BoolVal(false)},
		{"true", value.BoolVal(true)},
		{"int 1", num(1)},
		{"int -1", num(-1)},
		{"float 1", value.FloatVal(1)},
		{"float 0.3", value.FloatVal(0.3)},
		{"float just above 0.3", value.FloatVal(math.Nextafter(0.3, 1))},
		{"float -0", value.FloatVal(math.Copysign(0, -1))},
		{"float 0", value.FloatVal(0)},
		{"string 1", str("1")},
		{"empty string", str("")},
		{"secret string 1", str("1").Secret()},
		{"empty list", list()},
		{"empty object", obj(nil)},
		{"secret empty list", list().Secret()},
		{"list ab,c", list(str("ab"), str("c"))},
		{"list a,bc", list(str("a"), str("bc"))},
		{"object a:bc", obj(map[string]value.Value{"a": str("bc")})},
		{"object ab:c", obj(map[string]value.Value{"ab": str("c")})},
		{"list of empty list", list(list())},
		{"list of null", list(value.NullVal())},
	}
	seen := map[string]string{}
	for _, tt := range values {
		b := string(mustCanonical(t, tt.v))
		if other, ok := seen[b]; ok {
			t.Fatalf("%s and %s have the same canonical encoding", tt.name, other)
		}
		seen[b] = tt.name
	}
}

func TestCanonicalAgreesWithEqual(t *testing.T) {
	pairs := []struct {
		name string
		a, b value.Value
	}{
		{"container mark covers leaves", list(str("a"), str("b")).Secret(), list(str("a").Secret(), str("b").Secret())},
		{"nested container marks", obj(map[string]value.Value{"p": list(str("x"))}).Secret(), obj(map[string]value.Value{"p": list(str("x").Secret())})},
		{"partial mark", list(str("a").Secret(), str("b")), list(str("a"), str("b")).Secret()},
		{"same object built twice", obj(map[string]value.Value{"b": num(2), "a": num(1)}), obj(map[string]value.Value{"a": num(1), "b": num(2)})},
		{"NaN", value.FloatVal(math.NaN()), value.FloatVal(math.NaN())},
		{"int and float", num(2), value.FloatVal(2)},
		{"empty secret list", list().Secret(), list()},
	}
	for _, tt := range pairs {
		t.Run(tt.name, func(t *testing.T) {
			sameBytes := bytes.Equal(mustCanonical(t, tt.a), mustCanonical(t, tt.b))
			if eq := value.Equal(tt.a, tt.b); eq != sameBytes {
				t.Fatalf("Equal = %v but canonical encodings equal = %v for %s and %s", eq, sameBytes, tt.a, tt.b)
			}
		})
	}
}

func TestHasherIsKeyed(t *testing.T) {
	h := value.NewHasher(keyA)
	secret := str("hunter2").Secret()
	stored := h.Sum(mustCanonical(t, secret))

	if stored != value.NewHasher(keyA).Sum(mustCanonical(t, secret)) {
		t.Fatal("the same key must give the same digest")
	}
	if stored == value.NewHasher(keyB).Sum(mustCanonical(t, secret)) {
		t.Fatal("a different key must give a different digest")
	}
	// Someone who reads the digest but not the key hashes guesses with the
	// public encoding. None of them, including the right one, may match.
	for _, guess := range []string{"password", "123456", "hunter2", "letmein"} {
		for _, v := range []value.Value{str(guess), str(guess).Secret()} {
			if sha256.Sum256(mustCanonical(t, v)) == stored {
				t.Fatalf("an unkeyed hash of the guess %q matches the stored digest", guess)
			}
		}
	}
}

func TestHasherFramesParts(t *testing.T) {
	h := value.NewHasher(keyA)
	if h.Sum([]byte("ab"), []byte("c")) == h.Sum([]byte("a"), []byte("bc")) {
		t.Fatal("moving bytes between parts must change the digest")
	}
	if h.Sum([]byte("abc")) == h.Sum([]byte("abc"), nil) {
		t.Fatal("an extra empty part must change the digest")
	}
}

func TestNewHasherPanicsWithoutKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("an empty key must panic")
		}
	}()
	value.NewHasher(nil)
}
