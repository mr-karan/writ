package oidc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
)

func rsaJWK(t *testing.T, kid string, bits int) map[string]string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return map[string]string{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256", "e": enc(big.NewInt(65537).Bytes()), "n": enc(k.N.Bytes())}
}

func keySetJSON(keys ...map[string]string) []byte {
	b, _ := json.Marshal(map[string]any{"keys": keys})
	return b
}

func TestParseKeySet(t *testing.T) {
	good := rsaJWK(t, "a", 2048)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pt, _ := ec.PublicKey.Bytes()
	enc := base64.RawURLEncoding.EncodeToString
	ecJWK := map[string]string{"kty": "EC", "kid": "e", "crv": "P-256", "x": enc(pt[1:33]), "y": enc(pt[33:])}
	edit := func(base map[string]string, k, v string) map[string]string {
		out := map[string]string{}
		for a, b := range base {
			out[a] = b
		}
		out[k] = v
		return out
	}
	offCurve := edit(ecJWK, "y", enc(make([]byte, 32)))
	tests := []struct {
		name string
		body []byte
		kids []string // keys kept
		want Reason
	}{
		{"rsa and ec", keySetJSON(good, ecJWK), []string{"a", "e"}, 0},
		{"rsa 1024 bits", keySetJSON(rsaJWK(t, "small", 1024)), nil, ReasonRSABits},
		{"exponent 3", keySetJSON(edit(good, "e", enc([]byte{3}))), nil, ReasonRSAExponent},
		{"exponent with a leading zero", keySetJSON(edit(good, "e", enc([]byte{0, 1, 0, 1}))), nil, ReasonRSAExponent},
		{"modulus with a leading zero is the same key", keySetJSON(edit(good, "n", enc(append([]byte{0}, mustB64(good["n"])...)))), []string{"a"}, 0},
		{"modulus over 8192 bits", keySetJSON(edit(good, "n", enc(append([]byte{1}, make([]byte, 1100)...)))), nil, ReasonRSAModulusLong},
		{"ec point off the curve", keySetJSON(offCurve), nil, ReasonKeyUnparsable},
		// go-jose refuses an RSA private key without its primes before Writ
		// sees it; either way the set fails.
		{"a private key fails the set", keySetJSON(edit(good, "d", good["n"])), nil, ReasonKeyUnparsable},
		{"ec on P-384 is skipped", keySetJSON(edit(ecJWK, "crv", "P-384")), nil, 0},
		{"an encryption key is skipped", keySetJSON(edit(good, "use", "enc")), nil, 0},
		{"OKP is skipped", keySetJSON(map[string]string{"kty": "OKP", "kid": "o", "crv": "Ed25519", "x": "AA"}), nil, 0},
		{"RSA with alg PS256 is skipped", keySetJSON(edit(good, "alg", "PS256")), nil, 0},
		{"no kid is skipped", keySetJSON(edit(good, "kid", "")), nil, 0},
		{"two keys share a kid", keySetJSON(good, edit(ecJWK, "kid", "a")), nil, ReasonKidTwice},
		{"not json", []byte("<html>"), nil, ReasonNotJSON},
		{"too many keys", keySetJSON(manyKeys(MaxKeys + 1)...), nil, ReasonTooManyKeys},
		{"oversized body", append(keySetJSON(good), make([]byte, MaxKeySetBytes)...), nil, ReasonTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, ref := parseKeySet(tt.body)
			if tt.want != 0 {
				if ref == nil || ref.Reason() != tt.want || ref.Stage() != StageKeySet {
					t.Fatalf("got %v, want reason %d", ref, tt.want)
				}
				return
			}
			if ref != nil {
				t.Fatal(ref.Message())
			}
			if len(keys) != len(tt.kids) {
				t.Fatalf("kept %d keys, want %v", len(keys), tt.kids)
			}
			for _, kid := range tt.kids {
				if _, ok := keys[kid]; !ok {
					t.Fatalf("lost key %s", kid)
				}
			}
		})
	}
}

func manyKeys(n int) []map[string]string {
	out := make([]map[string]string, n)
	for i := range out {
		out[i] = map[string]string{"kty": "OKP", "kid": fmt.Sprint(i)}
	}
	return out
}

func mustB64(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
