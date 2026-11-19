package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Provider is a people issuer: the team's identity provider, which a
// person signs in with through the authorization code flow with PKCE, or
// writ login's device flow. writ serve is a confidential client of it, so
// the browser and the CLI never see the client secret, an access token, or
// an ID token.
type Provider struct {
	Name     string
	URL      string
	ClientID string
	// S256 is true when the discovery document lists S256 in
	// code_challenge_methods_supported. The challenge is sent either way.
	S256 bool

	oauth  oauth2.Config
	device bool
	groups bool
	client *http.Client
	// deviceClient is client with the client's id and secret on every
	// request, for the device authorization endpoint.
	deviceClient *http.Client
	discovered   *gooidc.Provider
	keys         *keySet
	now          func() time.Time
}

// ProviderOptions configures NewProvider.
type ProviderOptions struct {
	Name, URL, ClientID, ClientSecret string
	// GroupsFromUserinfo reads the groups claim from the userinfo endpoint
	// at sign-in. GitLab puts only groups_direct in its ID token.
	GroupsFromUserinfo bool
	// Client fetches discovery, keys, tokens, and userinfo. Nil trusts the
	// system roots. Redirects are refused either way.
	Client *http.Client
	Now    func() time.Time
	Log    *slog.Logger
}

// scopes are what a sign-in asks the identity provider for.
var scopes = []string{"openid", "profile", "email"}

// NewProvider fetches the provider's discovery document and key set. It
// fails when either cannot be fetched or the document lacks an endpoint
// the flow needs. The refusal does not name the issuer: the caller knows
// which one it asked for.
func NewProvider(ctx context.Context, o ProviderOptions) (*Provider, *Refusal) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if !issuerURL(o.URL) {
		return nil, refuse(StageConfig, ReasonIssuerURL)
	}
	if o.ClientID == "" || o.ClientSecret == "" {
		return nil, refuse(StageConfig, ReasonNoClient)
	}
	client := noRedirects(o.Client)
	doc, ref := discover(ctx, client, o.URL)
	if ref != nil {
		return nil, ref
	}
	ep := doc.provider.Endpoint()
	need := []struct {
		field Field
		url   string
	}{{FieldAuthorizationEndpoint, ep.AuthURL}, {FieldTokenEndpoint, ep.TokenURL}}
	if o.GroupsFromUserinfo {
		need = append(need, struct {
			field Field
			url   string
		}{FieldUserinfoEndpoint, doc.provider.UserInfoEndpoint()})
	}
	if doc.Device != "" {
		need = append(need, struct {
			field Field
			url   string
		}{FieldDeviceEndpoint, doc.Device})
	}
	for _, n := range need {
		if pu, ok := httpsURL(n.url); !ok || pu.Fragment != "" {
			return nil, refuse(StageDiscovery, ReasonNotHTTPS).of(n.field)
		}
	}
	ks := &keySet{issuer: o.Name, jwksURL: doc.JWKSURI, client: client, now: o.Now, log: o.Log}
	if ref := ks.refresh(ctx); ref != nil {
		return nil, ref
	}
	ep.DeviceAuthURL = doc.Device
	// HTTP Basic, form-encoded first as RFC 6749 section 2.3.1 says.
	ep.AuthStyle = oauth2.AuthStyleInHeader
	deviceClient := *client
	deviceClient.Transport = clientAuth{base: client.Transport, id: o.ClientID, secret: o.ClientSecret}
	return &Provider{
		Name: o.Name, URL: o.URL, ClientID: o.ClientID, S256: slices.Contains(doc.Methods, "S256"),
		oauth:  oauth2.Config{ClientID: o.ClientID, ClientSecret: o.ClientSecret, Endpoint: ep, Scopes: scopes},
		device: doc.Device != "", groups: o.GroupsFromUserinfo, client: client, deviceClient: &deviceClient,
		discovered: doc.provider, keys: ks, now: o.Now,
	}, nil
}

// clientAuth sends the client's id and secret in HTTP Basic
// authentication on every request. golang.org/x/oauth2 v0.37.0 sends only
// client_id to the device authorization endpoint, and a confidential
// client authenticates there as it does at the token endpoint (RFC 8628,
// section 3.1).
type clientAuth struct {
	base       http.RoundTripper
	id, secret string
}

func (c clientAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.SetBasicAuth(url.QueryEscape(c.id), url.QueryEscape(c.secret))
	return c.base.RoundTrip(req)
}

// with carries client in ctx, where go-oidc and oauth2 look for it, and
// records the call to the provider's endpoint, for remoteRefusal.
func with(ctx context.Context, client *http.Client) (context.Context, *call) {
	client, c := observe(client)
	return gooidc.ClientContext(ctx, client), c
}

// AuthCodeURL is where the browser goes to sign in: the authorization
// endpoint with the client, the redirect URI, state, nonce, and the S256
// challenge of a new PKCE code verifier, which it returns for the
// callback. The verifier never leaves writ serve's sign-in cookie.
func (p *Provider) AuthCodeURL(redirectURI, state, nonce string) (authURL, verifier string) {
	verifier = oauth2.GenerateVerifier()
	return p.ChallengeURL(redirectURI, state, nonce, oauth2.S256ChallengeFromVerifier(verifier)), verifier
}

// ChallengeURL is AuthCodeURL for a verifier the caller does not hold:
// writ login's, which reaches writ serve only with the code.
func (p *Provider) ChallengeURL(redirectURI, state, nonce, challenge string) string {
	cfg := p.redirecting(redirectURI)
	return cfg.AuthCodeURL(state, oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("code_challenge", challenge), gooidc.Nonce(nonce))
}

// redirecting is the provider's client config with redirectURI. It names
// the fields rather than copying p.oauth, whose auth-style cache a device
// login may be writing at the same time.
func (p *Provider) redirecting(redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.oauth.ClientID,
		ClientSecret: p.oauth.ClientSecret,
		Endpoint:     p.oauth.Endpoint,
		Scopes:       p.oauth.Scopes,
		RedirectURL:  redirectURI,
	}
}

// Exchange trades an authorization code for the provider's ID token, checks
// the token, its audience, and its nonce, and reads the groups from
// userinfo when the provider is set up to. The token never leaves this
// function: the caller gets its verified claims.
func (p *Provider) Exchange(ctx context.Context, code, verifier, redirectURI, nonce string) (*Token, *Refusal) {
	cfg := p.redirecting(redirectURI)
	cctx, c := with(ctx, p.client)
	tok, err := cfg.Exchange(cctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, remoteRefusal(StageTokenEndpoint, c, err)
	}
	return p.finish(ctx, tok, &nonce)
}

// finish checks the ID token in tok and reads the groups from userinfo
// when the provider is set up to. nonce is nil for the device flow, which
// sends none.
func (p *Provider) finish(ctx context.Context, tok *oauth2.Token, nonce *string) (*Token, *Refusal) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, refuse(StageTokenEndpoint, ReasonNoIDToken)
	}
	id, ref := p.verify(ctx, raw, nonce)
	if ref != nil {
		return nil, ref
	}
	if p.groups {
		groups, ref := p.userGroups(ctx, tok, id.Subject)
		if ref != nil {
			return nil, ref
		}
		id.Claims["groups"] = groups
	}
	return id, nil
}

// verify checks an ID token in the order Verify checks a CI token, with
// the audience rule of a people issuer and, when nonce is not nil, the
// nonce.
func (p *Provider) verify(ctx context.Context, raw string, nonce *string) (*Token, *Refusal) {
	iss, ref := screenIssuer(raw)
	if ref != nil {
		return nil, ref
	}
	if iss != p.URL {
		return nil, failed(CheckIssuer, ReasonOtherIssuer)
	}
	tok := &Token{Issuer: p.Name, Kind: People, URL: p.URL}
	return verifySigned(ctx, raw, p.ClientID, p.keys, p.now, tok, func(tok *Token) *Refusal {
		if ref := p.checkAudience(tok.Claims); ref != nil {
			return ref
		}
		if nonce != nil {
			got, _ := tok.Claims.String("nonce")
			if *nonce == "" || subtle.ConstantTimeCompare([]byte(got), []byte(*nonce)) != 1 {
				return failed(CheckNonce, ReasonNonce)
			}
		}
		sub, ok := tok.Claims.String("sub")
		if !ok || sub == "" {
			return failed(CheckClaims, ReasonClaimMissing).of(FieldSub)
		}
		tok.Subject = sub
		tok.ID, _ = tok.Claims.String("jti")
		return nil
	})
}

// checkAudience accepts an aud that holds the client id. When aud names
// other audiences too, azp must be present, and azp, when present, must be
// the client id (OpenID Connect Core 1.0, section 3.1.3.7).
func (p *Provider) checkAudience(c Claims) *Refusal {
	auds, ok := c.Values("aud")
	if !ok {
		return failed(CheckAudience, ReasonMissing).of(FieldAud)
	}
	if !slices.Contains(auds, p.ClientID) {
		return failed(CheckAudience, ReasonAudNoClient)
	}
	azp, hasAzp := c.String("azp")
	if len(auds) > 1 && !hasAzp {
		return failed(CheckAudience, ReasonAzpMissing).counted(int64(len(auds)), 1)
	}
	if hasAzp && azp != p.ClientID {
		return failed(CheckAudience, ReasonAzpOther)
	}
	return nil
}

// userGroups reads the groups claim from userinfo through go-oidc. The
// answer's sub must be the ID token's (OpenID Connect Core 1.0, section
// 5.3.2).
func (p *Provider) userGroups(ctx context.Context, tok *oauth2.Token, sub string) (json.RawMessage, *Refusal) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	cctx, c := with(ctx, p.client)
	info, err := p.discovered.UserInfo(cctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return nil, remoteRefusal(StageUserinfo, c, err)
	}
	if info.Subject != sub {
		return nil, failed(CheckClaims, ReasonUserinfoSubject)
	}
	var claims map[string]json.RawMessage
	if err := info.Claims(&claims); err != nil {
		return nil, refuse(StageUserinfo, ReasonNotJSON)
	}
	groups, ok := claims["groups"]
	if !ok {
		return json.RawMessage("[]"), nil
	}
	return groups, nil
}

// Health reports the provider's key set, for /healthz.
func (p *Provider) Health() Health {
	good, off, ok := p.keys.status()
	return Health{Issuer: p.Name, GoodAt: good, Offset: off, HasOffset: ok}
}
