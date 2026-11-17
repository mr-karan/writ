package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/oidc"
)

// SessionPrefix starts every session token, so a log scanner can find one.
const SessionPrefix = "writs_"

// How a person with a session proves themself, in the audit log: signed
// in to the control UI, or with writ login.
const (
	viaBrowser = "browser session"
	viaCLI     = "cli session"
)

// ErrNoSession means a token names no live session: it never did, it
// ended, or it ran out.
var ErrNoSession = errors.New("no live session")

// Session is a person's session, as the control UI holds it. The token is
// not part of it: only the cookie holds the token, and only its SHA-256
// reaches the control database.
type Session struct {
	ID      string
	Display string
	// Person is the stable id of the person, such as gitlab/user/2002,
	// the same id a CI token that this person started carries when its
	// issuer names the same identity.
	Person  string
	Issuer  string
	Subject string
	Teams   []string
	EndsAt  time.Time
}

// PeopleIssuers lists the issuers people sign in with, in name order.
func (s *Server) PeopleIssuers() []IssuerInfo {
	out := []IssuerInfo{}
	for _, is := range s.cfg.Issuers {
		if is.Kind == oidc.People {
			out = append(out, IssuerInfo{Name: is.Name, Kind: string(is.Kind), URL: is.URL, Identity: is.Identity})
		}
	}
	return out
}

// Dev reports whether this is writ serve --dev.
func (s *Server) Dev() bool { return s.cfg.Dev }

// URL is writ serve's own URL, which the control UI's redirect URI and
// origin checks start from.
func (s *Server) URL() string { return s.cfg.URL }

func (s *Server) provider(name string) (*oidc.Provider, error) {
	for _, p := range s.people {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("issuer %q is not one people sign in with", trunc(name))
}

// AuthURL is where a browser goes to sign in with issuer, and the PKCE
// code verifier that the callback must send back with the code.
func (s *Server) AuthURL(issuer, redirectURI, state, nonce string) (authURL, verifier string, err error) {
	p, err := s.provider(issuer)
	if err != nil {
		return "", "", err
	}
	authURL, verifier = p.AuthCodeURL(redirectURI, state, nonce)
	return authURL, verifier, nil
}

// CallbackRefusal says why the identity provider sent the browser back
// with an error parameter, in Writ's words: the RFC 6749 error code when it
// is one Writ knows, and never the parameter's own text.
func CallbackRefusal(errorParam string) string { return oidc.CallbackRefusal(errorParam).Message() }

// SignIn trades an authorization code for the person's ID token, maps its
// claims to teams, and starts a browser session. It returns the session's
// token, which only the cookie may hold. Every sign-in, allowed or not,
// leaves an audit row: an allowed one commits it with the session, and
// the middleware writes a refused one's before the answer.
func (s *Server) SignIn(ctx context.Context, r *http.Request, issuer, code, verifier, redirectURI, nonce string) (string, *Session, error) {
	now := s.now()
	req := reqOf(r)
	e := &req.entry
	e.Kind, e.Route, e.Issuer, e.PrincipalKind, e.Via, e.Decision = "sign-in", "/ui/callback", issuer, "person", viaBrowser, control.AuthFailed
	// fail records and logs a refused sign-in. why is Writ's own text: a
	// refusal's Message, or the provider lookup's.
	fail := func(check, why string) (string, *Session, error) {
		e.Check, e.Reason = check, why
		s.log.Warn("sign-in refused", "request", e.Request, "issuer", issuer, "check", e.Check, "remote", e.Remote, "reason", why)
		return "", nil, errors.New(why)
	}
	p, err := s.provider(issuer)
	if err != nil {
		return fail("", err.Error())
	}
	tok, ref := p.Exchange(ctx, code, verifier, redirectURI, nonce)
	if ref != nil {
		return fail(ref.Name(), ref.Message())
	}
	row := *e
	row.At, row.Decision, row.Status = now, control.Allowed, http.StatusSeeOther
	token, sess, err := s.startSession(ctx, tok, control.Browser, now, &row)
	if err != nil {
		e.Decision, e.Reason = control.Errored, err.Error()
		return "", nil, err
	}
	req.audited = true
	return token, sess, nil
}

// startSession issues a session to the person tok names, with the teams
// its claims map to, and stores it with e as its audit row. It returns
// the session's token, which only the cookie or the one answer that
// issues it may hold.
func (s *Server) startSession(ctx context.Context, tok *oidc.Token, carrier control.Carrier, now time.Time, e *control.Entry) (string, *Session, error) {
	person := personID(s.cfg.identity(tok.Issuer), tok.Subject)
	if person == "" {
		s.log.Error("a people issuer has no identity", "request", e.Request, "issuer", tok.Issuer)
		return "", nil, errors.New("issuer " + trunc(tok.Issuer) + " has no identity in the config, so it names no person; the request id is " + e.Request)
	}
	sess := &Session{
		ID: newID(), Display: displayOf(tok), Person: person,
		Issuer: tok.Issuer, Subject: tok.Subject, Teams: s.cfg.Policy.TeamsOf(tok.Issuer, tok.Claims), EndsAt: now.Add(s.cfg.SessionTTL),
	}
	if sess.Teams == nil {
		sess.Teams = []string{}
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := SessionPrefix + base64.RawURLEncoding.EncodeToString(raw)
	e.Kind, e.Issuer, e.Subject, e.Person, e.Principal, e.Teams = "sign-in", tok.Issuer, tok.Subject, sess.Person, sess.Display, sess.Teams
	e.PrincipalKind, e.Via = "person", viaOf(carrier)
	e.Reason, e.Elapsed = "session "+sess.ID, s.now().Sub(now)
	row := control.Session{ID: sess.ID, Carrier: carrier, Person: sess.Person, Display: sess.Display, Issuer: sess.Issuer,
		Subject: sess.Subject, Teams: sess.Teams, CreatedAt: now, EndsAt: sess.EndsAt}
	if err := s.store.CreateSession(ctx, row, hashToken(token), *e); err != nil {
		s.log.Error("store a session", "request", e.Request, "err", err)
		return "", nil, errors.New("the control database failed; the request id is " + e.Request)
	}
	s.log.Info("signed in", "request", e.Request, "session", sess.ID, "issuer", tok.Issuer, "person", sess.Person, "teams", sess.Teams, "carrier", carrier)
	return token, sess, nil
}

func viaOf(c control.Carrier) string {
	if c == control.CLI {
		return viaCLI
	}
	return viaBrowser
}

// displayOf is the name a person goes by: the preferred user name, the
// nickname, the name, or the subject.
func displayOf(tok *oidc.Token) string {
	for _, c := range []string{"preferred_username", "nickname", "name"} {
		if v, ok := tok.Claims.String(c); ok && strings.TrimSpace(v) != "" {
			return trunc(v)
		}
	}
	return tok.Subject
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Session returns the live browser session a control UI cookie names, or
// ErrNoSession. A session from writ login opens no page.
func (s *Server) Session(ctx context.Context, token string) (*Session, error) {
	return s.session(ctx, token, control.Browser)
}

// session returns the live session of carrier that token names, or
// ErrNoSession.
func (s *Server) session(ctx context.Context, token string, carrier control.Carrier) (*Session, error) {
	if !strings.HasPrefix(token, SessionPrefix) || len(token) > 128 {
		return nil, ErrNoSession
	}
	row, err := s.store.SessionByHash(ctx, hashToken(token), carrier, s.now())
	if errors.Is(err, control.ErrNotFound) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	return &Session{ID: row.ID, Display: row.Display, Person: row.Person, Issuer: row.Issuer, Subject: row.Subject, Teams: row.Teams, EndsAt: row.EndsAt}, nil
}

// SignOut ends the session a token names, as DELETE /v1/auth/session
// does, and commits the request's audit row with the end.
func (s *Server) SignOut(ctx context.Context, r *http.Request, token string, sess *Session) error {
	now := s.now()
	req := reqOf(r)
	e := &req.entry
	nameSession(e, sess)
	e.Kind, e.Route = "sign-out", "/ui/logout"
	row := *e
	row.At, row.Status, row.Decision, row.Reason = now, http.StatusSeeOther, control.Allowed, "session "+sess.ID
	err := s.store.EndSession(ctx, hashToken(token), sess.Person, now, row)
	switch {
	case errors.Is(err, control.ErrNotFound):
		e.Decision, e.Reason = control.Refused, "the session already ended"
		return ErrNoSession
	case err != nil:
		e.Decision, e.Reason = control.Errored, "end session "+sess.ID+": "+err.Error()
		return fmt.Errorf("the control database failed; the request id is %s", req.id)
	}
	req.audited = true
	s.log.Info("signed out", "request", req.id, "session", sess.ID, "person", sess.Person)
	return nil
}

// Attribute names the person of a control UI session on the request's
// audit row.
func (s *Server) Attribute(r *http.Request, sess *Session) { nameSession(&reqOf(r).entry, sess) }

// Allow counts one page view of a session against the rate limit the API
// applies to each principal.
func (s *Server) Allow(sess *Session) bool { return s.limiter.allow(sess.Person) }

type sessionKey struct{}

// Call answers GET target, a /v1 path and query or /healthz, for a person signed in to
// the control UI, as the API answers a bearer token: the same handler, the
// same grants, the same document, and an audit row whose via is a browser
// session.
func (s *Server) Call(r *http.Request, sess *Session, target string) (int, []byte) {
	return s.call(r, sess, http.MethodGet, target, nil)
}

// Post sends body to POST target for a person signed in to the control UI,
// as Call sends a GET: the control UI's forms change nothing the API
// would refuse.
func (s *Server) Post(r *http.Request, sess *Session, target string, body []byte) (int, []byte) {
	return s.call(r, sess, http.MethodPost, target, body)
}

func (s *Server) call(r *http.Request, sess *Session, method, target string, body []byte) (int, []byte) {
	if !strings.HasPrefix(target, "/v1/") && target != "/healthz" {
		return http.StatusBadRequest, []byte(`{"format":"writ.error/v0","ok":false,"diagnostics":[{"severity":"error","class":"usage","message":"the control UI calls /v1 routes only"}]}`)
	}
	ctx := context.WithValue(r.Context(), sessionKey{}, sess)
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return http.StatusBadRequest, []byte(`{"format":"writ.error/v0","ok":false,"diagnostics":[{"severity":"error","class":"usage","message":"the request is not a path"}]}`)
	}
	req.RemoteAddr = r.RemoteAddr
	req.Header["X-Forwarded-For"] = r.Header.Values("X-Forwarded-For")
	rec := &recorder{header: http.Header{}}
	s.audited.ServeHTTP(rec, req)
	return rec.status, rec.body.Bytes()
}

// recorder holds an answer that Call returns instead of sending, or
// that audit sends once the request's audit row commits. Once out is set,
// the answer goes straight there.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	out    http.ResponseWriter
}

func (r *recorder) Header() http.Header {
	if r.out != nil {
		return r.out.Header()
	}
	return r.header
}

func (r *recorder) WriteHeader(code int) {
	if r.out != nil {
		r.out.WriteHeader(code)
		return
	}
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.out != nil {
		return r.out.Write(b)
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// stream sends what r holds to out, and from then on every write too.
func (r *recorder) stream(out http.ResponseWriter) {
	maps.Copy(out.Header(), r.header)
	if r.status != 0 {
		out.WriteHeader(r.status)
	}
	if r.body.Len() > 0 {
		_, _ = out.Write(r.body.Bytes())
	}
	r.out, r.header, r.body = out, nil, bytes.Buffer{}
}

// sessionAuth takes the principal from a session Call put in the context.
// No request from the network can carry one.
func (s *Server) sessionAuth(r *http.Request, req *request) bool {
	sess, ok := r.Context().Value(sessionKey{}).(*Session)
	if !ok {
		return false
	}
	req.principal = nameSession(&req.entry, sess)
	return true
}

// nameSession names the person of a control UI session on e and returns
// the session's principal.
func nameSession(e *control.Entry, sess *Session) *principal {
	p := sessionPrincipal(sess, viaBrowser)
	e.Issuer, e.Subject, e.Person, e.Principal, e.Teams = sess.Issuer, sess.Subject, sess.Person, sess.Display, sess.Teams
	e.PrincipalKind, e.Via = p.info.Kind, p.via()
	return p
}
