package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
)

// toStandIn sends every request, whatever host its URL names, to the
// stand-in issuer on loopback, so an issuer URL may be any spelling of a
// host and no request leaves the machine.
type toStandIn struct {
	base   http.RoundTripper
	target *url.URL
}

func (s toStandIn) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = s.target.Scheme, s.target.Host
	clone.URL, clone.Host = &u, s.target.Host
	return s.base.RoundTrip(clone)
}

// verifiedPair verifies a GitLab CI token of user 1001 from an issuer at
// ciURL, and signs user 1001 in, with the code flow, at a people issuer
// at peopleURL. Both URLs reach one stand-in on loopback.
func verifiedPair(t *testing.T, ciURL, peopleURL string) (ci, person *oidc.Token) {
	t.Helper()
	ctx := context.Background()
	is := oidctest.New(t)
	target, err := url.Parse(is.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := is.Client()
	client.Transport = toStandIn{base: client.Transport, target: target}

	is.URL = ciURL
	v := oidc.New(oidc.Options{})
	if ref := v.Add(ctx, oidc.Issuer{Name: "gitlab", Kind: oidc.GitLabCI, URL: ciURL, Audience: "https://writ.example.com", Client: client}); ref != nil {
		t.Fatal(ref)
	}
	job := oidctest.GitLabJob(ciURL, "https://writ.example.com", time.Now())
	job["user_id"] = "1001"
	ci, ref := v.Verify(ctx, oidctest.Sign(is.Keys()[0], job))
	if ref != nil {
		t.Fatal(ref)
	}

	is.URL = peopleURL
	is.SetClient("writ-client", "synthetic-client-secret")
	claims, userinfo := oidctest.GitLabPerson()
	claims["sub"], userinfo["sub"] = "1001", "1001"
	is.SetPerson(claims, userinfo)
	p, ref := oidc.NewProvider(ctx, oidc.ProviderOptions{Name: "sso", URL: peopleURL, ClientID: "writ-client", ClientSecret: "synthetic-client-secret", Client: client})
	if ref != nil {
		t.Fatal(ref)
	}
	const redirect = "https://writ.example.com/ui/callback"
	auth, verifier := p.AuthCodeURL(redirect, "state", "nonce")
	browser := *client
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := browser.Get(auth)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("the stand-in's authorize answered %d: %v", resp.StatusCode, err)
	}
	person, ref = p.Exchange(ctx, back.Query().Get("code"), verifier, redirect, "nonce")
	if ref != nil {
		t.Fatal(ref)
	}
	return ci, person
}

// selfApproval makes a pending plan as the CI token's principal, signs the
// person in, and approves the plan as that session. It returns both
// person ids and the store's answer.
func selfApproval(t *testing.T, cfg *Config, ciTok, personTok *oidc.Token) (author, approver string, err error) {
	t.Helper()
	ctx := context.Background()
	store, _ := controltest.Open(t, controltest.Default())
	now := time.Now()
	cfg.SessionTTL = time.Hour
	s := &Server{store: store, cfg: cfg, now: time.Now, log: slog.New(slog.DiscardHandler)}
	ci := newPrincipal(ciTok, cfg.identity(ciTok.Issuer), &cfg.Policy)
	cfgID, err := store.RecordConfig(ctx, "{}", "https://writ.example.com", "test", &cfg.Policy, now)
	if err != nil {
		t.Fatal(err)
	}
	e := control.Entry{At: now, Kind: "test", Decision: control.Allowed, ConfigID: cfgID}
	pl := control.Plan{Ref: "PLAN", Env: "prod", Stack: "network", ConfigID: cfgID, RequestedAt: now, SourceDigest: "digest",
		Author: control.Principal{Issuer: ciTok.Issuer, Person: ci.info.Person, Detail: ci.detail()}, Origin: ci.origin(Hints{})}
	if err := store.CreatePlan(ctx, pl, control.TokenUse{}, e); err != nil {
		t.Fatal(err)
	}
	id := "11111111111111111111111111111111"
	if err := store.FinishPlan(ctx, pl.Ref, control.PlanResult{ID: id, Status: control.Pending, MadeAt: now, ExpiresAt: now.Add(time.Hour), Need: 1, Saved: []byte("saved")}, e); err != nil {
		t.Fatal(err)
	}
	_, sess, err := s.startSession(ctx, personTok, control.CLI, now, &e)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Approve(ctx, control.Approval{Person: sess.Person, Display: "alice", ApprovedAt: now}, []string{id}, control.Rules{Counts: func(string) bool { return true }, Need: func(control.Plan) int { return 1 }}, control.TokenUse{}, e)
	return ci.info.Person, sess.Person, err
}

func issuers(ciURL, ciIdentity, peopleURL, peopleIdentity string) *Config {
	return &Config{Issuers: []Issuer{
		{Name: "gitlab", Kind: oidc.GitLabCI, URL: ciURL, Identity: ciIdentity},
		{Name: "sso", Kind: oidc.People, URL: peopleURL, Identity: peopleIdentity},
	}}
}

// Two issuers of one GitLab whose URLs spell its host two ways name one
// person, because a person id holds the identity the config gives them,
// not a host. Before identities, each pair gave two ids, such as
// xn--bcher-kva.example/user/1001 and bücher.example/user/1001, and the
// author approved their own plan.
func TestOneIdentityOnePerson(t *testing.T) {
	for _, tt := range []struct{ name, ci, people string }{
		{"case", "https://gitlab.example.com", "https://GITLAB.example.com"},
		{"default port", "https://gitlab.example.com", "https://gitlab.example.com:443"},
		{"default port with zeros", "https://gitlab.example.com", "https://gitlab.example.com:0443"},
		{"port with zeros", "https://gitlab.example.com:8443", "https://gitlab.example.com:08443"},
		{"trailing dot", "https://gitlab.example.com", "https://gitlab.example.com."},
		{"idn", "https://xn--bcher-kva.example", "https://bücher.example"},
		{"ipv6", "https://[2001:db8::1]", "https://[2001:0db8:0000:0000:0000:0000:0000:0001]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ciTok, personTok := verifiedPair(t, tt.ci, tt.people)
			author, approver, err := selfApproval(t, issuers(tt.ci, "gitlab-main", tt.people, "gitlab-main"), ciTok, personTok)
			if author != "gitlab-main/user/1001" || approver != author {
				t.Errorf("the CI job's person is %q and the session's %q; want gitlab-main/user/1001 for both", author, approver)
			}
			var ae *control.ApprovalError
			if !errors.As(err, &ae) || !ae.Self {
				t.Errorf("the author approved their own plan through %s: %v", tt.people, err)
			}
		})
	}
}

// Issuers of two identities name two people, even at one URL: the config,
// not the URL, says whose user ids a token carries.
func TestTwoIdentitiesTwoPeople(t *testing.T) {
	const u = "https://gitlab.example.com"
	ciTok, personTok := verifiedPair(t, u, u)
	author, approver, err := selfApproval(t, issuers(u, "gitlab-main", u, "gitlab-staging"), ciTok, personTok)
	if author != "gitlab-main/user/1001" || approver != "gitlab-staging/user/1001" {
		t.Errorf("persons %q and %q; want gitlab-main/user/1001 and gitlab-staging/user/1001", author, approver)
	}
	if err != nil {
		t.Errorf("a person of another identity could not approve: %v", err)
	}
}

// A token of an issuer the config gives no identity names no person, and
// such an issuer starts no session.
func TestNoIdentityNoPerson(t *testing.T) {
	const u = "https://gitlab.example.com"
	ciTok, personTok := verifiedPair(t, u, u)
	if p := newPrincipal(ciTok, "", &authz.Policy{}); p.info.Person != "" {
		t.Errorf("a token without an identity names person %q", p.info.Person)
	}
	s := &Server{cfg: issuers(u, "", u, ""), now: time.Now, log: slog.New(slog.DiscardHandler)}
	if _, _, err := s.startSession(context.Background(), personTok, control.CLI, time.Now(), &control.Entry{Request: "R"}); err == nil {
		t.Error("an issuer without an identity started a session")
	}
}
