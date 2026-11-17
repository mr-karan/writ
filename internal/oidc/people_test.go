package oidc_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
)

const redirectURI = "https://writ.example.com/ui/callback"

func newProvider(t *testing.T, is *oidctest.Issuer, groups bool) *oidc.Provider {
	t.Helper()
	is.SetClient("writ-client", "client-secret-value")
	p, err := oidc.NewProvider(context.Background(), oidc.ProviderOptions{
		Name: "sso", URL: is.URL, ClientID: "writ-client", ClientSecret: "client-secret-value",
		GroupsFromUserinfo: groups, Client: is.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// authorize follows the provider's sign-in URL to the issuer and returns
// the code and state it redirects back with.
func authorize(t *testing.T, is *oidctest.Issuer, u string) (code, state string) {
	t.Helper()
	c := is.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize answered %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !strings.HasPrefix(loc.String(), redirectURI) {
		t.Fatalf("redirected to %s, not the redirect URI", loc)
	}
	return loc.Query().Get("code"), loc.Query().Get("state")
}

func TestProviderSignIn(t *testing.T) {
	is := oidctest.New(t)
	is.SetPerson(oidctest.GitLabPerson())
	p := newProvider(t, is, true)
	if !p.S256 {
		t.Error("discovery lists S256, and S256 is false")
	}
	nonce := rand.Text()
	u, verifier := p.AuthCodeURL(redirectURI, "state-1", nonce)
	q, _ := url.Parse(u)
	if got := q.Query(); got.Get("code_challenge_method") != "S256" || len(got.Get("code_challenge")) != 43 || got.Get("scope") != "openid profile email" ||
		got.Get("nonce") != nonce || got.Get("redirect_uri") != redirectURI || got.Get("client_id") != "writ-client" {
		t.Fatalf("auth URL %s lacks the S256 challenge, the scope, the nonce, or the client", u)
	}
	if len(verifier) < 43 || strings.Contains(u, verifier) {
		t.Fatalf("verifier %q is short, or the auth URL holds it", verifier)
	}
	if _, again := p.AuthCodeURL(redirectURI, "state-1", nonce); again == verifier {
		t.Fatal("two sign-ins share a verifier")
	}
	code, state := authorize(t, is, u)
	if state != "state-1" || code == "" {
		t.Fatalf("code %q state %q", code, state)
	}
	tok, err := p.Exchange(context.Background(), code, verifier, redirectURI, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Subject != "2002" || tok.Kind != oidc.People || tok.Issuer != "sso" {
		t.Fatalf("token %+v", tok)
	}
	groups, _ := tok.Claims.Values("groups")
	if !slices.Equal(groups, []string{"acme", "acme/platform"}) {
		t.Errorf("groups %v, want userinfo's", groups)
	}
	if _, err := p.Exchange(context.Background(), code, verifier, redirectURI, nonce); err == nil {
		t.Error("a code was traded twice")
	}
}

func TestProviderRefusals(t *testing.T) {
	tests := []struct {
		name     string
		edit     func(map[string]any)
		verifier func(string) string
		nonce    func(string) string
		redirect string
		check    oidc.Check
		msg      string
	}{
		{name: "wrong nonce", nonce: func(string) string { return "other" }, check: oidc.CheckNonce},
		{name: "no nonce", edit: func(c map[string]any) { delete(c, "nonce") }, check: oidc.CheckNonce},
		{name: "other audience", edit: func(c map[string]any) { c["aud"] = "someone-else" }, check: oidc.CheckAudience},
		{name: "two audiences without azp", edit: func(c map[string]any) { c["aud"] = []string{"writ-client", "other"} }, check: oidc.CheckAudience},
		{name: "azp of another client", edit: func(c map[string]any) { c["azp"] = "other" }, check: oidc.CheckAudience},
		{name: "expired", edit: func(c map[string]any) { c["exp"] = int64(1000) }, check: oidc.CheckTimes},
		{name: "other issuer", edit: func(c map[string]any) { c["iss"] = "https://evil.example.com" }, check: oidc.CheckIssuer},
		{name: "no sub", edit: func(c map[string]any) { delete(c, "sub") }, check: oidc.CheckClaims},
		{name: "wrong verifier", verifier: func(v string) string { return v + "x" }, msg: "invalid_grant"},
		{name: "other redirect URI", redirect: "https://evil.example.com/cb", msg: "invalid_grant"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := oidctest.New(t)
			is.SetPerson(oidctest.GitLabPerson())
			is.EditIDToken(tt.edit)
			p := newProvider(t, is, false)
			nonce := rand.Text()
			u, verifier := p.AuthCodeURL(redirectURI, "s", nonce)
			code, _ := authorize(t, is, u)
			if tt.verifier != nil {
				verifier = tt.verifier(verifier)
			}
			if tt.nonce != nil {
				nonce = tt.nonce(nonce)
			}
			redirect := redirectURI
			if tt.redirect != "" {
				redirect = tt.redirect
			}
			_, err := p.Exchange(context.Background(), code, verifier, redirect, nonce)
			if err == nil {
				t.Fatal("the sign-in passed")
			}
			if tt.check != 0 && err.Check() != tt.check {
				t.Fatalf("err %v, want check %s", err, tt.check)
			}
			if tt.msg != "" && !strings.Contains(err.Message(), tt.msg) {
				t.Fatalf("err %v, want %q", err, tt.msg)
			}
		})
	}
}

func TestProviderWrongSecret(t *testing.T) {
	is := oidctest.New(t)
	is.SetPerson(oidctest.GitLabPerson())
	p := newProvider(t, is, false)
	is.SetClient("writ-client", "rotated")
	nonce := rand.Text()
	u, verifier := p.AuthCodeURL(redirectURI, "s", nonce)
	code, _ := authorize(t, is, u)
	_, err := p.Exchange(context.Background(), code, verifier, redirectURI, nonce)
	if err == nil || !strings.Contains(err.Message(), "invalid_client") {
		t.Fatalf("err %v, want invalid_client", err)
	}
	if strings.Contains(err.Message(), "client-secret-value") {
		t.Fatal("the error holds the client secret")
	}
}

func TestProviderWithoutS256(t *testing.T) {
	is := oidctest.New(t)
	is.SetNoS256(true)
	if p := newProvider(t, is, false); p.S256 {
		t.Fatal("S256 is true for a discovery document that does not list it")
	}
}

func TestVerifierRefusesPeopleIssuer(t *testing.T) {
	is := oidctest.New(t)
	ref := oidc.New(oidc.Options{}).Add(context.Background(), oidc.Issuer{Name: "sso", Kind: oidc.People, URL: is.URL, Client: is.Client()})
	if ref == nil || ref.Reason() != oidc.ReasonPeopleIssuer {
		t.Fatalf("Add took a people issuer: %v", ref)
	}
}
