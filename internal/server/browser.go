package server

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/oidc"
)

// writ login in a browser, for an identity provider with no device flow,
// such as GitLab.com: the authorization code flow with PKCE (RFC 7636) and
// a loopback redirect (RFC 8252). writ serve stays the OAuth client, as in
// the device flow. The CLI listens on 127.0.0.1, makes the state and the
// code verifier, and sends the challenge to POST /v1/auth/browser, which
// answers the URL to open and a handle. The identity provider sends the
// browser back to the CLI with the code, and the CLI sends the code, the
// state, and the verifier to POST /v1/auth/browser/token. writ serve trades
// the code with its client secret, checks the ID token and the nonce it
// made, and issues the same session the device flow issues. The handle
// works once.

// browserLoginTTL is how long a browser sign-in may take, as the control
// UI's sign-in may.
const browserLoginTTL = 10 * time.Minute

// CallbackPath is the path of writ login's loopback redirect URI:
// http://127.0.0.1:PORT/callback, with the port the CLI listens on.
const CallbackPath = "/callback"

// browserLogin is one browser sign-in that waits for its callback.
type browserLogin struct {
	provider                             *oidc.Provider
	source                               string
	redirectURI, state, nonce, challenge string
	expires                              time.Time
}

// BrowserRequest is the body of POST /v1/auth/browser. Issuer may be
// empty when writ serve has one people issuer.
type BrowserRequest struct {
	Issuer string `json:"issuer"`
	// RedirectURI is http://127.0.0.1:PORT/callback, where the CLI
	// listens.
	RedirectURI string `json:"redirect_uri"`
	State       string `json:"state"`
	// CodeChallenge is the S256 challenge of the CLI's code verifier.
	CodeChallenge string `json:"code_challenge"`
}

// Browser is the answer of POST /v1/auth/browser.
type Browser struct {
	Format string `json:"format"`
	OK     bool   `json:"ok"`
	Issuer string `json:"issuer"`
	// AuthURL is the identity provider's sign-in page for this sign-in.
	AuthURL string `json:"auth_url"`
	// Handle names the sign-in in POST /v1/auth/browser/token. It works
	// once.
	Handle      string       `json:"handle"`
	ExpiresAt   string       `json:"expires_at"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// BrowserTokenRequest is the body of POST /v1/auth/browser/token: what
// the identity provider sent to the loopback redirect, and the code
// verifier.
type BrowserTokenRequest struct {
	Handle       string `json:"handle"`
	Code         string `json:"code"`
	State        string `json:"state"`
	CodeVerifier string `json:"code_verifier"`
}

// loopbackRedirect reports whether uri is http://127.0.0.1:PORT/callback,
// spelled exactly so, with a port of 1 to 65535.
func loopbackRedirect(uri string) bool {
	rest, ok := strings.CutPrefix(uri, "http://127.0.0.1:")
	if !ok {
		return false
	}
	port, ok := strings.CutSuffix(rest, CallbackPath)
	n, err := strconv.Atoi(port)
	return ok && err == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == port
}

// unreserved reports whether s is min to max characters of the URI's
// unreserved set, as RFC 7636 asks of a code verifier.
func unreserved(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, c := range []byte(s) {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

// browserProvider picks the people issuer a browser sign-in uses: the one
// named, or the only one.
func (s *Server) browserProvider(name string) (*oidc.Provider, error) {
	switch {
	case s.cfg.Dev && len(s.people) == 0:
		return nil, errors.New("writ serve --dev needs no sign-in: the CLI reads its token from client.json in the data directory")
	case len(s.people) == 0:
		return nil, errors.New("writ serve has no people issuer, so writ login is not set up here; the operator adds an issuer of kind people")
	case name == "" && len(s.people) == 1:
		return s.people[0], nil
	case name == "":
		names := make([]string, len(s.people))
		for i, p := range s.people {
			names[i] = p.Name
		}
		slices.Sort(names)
		return nil, errors.New("writ serve signs people in with " + strings.Join(names, ", ") + ": name one with writ login --issuer")
	}
	return s.provider(name)
}

// startBrowser is POST /v1/auth/browser.
func (s *Server) startBrowser(w http.ResponseWriter, r *http.Request) {
	if !s.allowAnonymous(w, r, FormatBrowser) {
		return
	}
	var body BrowserRequest
	if err := s.readBody(r, &body); err != nil {
		s.refuse(w, r, FormatBrowser, diag.Usage, http.StatusBadRequest, "%v", err)
		return
	}
	switch {
	case !loopbackRedirect(body.RedirectURI):
		s.refuse(w, r, FormatBrowser, diag.Usage, http.StatusBadRequest, "redirect_uri %q is not http://127.0.0.1:PORT%s: writ login takes the identity provider's answer on loopback alone (RFC 8252)", trunc(body.RedirectURI), CallbackPath)
		return
	case !unreserved(body.State, 22, 128):
		s.refuse(w, r, FormatBrowser, diag.Usage, http.StatusBadRequest, "state must be 22 to 128 random characters of A-Z, a-z, 0-9, and -._~")
		return
	case !unreserved(body.CodeChallenge, 43, 43):
		s.refuse(w, r, FormatBrowser, diag.Usage, http.StatusBadRequest, "code_challenge must be the S256 challenge of a PKCE code verifier, 43 characters (RFC 7636)")
		return
	}
	p, err := s.browserProvider(body.Issuer)
	if err != nil {
		s.refuse(w, r, FormatBrowser, diag.Usage, http.StatusNotFound, "%v", err)
		return
	}
	req := reqOf(r)
	req.entry.Issuer, req.entry.Kind = p.Name, "sign-in-started"
	now := s.now()
	handle := random32()
	l := &browserLogin{provider: p, source: s.clientKey(r), redirectURI: body.RedirectURI, state: body.State, nonce: random32(), challenge: body.CodeChallenge, expires: now.Add(browserLoginTTL)}
	key := handleKey(handle)
	s.mu.Lock()
	s.sweepLogins(now)
	full := s.loginsFull(len(s.browserLogins), l.source)
	if full == "" {
		s.browserLogins[key] = l
	}
	s.mu.Unlock()
	if full != "" {
		s.refuse(w, r, FormatBrowser, diag.Usage, http.StatusTooManyRequests, "%s", full)
		return
	}
	req.entry.Decision, req.entry.Reason = control.Allowed, "browser sign-in started at issuer "+p.Name
	if !s.commitStart(r, req) {
		s.mu.Lock()
		delete(s.browserLogins, key)
		s.mu.Unlock()
		return
	}
	s.log.Info("browser sign-in started", "request", req.id, "issuer", p.Name, "remote", req.entry.Remote, "expires", stamp(l.expires))
	s.reply(w, http.StatusOK, Browser{
		Format: FormatBrowser, OK: true, Issuer: p.Name, AuthURL: p.ChallengeURL(l.redirectURI, l.state, l.nonce, l.challenge),
		Handle: handle, ExpiresAt: stamp(l.expires), Diagnostics: []Diagnostic{},
	})
}

// finishBrowser is POST /v1/auth/browser/token. The handle ends at the
// first request that names it, whatever the request's fate.
func (s *Server) finishBrowser(w http.ResponseWriter, r *http.Request) {
	if !s.allowAnonymous(w, r, FormatSession) {
		return
	}
	var body BrowserTokenRequest
	if err := s.readBody(r, &body); err != nil {
		s.refuse(w, r, FormatSession, diag.Usage, http.StatusBadRequest, "%v", err)
		return
	}
	req := reqOf(r)
	key := handleKey(body.Handle)
	now := s.now()
	s.mu.Lock()
	l := s.browserLogins[key]
	delete(s.browserLogins, key)
	s.mu.Unlock()
	switch {
	case l == nil || body.Handle == "":
		s.authFail(w, r, FormatSession, "handle", "the sign-in is unknown, finished, or ran out: run writ login again")
		return
	case !now.Before(l.expires):
		s.authFail(w, r, FormatSession, "handle", "the sign-in ran out before the identity provider sent the browser back: run writ login again")
		return
	}
	req.entry.Issuer = l.provider.Name
	switch {
	case subtle.ConstantTimeCompare([]byte(body.State), []byte(l.state)) != 1:
		s.authFail(w, r, FormatSession, "state", "the callback's state is not the one the sign-in started with: run writ login again")
		return
	case subtle.ConstantTimeCompare([]byte(oidc.Challenge(body.CodeVerifier)), []byte(l.challenge)) != 1:
		s.authFail(w, r, FormatSession, "pkce", "the sign-in sent no PKCE code verifier, or not the one whose challenge started it (RFC 7636): run writ login again")
		return
	case body.Code == "":
		s.authFail(w, r, FormatSession, "code", "the callback carried no authorization code: run writ login again")
		return
	}
	tok, ref := l.provider.Exchange(r.Context(), body.Code, body.CodeVerifier, l.redirectURI, l.nonce)
	if ref != nil {
		s.oidcRefused(w, r, FormatSession, "the sign-in failed: ", ref)
		return
	}
	s.issueCLISession(w, r, tok, now)
}
