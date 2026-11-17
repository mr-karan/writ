package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// Kind is what an issuer's tokens speak for.
type Kind string

const (
	// GitLabCI is a GitLab instance that issues CI jobs' id_tokens.
	GitLabCI Kind = "gitlab-ci"
	// GitHubActions is GitHub Actions' token issuer.
	GitHubActions Kind = "github-actions"
	// People is the team's identity provider, which people sign in with.
	// Its ID tokens never reach Verify: a sign-in turns one into a session.
	People Kind = "people"
)

// ParseKind reads an issuer kind from config, and reports whether s is
// one of gitlab-ci, github-actions, and people.
func ParseKind(s string) (Kind, bool) {
	switch k := Kind(s); k {
	case GitLabCI, GitHubActions, People:
		return k, true
	}
	return "", false
}

// required lists the claims each kind's tokens must carry as strings, from
// GitLab's and GitHub's documents, read 2026-11-10. sub and jti are
// required of every kind.
var required = map[Kind][]Field{
	GitLabCI: {
		FieldProjectID, FieldProjectPath, FieldNamespaceID, FieldPipelineID, FieldPipelineSource,
		FieldJobID, FieldRefPath, FieldRefProtected, FieldSha, FieldUserID, FieldUserLogin,
	},
	GitHubActions: {
		FieldRepository, FieldRepositoryID, FieldRepositoryOwnerID, FieldRunID, FieldRef, FieldSha,
		FieldEventName, FieldWorkflowRef, FieldActor, FieldActorID,
	},
}

// Issuer is one issuer writ serve trusts.
type Issuer struct {
	// Name is the config's name for the issuer, such as gitlab.
	Name string
	Kind Kind
	// URL is the issuer, compared with a token's iss exactly. It must be
	// HTTPS.
	URL string
	// Audience is the only aud a token may carry: writ serve's own URL.
	Audience string
	// Client fetches discovery and key sets. Nil means a client that trusts
	// the system roots. Verifier refuses redirects on it either way.
	Client *http.Client
}

// Options configures New.
type Options struct {
	Now func() time.Time
	Log *slog.Logger
}

// Verifier checks tokens of the issuers added to it.
type Verifier struct {
	issuers map[string]*issuer // by URL
	now     func() time.Time
	log     *slog.Logger
}

type issuer struct {
	Issuer
	keys *keySet
}

// New returns a verifier that trusts no issuer yet.
func New(opts Options) *Verifier {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	return &Verifier{issuers: map[string]*issuer{}, now: opts.Now, log: opts.Log}
}

// Add fetches is's discovery document and key set and trusts its tokens
// from then on. It fails when either cannot be fetched, so a wrong URL
// shows at start. The refusal does not name the issuer: the caller knows
// which one it added.
func (v *Verifier) Add(ctx context.Context, is Issuer) *Refusal {
	if is.Kind == People {
		return refuse(StageConfig, ReasonPeopleIssuer)
	}
	if !issuerURL(is.URL) {
		return refuse(StageConfig, ReasonIssuerURL)
	}
	if _, dup := v.issuers[is.URL]; dup {
		return refuse(StageConfig, ReasonDuplicateURL)
	}
	client := noRedirects(is.Client)
	doc, ref := discover(ctx, client, is.URL)
	if ref != nil {
		return ref
	}
	ks := &keySet{issuer: is.Name, jwksURL: doc.JWKSURI, client: client, now: v.now, log: v.log}
	if ref := ks.refresh(ctx); ref != nil {
		return ref
	}
	v.issuers[is.URL] = &issuer{Issuer: is, keys: ks}
	return nil
}

// httpsURL parses s as an HTTPS URL with a host and no user: the least
// that every address an issuer or Writ's config names must be.
func httpsURL(s string) (*url.URL, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, false
	}
	return u, true
}

// issuerURL reports whether s is an HTTPS URL with no user, query, or
// fragment.
func issuerURL(s string) bool {
	u, ok := httpsURL(s)
	return ok && u.RawQuery == "" && u.Fragment == ""
}

// maxAnswer bounds every answer an issuer sends. The libraries read whole
// bodies, so the bound lives in the client's transport.
const maxAnswer = 1 << 20

// noRedirects returns a copy of c that refuses redirects, so a discovery
// document, a key set, or a token is always read from the address config
// or discovery names, and that cuts every answer at maxAnswer bytes.
func noRedirects(c *http.Client) *http.Client {
	out := &http.Client{}
	if c != nil {
		*out = *c
	}
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirects are refused") }
	if out.Timeout == 0 || out.Timeout > FetchTimeout {
		out.Timeout = FetchTimeout
	}
	base := out.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out.Transport = boundedTransport{base: base}
	return out
}

// boundedTransport cuts each answer's body at maxAnswer bytes: a read
// past it fails.
type boundedTransport struct {
	base http.RoundTripper
}

func (t boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, left: maxAnswer}
	return resp, nil
}

// errTooLarge is an answer over maxAnswer bytes.
var errTooLarge = fmt.Errorf("the answer is over %d bytes", maxAnswer)

type boundedBody struct {
	io.ReadCloser
	left int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return 0, errTooLarge
	}
	return n, err
}

// Token is a verified token.
type Token struct {
	Issuer  string // the issuer's config name
	Kind    Kind
	URL     string // the issuer's URL
	Subject string
	ID      string // jti
	Issued  time.Time
	Expires time.Time
	Claims  Claims
}

// Claims are a verified token's claims.
type Claims map[string]json.RawMessage

// String returns a claim that is a JSON string.
func (c Claims) String(name string) (string, bool) {
	var s string
	raw, ok := c[name]
	if !ok || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Values returns a claim as text, for team rules: a string as itself, a
// boolean or a number as its JSON text, and a list as each scalar element's
// text. ok is false for a missing claim, null, an object, or a list with no
// scalar element.
func (c Claims) Values(name string) ([]string, bool) {
	raw, ok := c[name]
	if !ok {
		return nil, false
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var out []string
		for _, e := range list {
			if s, ok := scalarText(e); ok {
				out = append(out, s)
			}
		}
		return out, len(out) > 0
	}
	s, ok := scalarText(raw)
	if !ok {
		return nil, false
	}
	return []string{s}, true
}

func scalarText(raw json.RawMessage) (string, bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	case bool, float64:
		return strings.TrimSpace(string(raw)), true
	}
	return "", false
}

// Verify checks raw in the documented order and stops at the first check it
// fails, with a refusal that names it.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Token, *Refusal) {
	iss, ref := screenIssuer(raw)
	if ref != nil {
		return nil, ref
	}
	is, ok := v.issuers[iss]
	if !ok {
		return nil, failed(CheckIssuer, ReasonUntrustedIssuer)
	}
	tok := &Token{Issuer: is.Name, Kind: is.Kind, URL: is.URL}
	return verifySigned(ctx, raw, is.Audience, is.keys, v.now, tok, func(tok *Token) *Refusal {
		if ref := checkAudience(tok.Claims, is.Audience); ref != nil {
			return ref
		}
		return checkClaims(tok.Claims, is.Kind, tok)
	})
}

// screenIssuer screens raw and reads the issuer it claims, which picks the
// keys its signature is checked with.
func screenIssuer(raw string) (string, *Refusal) {
	t, ref := screen(raw)
	if ref != nil {
		return "", ref
	}
	var iss string
	if why := stringField(t.claims, FieldIss, &iss); why != 0 {
		return "", failed(CheckIssuer, why).of(FieldIss)
	}
	return iss, nil
}

// algs are the signature algorithms Writ accepts, for the libraries.
var algs = []string{string(RS256), string(ES256)}

// errRefused is what signatureCheck returns to go-oidc. The refusal stays
// in signatureCheck; go-oidc's error, which can quote the token, is never
// read.
var errRefused = errors.New("refused")

// verifySigned checks raw's signature, issuer, and audience with go-oidc,
// against keys, then checks exp, iat, and nbf with Writ's leeway, then
// runs policy, the issuer kind's own checks. tok names the issuer and
// gets the claims of the payload the signature covers. raw has passed the
// screen. go-oidc's expiry check is off.
//
// go-oidc names no failure by type, and its message can quote the token.
// When it refuses a token whose signature held, Writ's checks, which are
// stricter, name the check that failed.
func verifySigned(ctx context.Context, raw, clientID string, keys *keySet, now func() time.Time, tok *Token, policy func(*Token) *Refusal) (*Token, *Refusal) {
	sc := &signatureCheck{keys: keys}
	verifier := gooidc.NewVerifier(tok.URL, sc, &gooidc.Config{ClientID: clientID, SupportedSigningAlgs: algs, SkipExpiryCheck: true})
	_, libErr := verifier.Verify(ctx, raw)
	switch {
	case sc.ref != nil:
		return nil, sc.ref
	case libErr != nil && sc.payload == nil:
		return nil, failed(CheckShape, ReasonNotJWS)
	}
	claims, why := object(sc.payload)
	if why != 0 {
		return nil, failed(CheckShape, why).of(FieldPayload)
	}
	tok.Claims = Claims(claims)
	if ref := checkTimes(claims, now(), tok); ref != nil {
		return nil, ref
	}
	if ref := policy(tok); ref != nil {
		return nil, ref
	}
	if libErr != nil {
		return nil, failed(CheckClaims, ReasonLibrary)
	}
	return tok, nil
}

// signatureCheck is the key set go-oidc checks one token's signature
// with: the issuer's cached keys, the key the token's kid names, and
// go-jose's signature check. It keeps the first refusal.
type signatureCheck struct {
	keys    *keySet
	ref     *Refusal
	payload []byte
}

// VerifySignature implements go-oidc's KeySet.
func (c *signatureCheck) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil || len(jws.Signatures) != 1 {
		c.ref = failed(CheckShape, ReasonNotCompactJWS)
		return nil, errRefused
	}
	h := jws.Signatures[0].Header
	k, ref := c.keys.key(ctx, h.KeyID)
	if ref != nil {
		c.ref = ref
		return nil, errRefused
	}
	if !k.fits(Alg(h.Algorithm)) {
		c.ref = failed(CheckSignature, ReasonKeyAlg)
		return nil, errRefused
	}
	payload, err := jws.Verify(k.pub)
	if err != nil {
		c.ref = failed(CheckSignature, ReasonSignature)
		return nil, errRefused
	}
	c.payload = payload
	return payload, nil
}

func checkTimes(claims map[string]json.RawMessage, now time.Time, tok *Token) *Refusal {
	exp, ok, why := numericDate(claims, FieldExp)
	switch {
	case why != 0:
		return failed(CheckTimes, why).of(FieldExp)
	case !ok:
		return failed(CheckTimes, ReasonMissing).of(FieldExp)
	case !exp.After(now.Add(-Leeway)):
		return failed(CheckTimes, ReasonExpired)
	}
	iat, ok, why := numericDate(claims, FieldIat)
	switch {
	case why != 0:
		return failed(CheckTimes, why).of(FieldIat)
	case !ok:
		return failed(CheckTimes, ReasonMissing).of(FieldIat)
	case iat.After(now.Add(Leeway)):
		return failed(CheckTimes, ReasonIssuedInFuture)
	}
	nbf, ok, why := numericDate(claims, FieldNbf)
	switch {
	case why != 0:
		return failed(CheckTimes, why).of(FieldNbf)
	case ok && nbf.After(now.Add(Leeway)):
		return failed(CheckTimes, ReasonNotYetValid)
	}
	tok.Issued, tok.Expires = iat, exp
	return nil
}

// checkAudience accepts aud equal to want, or a list whose only element is
// want. A list that also names another audience is refused, because that
// other relying party could then replay the token here.
func checkAudience(claims map[string]json.RawMessage, want string) *Refusal {
	raw, ok := claims["aud"]
	if !ok {
		return failed(CheckAudience, ReasonMissing).of(FieldAud)
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one != want {
			return failed(CheckAudience, ReasonAudOther)
		}
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return failed(CheckAudience, ReasonAudNotStrings)
	}
	if len(list) != 1 || list[0] != want {
		return failed(CheckAudience, ReasonAudList).counted(int64(len(list)), 1)
	}
	return nil
}

func checkClaims(claims map[string]json.RawMessage, kind Kind, tok *Token) *Refusal {
	c := Claims(claims)
	for _, f := range append([]Field{FieldSub, FieldJti}, required[kind]...) {
		s, ok := c.String(f.Name())
		if !ok {
			if _, present := c[f.Name()]; present {
				return failed(CheckClaims, ReasonNotString).of(f)
			}
			return failed(CheckClaims, ReasonClaimMissing).of(f)
		}
		if s == "" {
			return failed(CheckClaims, ReasonEmpty).of(f)
		}
	}
	if kind == GitLabCI {
		if p, _ := c.String("ref_protected"); !slices.Contains([]string{"true", "false"}, p) {
			return failed(CheckClaims, ReasonRefProtected)
		}
	}
	tok.Subject, _ = c.String("sub")
	tok.ID, _ = c.String("jti")
	return nil
}

// Health is one issuer's key set, for /healthz.
type Health struct {
	Issuer string
	GoodAt time.Time
	// Offset is the issuer's clock minus this host's, from the Date header
	// of the last key set fetch. Valid only when HasOffset.
	Offset    time.Duration
	HasOffset bool
}

// Health reports every issuer's key set.
func (v *Verifier) Health() []Health {
	var out []Health
	for _, is := range v.issuers {
		good, off, ok := is.keys.status()
		out = append(out, Health{Issuer: is.Name, GoodAt: good, Offset: off, HasOffset: ok})
	}
	slices.SortFunc(out, func(a, b Health) int { return strings.Compare(a.Issuer, b.Issuer) })
	return out
}
