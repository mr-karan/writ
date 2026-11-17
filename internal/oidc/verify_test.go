package oidc_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
)

const audience = "https://writ.example.com"

// clock is a fake clock the tests move by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	is    *oidctest.Issuer
	clock *clock
	v     *oidc.Verifier
}

func newFixture(t *testing.T, kind oidc.Kind) *fixture {
	t.Helper()
	is := oidctest.New(t)
	c := &clock{now: time.Date(2026, 11, 10, 10, 42, 0, 0, time.UTC)}
	v := oidc.New(oidc.Options{Now: c.Now})
	if ref := v.Add(context.Background(), oidc.Issuer{Name: "gitlab", Kind: kind, URL: is.URL, Audience: audience, Client: is.Client()}); ref != nil {
		t.Fatal(ref.Message())
	}
	return &fixture{is: is, clock: c, v: v}
}

func (f *fixture) claims() map[string]any {
	return oidctest.GitLabJob(f.is.URL, audience, f.clock.Now())
}

func (f *fixture) key(id string) oidctest.Key {
	for _, k := range f.is.Keys() {
		if k.ID == id {
			return k
		}
	}
	panic("no key " + id)
}

func checkOf(ref *oidc.Refusal) oidc.Check {
	if ref == nil {
		return 0
	}
	return ref.Check()
}

func with(base map[string]any, edits map[string]any) map[string]any {
	out := maps.Clone(base)
	for k, v := range edits {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func TestVerifyChecks(t *testing.T) {
	f := newFixture(t, oidc.GitLabCI)
	rsaKey, ecKey := f.key("rsa-1"), f.key("ec-1")
	now := f.clock.Now()
	enc := base64.RawURLEncoding.EncodeToString
	good := f.claims()
	stranger := oidctest.NewRSAKey(t, "rsa-1", 2048)
	tests := []struct {
		name  string
		token string
		want  oidc.Check // 0 means the token passes
	}{
		{"rs256 passes", oidctest.Sign(rsaKey, good), 0},
		{"es256 passes", oidctest.Sign(ecKey, good), 0},
		{"two parts", "a.b", oidc.CheckShape},
		{"five parts, a JWE", "a.b.c.d.e", oidc.CheckShape},
		{"oversized", strings.Repeat("a", oidc.MaxTokenBytes+1), oidc.CheckShape},
		{"padded base64", oidctest.Sign(rsaKey, good) + "=", oidc.CheckShape},
		{"header not json", enc([]byte("nope")) + "." + enc([]byte("{}")) + ".AA", oidc.CheckHeader},
		{"alg none", oidctest.SignWith(rsaKey, map[string]any{"alg": "none", "kid": "rsa-1"}, good), oidc.CheckHeader},
		{"alg none unsigned", enc([]byte(`{"alg":"none","kid":"rsa-1"}`)) + "." + enc(mustJSON(good)) + ".", oidc.CheckHeader},
		{"alg HS256", oidctest.SignWith(rsaKey, map[string]any{"alg": "HS256", "kid": "rsa-1"}, good), oidc.CheckHeader},
		{"alg RS384", oidctest.SignWith(rsaKey, map[string]any{"alg": "RS384", "kid": "rsa-1"}, good), oidc.CheckHeader},
		{"alg PS256", oidctest.SignWith(rsaKey, map[string]any{"alg": "PS256", "kid": "rsa-1"}, good), oidc.CheckHeader},
		{"alg lowercase", oidctest.SignWith(rsaKey, map[string]any{"alg": "rs256", "kid": "rsa-1"}, good), oidc.CheckHeader},
		{"alg missing", oidctest.SignWith(rsaKey, map[string]any{"kid": "rsa-1"}, good), oidc.CheckHeader},
		{"alg not a string", oidctest.SignWith(rsaKey, map[string]any{"alg": 1, "kid": "rsa-1"}, good), oidc.CheckHeader},
		{"alg twice", oidctest.SignBytes(rsaKey, []byte(`{"alg":"none","alg":"RS256","kid":"rsa-1"}`), mustJSON(good)), oidc.CheckHeader},
		{"jku", oidctest.SignWith(rsaKey, map[string]any{"alg": "RS256", "kid": "rsa-1", "jku": "https://evil.example/keys"}, good), oidc.CheckHeader},
		{"jwk", oidctest.SignWith(rsaKey, map[string]any{"alg": "RS256", "kid": "rsa-1", "jwk": oidctest.JWK(rsaKey)}, good), oidc.CheckHeader},
		{"x5u", oidctest.SignWith(rsaKey, map[string]any{"alg": "RS256", "kid": "rsa-1", "x5u": "https://evil.example/c"}, good), oidc.CheckHeader},
		{"x5c", oidctest.SignWith(rsaKey, map[string]any{"alg": "RS256", "kid": "rsa-1", "x5c": []string{"MII"}}, good), oidc.CheckHeader},
		{"crit", oidctest.SignWith(rsaKey, map[string]any{"alg": "RS256", "kid": "rsa-1", "crit": []string{"exp"}}, good), oidc.CheckHeader},
		{"kid missing", oidctest.SignWith(rsaKey, map[string]any{"alg": "RS256"}, good), oidc.CheckHeader},
		{"kid empty", oidctest.SignWith(rsaKey, map[string]any{"alg": "RS256", "kid": ""}, good), oidc.CheckHeader},
		{"claims twice", oidctest.SignBytes(rsaKey, []byte(`{"alg":"RS256","kid":"rsa-1"}`), []byte(`{"aud":"x","aud":"https://writ.example.com"}`)), oidc.CheckShape},
		{"issuer unknown", oidctest.Sign(rsaKey, with(good, map[string]any{"iss": "https://gitlab.example.org"})), oidc.CheckIssuer},
		{"issuer with a trailing slash", oidctest.Sign(rsaKey, with(good, map[string]any{"iss": f.is.URL + "/"})), oidc.CheckIssuer},
		{"issuer missing", oidctest.Sign(rsaKey, with(good, map[string]any{"iss": nil})), oidc.CheckIssuer},
		{"kid unknown", oidctest.Sign(oidctest.Key{ID: "rsa-9", RSA: rsaKey.RSA}, good), oidc.CheckSignature},
		{"signed by a stranger with a known kid", oidctest.Sign(stranger, good), oidc.CheckSignature},
		{"rs256 header on an ec key", oidctest.SignWith(rsaKey, map[string]any{"alg": "RS256", "kid": "ec-1"}, good), oidc.CheckSignature},
		{"es256 header on an rsa key", oidctest.SignWith(ecKey, map[string]any{"alg": "ES256", "kid": "rsa-1"}, good), oidc.CheckSignature},
		{"signature cut short", cut(oidctest.Sign(rsaKey, good)), oidc.CheckSignature},
		{"payload changed after signing", swapPayload(oidctest.Sign(rsaKey, good), with(good, map[string]any{"project_id": "1"})), oidc.CheckSignature},
		{"exp just inside the leeway", oidctest.Sign(rsaKey, with(good, map[string]any{"exp": now.Add(-59 * time.Second).Unix()})), 0},
		{"exp at the leeway", oidctest.Sign(rsaKey, with(good, map[string]any{"exp": now.Add(-60 * time.Second).Unix()})), oidc.CheckTimes},
		{"exp missing", oidctest.Sign(rsaKey, with(good, map[string]any{"exp": nil})), oidc.CheckTimes},
		{"exp a string", oidctest.Sign(rsaKey, with(good, map[string]any{"exp": "tomorrow"})), oidc.CheckTimes},
		{"exp huge", oidctest.Sign(rsaKey, with(good, map[string]any{"exp": 1e300})), oidc.CheckTimes},
		{"iat at the leeway", oidctest.Sign(rsaKey, with(good, map[string]any{"iat": now.Add(60 * time.Second).Unix()})), 0},
		{"iat past the leeway", oidctest.Sign(rsaKey, with(good, map[string]any{"iat": now.Add(61 * time.Second).Unix()})), oidc.CheckTimes},
		{"iat missing", oidctest.Sign(rsaKey, with(good, map[string]any{"iat": nil})), oidc.CheckTimes},
		{"nbf at the leeway", oidctest.Sign(rsaKey, with(good, map[string]any{"nbf": now.Add(60 * time.Second).Unix()})), 0},
		{"nbf past the leeway", oidctest.Sign(rsaKey, with(good, map[string]any{"nbf": now.Add(61 * time.Second).Unix()})), oidc.CheckTimes},
		{"nbf missing", oidctest.Sign(rsaKey, with(good, map[string]any{"nbf": nil})), 0},
		{"audience of another service", oidctest.Sign(rsaKey, with(good, map[string]any{"aud": "https://vault.example.com"})), oidc.CheckAudience},
		{"audience a list of ours", oidctest.Sign(rsaKey, with(good, map[string]any{"aud": []string{audience}})), 0},
		{"audience shared with another service", oidctest.Sign(rsaKey, with(good, map[string]any{"aud": []string{audience, "https://vault.example.com"}})), oidc.CheckAudience},
		{"audience missing", oidctest.Sign(rsaKey, with(good, map[string]any{"aud": nil})), oidc.CheckAudience},
		{"audience a number", oidctest.Sign(rsaKey, with(good, map[string]any{"aud": 7})), oidc.CheckAudience},
		{"project_id missing", oidctest.Sign(rsaKey, with(good, map[string]any{"project_id": nil})), oidc.CheckClaims},
		{"project_id a number", oidctest.Sign(rsaKey, with(good, map[string]any{"project_id": 4242})), oidc.CheckClaims},
		{"user_login empty", oidctest.Sign(rsaKey, with(good, map[string]any{"user_login": ""})), oidc.CheckClaims},
		{"ref_protected not a boolean word", oidctest.Sign(rsaKey, with(good, map[string]any{"ref_protected": "yes"})), oidc.CheckClaims},
		{"jti missing", oidctest.Sign(rsaKey, with(good, map[string]any{"jti": nil})), oidc.CheckClaims},
		{"sub missing", oidctest.Sign(rsaKey, with(good, map[string]any{"sub": nil})), oidc.CheckClaims},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, err := f.v.Verify(context.Background(), tt.token)
			if got := checkOf(err); got != tt.want || (tt.want == 0 && err != nil) {
				t.Fatalf("check %q, want %q (err %v)", got, tt.want, err)
			}
			if err != nil && strings.Contains(err.Message(), tt.token) && len(tt.token) > 20 {
				t.Fatalf("the error holds the token: %v", err)
			}
			if tt.want == 0 && (tok.Issuer != "gitlab" || tok.Kind != oidc.GitLabCI || tok.ID == "" || tok.Subject == "") {
				t.Fatalf("token %+v", tok)
			}
		})
	}
}

func TestGitHubClaims(t *testing.T) {
	f := newFixture(t, oidc.GitHubActions)
	good := oidctest.GitHubJob(f.is.URL, audience, f.clock.Now())
	if _, err := f.v.Verify(context.Background(), oidctest.Sign(f.key("rsa-1"), good)); err != nil {
		t.Fatal(err)
	}
	bad := with(good, map[string]any{"repository_id": nil})
	if _, err := f.v.Verify(context.Background(), oidctest.Sign(f.key("rsa-1"), bad)); checkOf(err) != oidc.CheckClaims {
		t.Fatalf("got %v", err)
	}
	// A GitLab token is not a GitHub token: it lacks repository and the rest.
	gl := oidctest.GitLabJob(f.is.URL, audience, f.clock.Now())
	if _, err := f.v.Verify(context.Background(), oidctest.Sign(f.key("rsa-1"), gl)); checkOf(err) != oidc.CheckClaims {
		t.Fatalf("got %v", err)
	}
}

func TestClaimValues(t *testing.T) {
	c := oidc.Claims{
		"s": json.RawMessage(`"4242"`), "b": json.RawMessage(`true`), "n": json.RawMessage(`7`),
		"l": json.RawMessage(`["a", 1, {"x":1}]`), "o": json.RawMessage(`{"a":1}`), "z": json.RawMessage(`null`),
	}
	tests := []struct {
		name string
		want []string
	}{
		{"s", []string{"4242"}}, {"b", []string{"true"}}, {"n", []string{"7"}}, {"l", []string{"a", "1"}},
		{"o", nil}, {"z", nil}, {"missing", nil},
	}
	for _, tt := range tests {
		got, ok := c.Values(tt.name)
		if strings.Join(got, ",") != strings.Join(tt.want, ",") || ok != (tt.want != nil) {
			t.Errorf("%s: got %v %v", tt.name, got, ok)
		}
	}
}

func TestKeyRotation(t *testing.T) {
	f := newFixture(t, oidc.GitLabCI)
	next := oidctest.NewRSAKey(t, "rsa-2", 2048)
	f.is.SetKeys(next)
	before := f.is.Fetches()
	// An unknown kid causes one refresh, which finds the new key.
	if _, err := f.v.Verify(context.Background(), oidctest.Sign(next, f.claims())); err != nil {
		t.Fatal(err)
	}
	if got := f.is.Fetches() - before; got != 1 {
		t.Fatalf("%d fetches, want 1", got)
	}
	// The kid the refresh found is known now, so it causes no second
	// refresh.
	if _, err := f.v.Verify(context.Background(), oidctest.Sign(f.key("rsa-2"), f.claims())); err != nil {
		t.Fatal(err)
	}
	if f.is.Fetches()-before != 1 {
		t.Fatal("a known kid caused a refresh")
	}
}

func TestUnknownKidRefreshesOnceAMinute(t *testing.T) {
	f := newFixture(t, oidc.GitLabCI)
	ghost := oidctest.NewRSAKey(t, "ghost", 2048)
	before := f.is.Fetches()
	for range 20 {
		if _, err := f.v.Verify(context.Background(), oidctest.Sign(ghost, f.claims())); checkOf(err) != oidc.CheckSignature {
			t.Fatalf("got %v", err)
		}
	}
	if got := f.is.Fetches() - before; got != 1 {
		t.Fatalf("20 unknown kids made %d fetches, want 1", got)
	}
	f.clock.Add(59 * time.Second)
	_, _ = f.v.Verify(context.Background(), oidctest.Sign(ghost, f.claims()))
	if got := f.is.Fetches() - before; got != 1 {
		t.Fatalf("%d fetches before a minute passed, want 1", got)
	}
	f.clock.Add(time.Second)
	_, _ = f.v.Verify(context.Background(), oidctest.Sign(ghost, f.claims()))
	if got := f.is.Fetches() - before; got != 2 {
		t.Fatalf("%d fetches after a minute, want 2", got)
	}
}

func TestKeySetSchedule(t *testing.T) {
	tests := []struct {
		name  string
		cache string
		after time.Duration // when the next scheduled refresh is due
	}{
		{"default hour", "", time.Hour},
		{"max-age ten minutes", "public, max-age=600", 10 * time.Minute},
		{"max-age below five minutes", "max-age=10", 5 * time.Minute},
		{"max-age above an hour", "max-age=86400", time.Hour},
		{"max-age not a number", "max-age=soon", time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := oidctest.New(t)
			is.SetCacheControl(tt.cache)
			c := &clock{now: time.Date(2026, 11, 10, 10, 42, 0, 0, time.UTC)}
			v := oidc.New(oidc.Options{Now: c.Now})
			if ref := v.Add(context.Background(), oidc.Issuer{Name: "gitlab", Kind: oidc.GitLabCI, URL: is.URL, Audience: audience, Client: is.Client()}); ref != nil {
				t.Fatal(ref.Message())
			}
			k := is.Keys()[0]
			verify := func() {
				t.Helper()
				if _, err := v.Verify(context.Background(), oidctest.Sign(k, oidctest.GitLabJob(is.URL, audience, c.Now()))); err != nil {
					t.Fatal(err)
				}
			}
			start := is.Fetches()
			c.Add(tt.after - time.Second)
			verify()
			if is.Fetches() != start {
				t.Fatalf("refreshed before %s", tt.after)
			}
			c.Add(time.Second)
			verify()
			if is.Fetches() != start+1 {
				t.Fatalf("did not refresh at %s", tt.after)
			}
		})
	}
}

func TestFailedRefreshKeepsLastSetForADay(t *testing.T) {
	f := newFixture(t, oidc.GitLabCI)
	k := f.key("rsa-1")
	f.is.SetFail(true)
	verify := func() *oidc.Refusal {
		_, ref := f.v.Verify(context.Background(), oidctest.Sign(k, f.claims()))
		return ref
	}
	f.clock.Add(2 * time.Hour)
	before := f.is.Fetches()
	if err := verify(); err != nil {
		t.Fatalf("a failed refresh dropped the last good set: %v", err)
	}
	if f.is.Fetches() != before+1 {
		t.Fatal("no refresh was tried")
	}
	// The next try waits a minute.
	if err := verify(); err != nil || f.is.Fetches() != before+1 {
		t.Fatalf("retried at once: %v", err)
	}
	f.clock.Add(22*time.Hour - time.Second)
	if err := verify(); err != nil {
		t.Fatalf("within a day: %v", err)
	}
	f.clock.Add(2 * time.Second)
	if err := verify(); checkOf(err) != oidc.CheckSignature {
		t.Fatalf("a day without a good set: %v", err)
	}
	f.is.SetFail(false)
	f.clock.Add(time.Minute)
	if err := verify(); err != nil {
		t.Fatalf("after the issuer came back: %v", err)
	}
}

func TestDiscovery(t *testing.T) {
	tests := []struct {
		name    string
		issuer  string
		jwksURI string
		want    string
	}{
		{"issuer differs", "https://gitlab.com", "", "names another issuer"},
		{"key set on another host", "", "https://evil.example/keys", "jwks_uri is not an HTTPS URL on the issuer's host"},
		{"key set over http", "", "http://127.0.0.1/keys", "not an HTTPS URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := oidctest.New(t)
			is.SetDiscovery(tt.issuer, tt.jwksURI)
			err := oidc.New(oidc.Options{}).Add(context.Background(), oidc.Issuer{Name: "gitlab", Kind: oidc.GitLabCI, URL: is.URL, Audience: audience, Client: is.Client()})
			if err == nil || !strings.Contains(err.Message(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
	t.Run("issuer over http", func(t *testing.T) {
		err := oidc.New(oidc.Options{}).Add(context.Background(), oidc.Issuer{Name: "x", Kind: oidc.GitLabCI, URL: "http://gitlab.example.com", Audience: audience})
		if err == nil || !strings.Contains(err.Message(), "HTTPS") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unreachable issuer fails the start", func(t *testing.T) {
		is := oidctest.New(t)
		is.SetFail(true)
		err := oidc.New(oidc.Options{}).Add(context.Background(), oidc.Issuer{Name: "x", Kind: oidc.GitLabCI, URL: is.URL, Audience: audience, Client: is.Client()})
		if err == nil {
			t.Fatal("started with no key set")
		}
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// cut drops the last byte of the signature.
func cut(tok string) string {
	parts := strings.Split(tok, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		panic(err)
	}
	parts[2] = base64.RawURLEncoding.EncodeToString(sig[:len(sig)-1])
	return strings.Join(parts, ".")
}

func swapPayload(tok string, claims map[string]any) string {
	parts := strings.Split(tok, ".")
	parts[1] = base64.RawURLEncoding.EncodeToString(mustJSON(claims))
	return strings.Join(parts, ".")
}
