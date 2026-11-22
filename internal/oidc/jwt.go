// Package oidc checks OIDC tokens for writ serve and signs people in with
// the team's identity provider. It accepts one form of token, a compact JWS
// signed with RS256 or ES256, and checks it in a fixed order: shape,
// header, issuer, signature, times, audience, and the claims the issuer's
// kind needs.
//
// The protocols come from libraries: github.com/coreos/go-oidc/v3 verifies
// ID tokens, github.com/go-jose/go-jose/v4 parses keys and signatures, and
// golang.org/x/oauth2 runs the authorization code flow with PKCE and the
// device flow. This package adds Writ's policy around them: a size limit
// and a header screen before any library sees a token, one key set cache
// per issuer with bounds, and the audience, time, and claim rules after.
//
// Verify returns a Token whose claims have passed every check. Nothing past
// it sees the raw token. A failure is a *Refusal, which holds no text from
// the token or the issuer.
package oidc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"strings"
	"time"
)

// MaxTokenBytes bounds a token before any part of it is decoded.
const MaxTokenBytes = 16 << 10

// Leeway is the clock skew allowed on exp, nbf, and iat. RFC 7519 allows
// "some small leeway, usually no more than a few minutes".
const Leeway = 60 * time.Second

// Alg is a signature algorithm Verify accepts.
type Alg string

const (
	RS256 Alg = "RS256"
	ES256 Alg = "ES256"
)

// forbiddenHeader lists header parameters that would let a token name its
// own key, change how its payload is read, or carry JWE fields.
var forbiddenHeader = []Field{FieldJku, FieldJwk, FieldX5u, FieldX5c, FieldCrit, FieldB64, FieldEnc, FieldZip, FieldEpk}

// screened is a token that passed the screen, before its signature is
// checked. Its claims serve only to pick the issuer.
type screened struct {
	claims map[string]json.RawMessage
}

var b64 = base64.RawURLEncoding.Strict()

// screen checks the shape and the header of raw and decodes its claims. It
// checks no signature: it refuses what Writ never accepts before a library
// parses the token, so a parser that reads a token two ways never sees one.
func screen(raw string) (*screened, *Refusal) {
	if len(raw) > MaxTokenBytes {
		return nil, failed(CheckShape, ReasonTokenTooLong).counted(int64(len(raw)), MaxTokenBytes)
	}
	parts := strings.Split(raw, ".")
	switch len(parts) {
	case 3:
	case 5:
		return nil, failed(CheckShape, ReasonEncrypted)
	default:
		return nil, failed(CheckShape, ReasonParts).counted(int64(len(parts)), 3)
	}
	head, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, failed(CheckShape, ReasonNotBase64).of(FieldHeader)
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, failed(CheckShape, ReasonNotBase64).of(FieldPayload)
	}
	if _, err := b64.DecodeString(parts[2]); err != nil {
		return nil, failed(CheckShape, ReasonNotBase64).of(FieldSignature)
	}
	h, why := object(head)
	if why != 0 {
		return nil, failed(CheckHeader, why).of(FieldHeader)
	}
	for _, f := range forbiddenHeader {
		if _, ok := h[f.Name()]; ok {
			return nil, failed(CheckHeader, ReasonForbiddenHeader).of(f)
		}
	}
	var alg, kid string
	if why := stringField(h, FieldAlg, &alg); why != 0 {
		return nil, failed(CheckHeader, why).of(FieldAlg)
	}
	switch Alg(alg) {
	case RS256, ES256:
	default:
		return nil, failed(CheckHeader, ReasonAlg)
	}
	if why := stringField(h, FieldKid, &kid); why != 0 {
		return nil, failed(CheckHeader, why).of(FieldKid)
	}
	if kid == "" || len(kid) > 256 {
		return nil, failed(CheckHeader, ReasonKidLength)
	}
	claims, why := object(payload)
	if why != 0 {
		return nil, failed(CheckShape, why).of(FieldPayload)
	}
	return &screened{claims: claims}, nil
}

// Screen checks a token's shape and header as Verify does first, with no
// key and no signature check: a compact JWS of three base64url parts, a
// JSON header with alg RS256 or ES256 and a kid, and a JSON payload. A
// client checks a token it was handed with it before it sends it on; only
// writ serve checks the signature.
func Screen(raw string) *Refusal {
	_, r := screen(raw)
	return r
}

// stringField reads a required string member of a decoded object, and
// returns 0, ReasonMissing, or ReasonNotString.
func stringField(obj map[string]json.RawMessage, f Field, out *string) Reason {
	raw, ok := obj[f.Name()]
	if !ok {
		return ReasonMissing
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return ReasonNotString
	}
	return 0
}

// maxMembers bounds the members of a header or a claims object.
const maxMembers = 256

// object decodes one JSON object and refuses a repeated member name, so
// two parsers can never read different values from one token. It returns
// 0, ReasonNotObject, ReasonMemberTwice, or ReasonTooManyMembers.
func object(b []byte) (map[string]json.RawMessage, Reason) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, ReasonNotObject
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, ReasonNotObject
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, ReasonNotObject
		}
		name, ok := tok.(string)
		if !ok {
			return nil, ReasonNotObject
		}
		if _, dup := out[name]; dup {
			return nil, ReasonMemberTwice
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, ReasonNotObject
		}
		out[name] = v
		if len(out) > maxMembers {
			return nil, ReasonTooManyMembers
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, ReasonNotObject
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ReasonNotObject
	}
	return out, 0
}

// numericDate reads a NumericDate claim: seconds since the epoch, finite,
// after 1970 and before the year 5000. why is 0, ReasonNotNumber, or
// ReasonNotDate.
func numericDate(claims map[string]json.RawMessage, f Field) (t time.Time, ok bool, why Reason) {
	raw, ok := claims[f.Name()]
	if !ok {
		return time.Time{}, false, 0
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return time.Time{}, true, ReasonNotNumber
	}
	v, err := n.Float64()
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 95617584000 {
		return time.Time{}, true, ReasonNotDate
	}
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), true, 0
}
