package oidc

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/oidc/oidctest"
)

// staticVerifier checks tokens against keys held in memory, with a fixed
// clock and no HTTP. A refresh is never due, and an unknown kid never
// causes one.
func staticVerifier(is Issuer, keys map[string]key, now time.Time) *Verifier {
	ks := &keySet{
		issuer: is.Name, keys: keys, now: func() time.Time { return now }, log: slog.New(slog.DiscardHandler),
		goodAt: now, due: now.Add(time.Hour), forcedAt: now,
	}
	return &Verifier{issuers: map[string]*issuer{is.URL: {Issuer: is, keys: ks}}, now: func() time.Time { return now }}
}

func fuzzKeys(t testing.TB) (oidctest.Key, oidctest.Key, map[string]key) {
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rk, ek := oidctest.Key{ID: "rsa-1", RSA: r}, oidctest.Key{ID: "ec-1", EC: e}
	body, _ := json.Marshal(map[string]any{"keys": []map[string]string{oidctest.JWK(rk), oidctest.JWK(ek)}})
	keys, ref := parseKeySet(body)
	if ref != nil {
		t.Fatal(ref.Message())
	}
	return rk, ek, keys
}

// FuzzVerify feeds arbitrary tokens to a verifier whose keys signed only
// the seeds. It must never panic, and it must accept only a token whose
// signed part equals a seed's: anything else would be a forgery or a parser
// that reads a token two ways.
func FuzzVerify(f *testing.F) {
	now := time.Date(2026, 11, 10, 10, 42, 0, 0, time.UTC)
	is := Issuer{Name: "gitlab", Kind: GitLabCI, URL: "https://gitlab.com", Audience: "https://writ.example.com"}
	rk, ek, keys := fuzzKeys(f)
	v := staticVerifier(is, keys, now)
	claims := oidctest.GitLabJob(is.URL, is.Audience, now)
	seeds := []string{
		oidctest.Sign(rk, claims),
		oidctest.Sign(ek, claims),
		oidctest.SignWith(rk, map[string]any{"alg": "none", "kid": "rsa-1"}, claims),
		oidctest.SignWith(rk, map[string]any{"alg": "RS256", "kid": "rsa-1", "jwk": oidctest.JWK(rk)}, claims),
		oidctest.SignWith(ek, map[string]any{"alg": "RS256", "kid": "ec-1"}, claims),
		"a.b.c.d.e",
	}
	signed := map[string]bool{}
	for _, s := range seeds {
		signed[s[:strings.LastIndexByte(s, '.')]] = true
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, tok string) {
		got, ref := v.Verify(context.Background(), tok)
		if ref != nil {
			if ref.Stage() != StageToken || ref.Check() == 0 {
				t.Fatalf("a refusal that names no check: %s", ref.Message())
			}
			return
		}
		if !signed[tok[:strings.LastIndexByte(tok, '.')]] {
			t.Fatalf("accepted a token no key signed: %q", tok)
		}
		if got.ID == "" || got.Subject == "" {
			t.Fatalf("accepted a token without jti or sub")
		}
	})
}

// FuzzParseKeySet feeds arbitrary key sets to the parser. It must never
// panic, and every key it keeps must be inside the bounds.
func FuzzParseKeySet(f *testing.F) {
	rk, ek, _ := fuzzKeys(f)
	good, _ := json.Marshal(map[string]any{"keys": []map[string]string{oidctest.JWK(rk), oidctest.JWK(ek)}})
	f.Add(good)
	f.Add([]byte(`{"keys":[{"kty":"RSA","kid":"a","n":"AQAB","e":"AQAB"}]}`))
	f.Add([]byte(`{"keys":[{"kty":"EC","kid":"a","crv":"P-256","x":"","y":""}]}`))
	f.Add([]byte(`{"keys":null}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		keys, ref := parseKeySet(body)
		if ref != nil {
			return
		}
		if len(keys) > MaxKeys {
			t.Fatalf("kept %d keys", len(keys))
		}
		for kid, k := range keys {
			if k.kid != kid {
				t.Fatalf("key %q filed under %q", k.kid, kid)
			}
			switch pub := k.pub.(type) {
			case *rsa.PublicKey:
				if pub.N.BitLen() < minRSABits || pub.N.BitLen() > maxRSABits || pub.E != 65537 {
					t.Fatalf("kept an RSA key out of bounds: %d bits, e %d", pub.N.BitLen(), pub.E)
				}
			case *ecdsa.PublicKey:
				if pub.Curve != elliptic.P256() {
					t.Fatalf("kept an EC key on %s", pub.Curve.Params().Name)
				}
			default:
				t.Fatalf("kept key %q with a %T", kid, k.pub)
			}
		}
	})
}

var update = flag.Bool("update", false, "rewrite the token fixtures in testdata/tokens")

// tokenFixture is one file in testdata/tokens: a token, the key set that
// signed it, and the clock and audience to check it with. The fixtures in
// the repository were signed in a test, in the shapes GitLab's and GitHub's
// documents show. A token recorded from a real job goes in the same format;
// see docs/development.md for how to record one.
type tokenFixture struct {
	Comment  string          `json:"comment"`
	Kind     Kind            `json:"kind"`
	Issuer   string          `json:"issuer"`
	Audience string          `json:"audience"`
	At       time.Time       `json:"at"`
	JWKS     json.RawMessage `json:"jwks"`
	Token    string          `json:"token"`
	// Want is the name of the check the token fails, "" when it passes.
	Want string `json:"want"`
}

func TestTokenFixtures(t *testing.T) {
	if *update {
		writeFixtures(t)
	}
	paths, err := filepath.Glob("testdata/tokens/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var fx tokenFixture
			if err := json.Unmarshal(b, &fx); err != nil {
				t.Fatal(err)
			}
			keys, ref := parseKeySet(fx.JWKS)
			if ref != nil {
				t.Fatal(ref.Message())
			}
			v := staticVerifier(Issuer{Name: "fixture", Kind: fx.Kind, URL: fx.Issuer, Audience: fx.Audience}, keys, fx.At)
			_, ref = v.Verify(context.Background(), fx.Token)
			got := ""
			if ref != nil {
				got = ref.Check().String()
			}
			if got != fx.Want {
				t.Fatalf("check %q, want %q: %v", got, fx.Want, ref)
			}
		})
	}
}

func writeFixtures(t *testing.T) {
	at := time.Date(2026, 11, 10, 10, 42, 0, 0, time.UTC)
	rk, _, _ := fuzzKeys(t)
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{oidctest.JWK(rk)}})
	gl := oidctest.GitLabJob("https://gitlab.com", "https://writ.example.com", at)
	gl["jti"] = "235b3a54-b797-45c7-ae9a-f72d7bc6ef5b"
	gh := oidctest.GitHubJob("https://token.actions.githubusercontent.com", "https://writ.example.com", at)
	gh["jti"] = "example-id"
	// GitHub's tokens carry x5t, a thumbprint, in the header. Writ ignores it.
	ghHeader := map[string]any{"typ": "JWT", "alg": "RS256", "x5t": "example-thumbprint", "kid": "rsa-1"}
	shape := "signed in a test with a generated key, in the claim shape of "
	fixtures := map[string]tokenFixture{
		"gitlab-mr-job.json": {
			Comment: shape + "docs.gitlab.com/ci/secrets/id_token_authentication/, read 2026-11-10",
			Kind:    GitLabCI, Issuer: "https://gitlab.com", Audience: "https://writ.example.com", At: at,
			JWKS: jwks, Token: oidctest.Sign(rk, gl),
		},
		"gitlab-mr-job-expired.json": {
			Comment: "the same token, checked an hour after it expired",
			Kind:    GitLabCI, Issuer: "https://gitlab.com", Audience: "https://writ.example.com", At: at.Add(65 * time.Minute),
			JWKS: jwks, Token: oidctest.Sign(rk, gl), Want: CheckTimes.String(),
		},
		"github-pr-job.json": {
			Comment: shape + "docs.github.com/en/actions/reference/security/oidc, read 2026-11-10",
			Kind:    GitHubActions, Issuer: "https://token.actions.githubusercontent.com", Audience: "https://writ.example.com", At: at,
			JWKS: jwks, Token: oidctest.SignWith(rk, ghHeader, gh),
		},
	}
	if err := os.MkdirAll("testdata/tokens", 0o755); err != nil {
		t.Fatal(err)
	}
	for name, fx := range fixtures {
		b, _ := json.MarshalIndent(fx, "", "  ")
		if err := os.WriteFile(filepath.Join("testdata/tokens", name), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fuzzMarker is in every seed of FuzzRefusal. No refusal and no log line
// may hold it, whatever the answer around it.
const fuzzMarker = "QXJ5MARKERZ"

// Stages of FuzzRefusal: which answer of the identity provider the
// fuzzer writes, or, for stageRawToken, the ID token and the CI token.
const (
	stageDiscovery = iota
	stageKeys
	stageToken
	stageUserinfo
	stageDevice
	stageRawToken
	fuzzStages
)

// fuzzIdP answers https://idp.test from memory: good answers, except the
// one the fuzzer writes. It never dials, so no input reaches a host.
type fuzzIdP struct {
	stage  int
	status int
	body   []byte
	good   map[string][]byte
}

func (f fuzzIdP) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		req.Body.Close()
	}
	status, body := http.StatusOK, f.good[path]
	if body == nil {
		status = http.StatusNotFound
	}
	if fuzzPaths[f.stage] == path {
		status, body = f.status, f.body
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)), Request: req,
	}, nil
}

var fuzzPaths = map[int]string{
	stageDiscovery: "/.well-known/openid-configuration", stageKeys: "/keys", stageToken: "/token",
	stageUserinfo: "/userinfo", stageDevice: "/device",
}

// fuzzHarness is an identity provider in memory and the flows that read
// it.
type fuzzHarness struct {
	now         time.Time
	key         oidctest.Key
	idClaims    map[string]any
	good        map[string][]byte
	tokenAnswer func(id string) []byte
	discovery   []byte
}

const fuzzIssuer, fuzzClient, fuzzAudience = "https://idp.test", "writ-client", "https://writ.example.com"

func newFuzzHarness(tb testing.TB) *fuzzHarness {
	h := &fuzzHarness{now: time.Date(2026, 11, 10, 10, 42, 0, 0, time.UTC)}
	h.key, _, _ = fuzzKeys(tb)
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{oidctest.JWK(h.key)}})
	h.idClaims = map[string]any{"iss": fuzzIssuer, "aud": fuzzClient, "sub": "2002", "nonce": "n", "iat": h.now.Unix(), "exp": h.now.Add(time.Hour).Unix()}
	h.tokenAnswer = func(id string) []byte {
		b, _ := json.Marshal(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": id})
		return b
	}
	h.discovery, _ = json.Marshal(map[string]any{
		"issuer": fuzzIssuer, "jwks_uri": fuzzIssuer + "/keys", "authorization_endpoint": fuzzIssuer + "/authorize",
		"token_endpoint": fuzzIssuer + "/token", "userinfo_endpoint": fuzzIssuer + "/userinfo",
		"device_authorization_endpoint": fuzzIssuer + "/device", "code_challenge_methods_supported": []string{"S256"},
	})
	h.good = map[string][]byte{
		fuzzPaths[stageDiscovery]: h.discovery,
		fuzzPaths[stageKeys]:      jwks,
		fuzzPaths[stageToken]:     h.tokenAnswer(oidctest.Sign(h.key, h.idClaims)),
		fuzzPaths[stageUserinfo]:  []byte(`{"sub":"2002","groups":["acme"]}`),
		fuzzPaths[stageDevice]:    []byte(`{"device_code":"dc","user_code":"ABCD-EFGH","verification_uri":"https://idp.test/device","expires_in":300,"interval":5}`),
	}
	return h
}

// run answers stage's request with status and body, runs every flow, and
// returns their refusals, nil where a flow passed, and the log.
func (h *fuzzHarness) run(stage, status int, body []byte) ([]*Refusal, string) {
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	answers := h.good
	if stage == stageRawToken {
		answers = maps.Clone(h.good)
		answers[fuzzPaths[stageToken]] = h.tokenAnswer(string(body))
	}
	hc := &http.Client{Transport: fuzzIdP{stage: stage, status: status, body: body, good: answers}}
	ctx := context.Background()
	now := func() time.Time { return h.now }
	var refs []*Refusal
	p, ref := NewProvider(ctx, ProviderOptions{Name: "sso", URL: fuzzIssuer, ClientID: fuzzClient, ClientSecret: "secret", GroupsFromUserinfo: true, Client: hc, Now: now, Log: logger})
	refs = append(refs, ref)
	if ref == nil {
		_, ref = p.Exchange(ctx, "code", "verifier", fuzzAudience+"/ui/callback", "n")
		refs = append(refs, ref)
		_, ref = p.StartDevice(ctx)
		refs = append(refs, ref)
	}
	v := New(Options{Now: now, Log: logger})
	ref = v.Add(ctx, Issuer{Name: "ci", Kind: GitLabCI, URL: fuzzIssuer, Audience: fuzzAudience, Client: hc})
	refs = append(refs, ref)
	if ref == nil && stage == stageRawToken {
		_, ref = v.Verify(ctx, string(body))
		refs = append(refs, ref)
	}
	return refs, log.String()
}

// TestFuzzHarnessPasses runs FuzzRefusal's flows with good answers only,
// so a seed that fails is known to fail on its own answer.
func TestFuzzHarnessPasses(t *testing.T) {
	h := newFuzzHarness(t)
	refs, log := h.run(stageDiscovery, http.StatusOK, h.discovery)
	if len(refs) != 4 {
		t.Fatalf("%d flows ran, want 4", len(refs))
	}
	for i, r := range refs {
		if r != nil {
			t.Errorf("flow %d: %s\n%s", i, r.Message(), log)
		}
	}
}

// FuzzRefusal drives a people issuer's start, a code exchange with
// userinfo, a device sign-in's start, and a CI issuer's start and token
// check against an identity provider whose one answer is the fuzzer's. No
// refusal's message and no log line may hold fuzzMarker.
func FuzzRefusal(f *testing.F) {
	h := newFuzzHarness(f)
	m, rk, idClaims, idp := fuzzMarker, h.key, h.idClaims, fuzzIssuer
	discoveryDoc, tokenAnswer := string(h.discovery), h.tokenAnswer
	seeds := []struct {
		stage  int
		status int
		body   string
	}{
		{stageDiscovery, 200, strings.Replace(discoveryDoc, `"issuer":"`+idp, `"issuer":"`+m, 1)},
		{stageDiscovery, 200, strings.Replace(discoveryDoc, idp+"/keys", "https://"+m+".invalid/keys", 1)},
		{stageDiscovery, 200, strings.Replace(discoveryDoc, idp+"/token", "http://"+m, 1)},
		{stageDiscovery, 500, m},
		{stageDiscovery, 302, m},
		{stageKeys, 200, `{"keys":[{"kty":"RSA","kid":"` + m + `","n":"AQ","e":"AQAB"}]}`},
		{stageKeys, 200, `{"keys":[{"kty":"RSA","kid":"` + m + `"},{"kty":"RSA","kid":"` + m + `"}]}`},
		{stageKeys, 200, `{"keys":[{"kty":"EC","kid":"k","crv":"P-256","x":"` + m + `","y":"` + m + `"}]}`},
		{stageKeys, 200, `{"keys":"` + m + `"}`},
		{stageToken, 400, `{"error":"` + m + `","error_description":"` + m + `","error_uri":"` + m + `"}`},
		{stageToken, 400, `{"error":"invalid_grant","error_description":"` + m + `"}`},
		{stageToken, 200, `{"` + m + `":`},
		{stageToken, 200, string(tokenAnswer(m))},
		{stageToken, 200, string(tokenAnswer(oidctest.SignWith(rk, map[string]any{"alg": m, "kid": "rsa-1"}, idClaims)))},
		{stageToken, 200, string(tokenAnswer(oidctest.SignWith(rk, map[string]any{"alg": "RS256", "kid": m}, idClaims)))},
		{stageUserinfo, 200, `{"sub":"` + m + `"}`},
		{stageUserinfo, 401, `{"error":"invalid_token","error_description":"` + m + `"}`},
		{stageDevice, 200, `{"device_code":"dc","user_code":"` + strings.Repeat(m, 6) + `","verification_uri":"https://idp.test/d","expires_in":300}`},
		{stageDevice, 200, `{"device_code":"dc","user_code":"AB","verification_uri":"https://` + m + `.invalid/","expires_in":300}`},
		{stageDevice, 200, `{"device_code":"dc","user_code":"AB","verification_uri":"https://idp.test/d","expires_in":"` + m + `"}`},
		{stageRawToken, 200, oidctest.SignWith(rk, map[string]any{"alg": m, "kid": "rsa-1"}, idClaims)},
		{stageRawToken, 200, oidctest.SignWith(rk, map[string]any{"alg": "RS256", "kid": "rsa-1"}, map[string]any{"iss": m, "aud": m})},
		{stageRawToken, 200, m + "." + m + "." + m},
	}
	for _, s := range seeds {
		f.Add(uint8(s.stage), uint16(s.status-200), []byte(s.body))
	}
	f.Fuzz(func(t *testing.T, stage uint8, status uint16, body []byte) {
		refs, log := h.run(int(stage)%fuzzStages, 200+int(status)%400, body)
		for _, r := range refs {
			if r != nil && strings.Contains(r.Message(), fuzzMarker) {
				t.Fatalf("a refusal holds the answer: %s", r.Message())
			}
		}
		if strings.Contains(log, fuzzMarker) {
			t.Fatalf("the log holds the answer: %s", log)
		}
	})
}
