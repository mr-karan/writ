package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"

	jose "github.com/go-jose/go-jose/v4"
)

// Bounds on a key set. go-jose published GHSA-mgcc-cww5-fp2q on 2026-10-14
// for a denial of service when parsing JWKs with RSA keys, fixed in the
// v4.1.6 Writ pins; these bounds hold before go-jose sees a key.
const (
	MaxKeySetBytes = 1 << 20
	MaxKeys        = 100
	minRSABits     = 2048
	maxRSABits     = 8192
)

// key is one verification key from a key set.
type key struct {
	kid string
	alg Alg // "" when the key set names none
	// pub is an *rsa.PublicKey or an *ecdsa.PublicKey on P-256.
	pub crypto.PublicKey
}

// fits reports whether k may check a signature made with alg.
func (k key) fits(alg Alg) bool {
	if k.alg != "" && k.alg != alg {
		return false
	}
	switch k.pub.(type) {
	case *rsa.PublicKey:
		return alg == RS256
	case *ecdsa.PublicKey:
		return alg == ES256
	}
	return false
}

// jwk is the part of a key set member Writ reads before go-jose parses it.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
}

// parseKeySet reads a JWKS document. A key that is not for signatures, or
// of a type writ does not check, is skipped. A key writ would use that is
// malformed or out of bounds fails the whole set, as does a kid that two
// keys share, because then a token could pick either. A refusal names a key
// by its position, never by its kid.
func parseKeySet(body []byte) (map[string]key, *Refusal) {
	if len(body) > MaxKeySetBytes {
		return nil, refuse(StageKeySet, ReasonTooLarge).counted(int64(len(body)), MaxKeySetBytes)
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, refuse(StageKeySet, ReasonNotJSON)
	}
	if len(doc.Keys) > MaxKeys {
		return nil, refuse(StageKeySet, ReasonTooManyKeys).counted(int64(len(doc.Keys)), MaxKeys)
	}
	out := map[string]key{}
	seen := map[string]bool{}
	for i, raw := range doc.Keys {
		var j jwk
		if err := json.Unmarshal(raw, &j); err != nil {
			return nil, refuse(StageKeySet, ReasonKeyNotJWK).atKey(i + 1)
		}
		if j.Use != "" && j.Use != "sig" || j.Kid == "" {
			continue
		}
		if seen[j.Kid] {
			return nil, refuse(StageKeySet, ReasonKidTwice).atKey(i + 1)
		}
		seen[j.Kid] = true
		if !j.wanted() {
			continue
		}
		pub, why, bits := j.public(raw)
		if why != 0 {
			return nil, refuse(StageKeySet, why).atKey(i+1).counted(int64(bits), 0)
		}
		out[j.Kid] = key{kid: j.Kid, alg: Alg(j.Alg), pub: pub}
	}
	return out, nil
}

// wanted reports whether j is a key writ checks: RSA for RS256 or EC on
// P-256 for ES256. Others, such as OKP or PS256, are skipped.
func (j jwk) wanted() bool {
	switch j.Kty {
	case "RSA":
		return j.Alg == "" || j.Alg == string(RS256)
	case "EC":
		return (j.Alg == "" || j.Alg == string(ES256)) && j.Crv == "P-256"
	}
	return false
}

// public parses raw with go-jose and holds the key to Writ's bounds. On
// failure it returns why, and the modulus's bits for ReasonRSABits.
// go-jose's error text is dropped: it can quote the key.
func (j jwk) public(raw json.RawMessage) (pub crypto.PublicKey, why Reason, bits int) {
	if j.Kty == "RSA" {
		if len(j.N) > (maxRSABits/8)*4/3+4 {
			return nil, ReasonRSAModulusLong, 0
		}
		// AQAB is 65537, the only exponent writ accepts, in its one
		// minimal encoding.
		if j.E != "AQAB" {
			return nil, ReasonRSAExponent, 0
		}
	}
	var k jose.JSONWebKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, ReasonKeyUnparsable, 0
	}
	if !k.IsPublic() {
		return nil, ReasonPrivateKey, 0
	}
	switch pub := k.Key.(type) {
	case *rsa.PublicKey:
		if bits := pub.N.BitLen(); bits < minRSABits || bits > maxRSABits {
			return nil, ReasonRSABits, bits
		}
		if pub.N.Bit(0) == 0 {
			return nil, ReasonRSAEven, 0
		}
		return pub, 0, 0
	case *ecdsa.PublicKey:
		if pub.Curve != elliptic.P256() {
			return nil, ReasonECCurve, 0
		}
		return pub, 0, 0
	}
	return nil, ReasonKeyType, 0
}
