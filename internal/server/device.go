package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/oidc"
)

// writ login: the device flow that writ serve brokers. POST
// /v1/auth/device starts a sign-in at a people issuer and returns the user
// code and a handle. From then on one goroutine polls the identity
// provider's token endpoint, through golang.org/x/oauth2, at the interval
// the provider sets, whatever the client does. POST /v1/auth/device/token
// reads what that goroutine found, with the handle, and returns a session
// once the person signs in. The device code stays in this process, and the
// handle works once.

// maxLogins bounds the sign-ins of each flow that wait for a person at
// once, those still starting included.
const maxLogins = 1000

// maxLoginsPerSource bounds the sign-ins of both flows that wait at once
// for one client address, as the address limit counts it, so one address
// cannot take the places of maxLogins. A person needs one at a time, and
// a sign-in that is used ends within a minute, so 10 leaves room for an
// office behind one NAT address. Many addresses can still fill maxLogins.
const maxLoginsPerSource = 10

// login is one sign-in that waits for its person. A login whose cancel is
// nil is still starting: it holds its place under maxLogins while writ
// serve asks the identity provider for a code.
type login struct {
	provider *oidc.Provider
	// source is the client address that started it, as clientKey reads it.
	source   string
	interval time.Duration
	expires  time.Time
	cancel   context.CancelFunc
	// done is set when the poller ended, with tok or ref.
	done bool
	tok  *oidc.Token
	ref  *oidc.Refusal
}

// DeviceRequest is the body of POST /v1/auth/device. Issuer may be empty
// when writ serve has one people issuer with the device flow.
type DeviceRequest struct {
	Issuer string `json:"issuer"`
}

// Device is the answer of POST /v1/auth/device.
type Device struct {
	Format string `json:"format"`
	OK     bool   `json:"ok"`
	Issuer string `json:"issuer"`
	// UserCode is what the person types at VerificationURI.
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	// Handle names the sign-in in POST /v1/auth/device/token. It works
	// once and dies with the device code.
	Handle          string       `json:"handle"`
	IntervalSeconds int          `json:"interval_seconds"`
	ExpiresAt       string       `json:"expires_at"`
	Diagnostics     []Diagnostic `json:"diagnostics"`
}

// DeviceTokenRequest is the body of POST /v1/auth/device/token.
type DeviceTokenRequest struct {
	Handle string `json:"handle"`
}

// Session statuses in writ.session/v0.
const (
	SessionPending  = "pending"
	SessionSignedIn = "signed-in"
	SessionEnded    = "ended"
)

// SessionDoc is the answer of POST /v1/auth/device/token and DELETE
// /v1/auth/session. Token is set in one answer only: the poll that
// issues the session.
type SessionDoc struct {
	Format string `json:"format"`
	OK     bool   `json:"ok"`
	// Status is pending, signed-in, or ended.
	Status string `json:"status"`
	// IntervalSeconds is how long to wait before the next poll, while
	// pending.
	IntervalSeconds int          `json:"interval_seconds,omitempty"`
	Token           string       `json:"token,omitempty"`
	Session         *SessionInfo `json:"session,omitempty"`
	Diagnostics     []Diagnostic `json:"diagnostics"`
}

// SessionInfo is a session as the API shows it. It never holds the token.
type SessionInfo struct {
	ID         string   `json:"id"`
	Carrier    string   `json:"carrier"`
	Person     string   `json:"person"`
	Display    string   `json:"display"`
	Issuer     string   `json:"issuer"`
	Subject    string   `json:"subject"`
	Teams      []string `json:"teams"`
	CreatedAt  string   `json:"created_at,omitempty"`
	EndsAt     string   `json:"ends_at"`
	LastSeenAt string   `json:"last_seen_at,omitempty"`
}

// Sessions is GET /v1/sessions and DELETE /v1/sessions/{id}.
type Sessions struct {
	Format      string        `json:"format"`
	OK          bool          `json:"ok"`
	Sessions    []SessionInfo `json:"sessions"`
	Diagnostics []Diagnostic  `json:"diagnostics"`
}

func sessionInfo(sess control.Session) SessionInfo {
	return SessionInfo{
		ID: sess.ID, Carrier: string(sess.Carrier), Person: sess.Person, Display: sess.Display, Issuer: sess.Issuer, Subject: sess.Subject,
		Teams: orEmpty(sess.Teams), CreatedAt: stamp(sess.CreatedAt), EndsAt: stamp(sess.EndsAt), LastSeenAt: stamp(sess.LastSeenAt),
	}
}

func handleKey(handle string) [32]byte { return sha256.Sum256([]byte(handle)) }

// loginProvider picks the people issuer a sign-in uses: the one named, or
// the only one with the device flow.
func (s *Server) loginProvider(name string) (*oidc.Provider, error) {
	var device []string
	for _, p := range s.people {
		if p.Device() {
			device = append(device, p.Name)
		}
	}
	slices.Sort(device)
	switch {
	case s.cfg.Dev && len(s.people) == 0:
		return nil, errors.New("writ serve --dev needs no sign-in: the CLI reads its token from client.json in the data directory")
	case len(s.people) == 0:
		return nil, errors.New("writ serve has no people issuer, so writ login is not set up here; the operator adds an issuer of kind people")
	case name == "" && len(device) == 1:
		name = device[0]
	case name == "" && len(device) == 0:
		return nil, errors.New("no people issuer of writ serve offers the device flow: the identity provider's discovery document names no device_authorization_endpoint; run writ login without --device to sign in through a browser")
	case name == "":
		return nil, errors.New("writ serve signs people in with " + strings.Join(device, ", ") + ": name one with writ login --issuer")
	}
	p, err := s.provider(name)
	if err != nil {
		return nil, err
	}
	if !p.Device() {
		return nil, errors.New("issuer " + p.Name + ": the identity provider's discovery document names no device_authorization_endpoint, so it offers no device flow; run writ login without --device to sign in through a browser")
	}
	return p, nil
}

// allowAnonymous counts an unauthenticated request against the rate limit
// of its remote address, and answers 429 past it.
func (s *Server) allowAnonymous(w http.ResponseWriter, r *http.Request, format string) bool {
	if s.limiter.allow("remote:" + s.clientKey(r)) {
		return true
	}
	s.refuse(w, r, format, diag.Usage, http.StatusTooManyRequests, "more than %d requests a second from %s", s.cfg.Limits.RequestsPerSecond, s.remoteOf(r))
	return false
}

// startDevice is POST /v1/auth/device.
func (s *Server) startDevice(w http.ResponseWriter, r *http.Request) {
	if !s.allowAnonymous(w, r, FormatDevice) {
		return
	}
	var body DeviceRequest
	if r.ContentLength != 0 {
		if err := s.readBody(r, &body); err != nil {
			s.refuse(w, r, FormatDevice, diag.Usage, http.StatusBadRequest, "%v", err)
			return
		}
	}
	p, err := s.loginProvider(body.Issuer)
	if err != nil {
		s.refuse(w, r, FormatDevice, diag.Usage, http.StatusNotFound, "%v", err)
		return
	}
	req := reqOf(r)
	req.entry.Issuer, req.entry.Kind = p.Name, "sign-in-started"
	now := s.now()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	handle := base64.RawURLEncoding.EncodeToString(raw)
	key := handleKey(handle)
	// The place is taken before the identity provider is asked, so
	// concurrent starts cannot pass the bound together.
	l := &login{provider: p, source: s.clientKey(r)}
	s.mu.Lock()
	s.sweepLogins(now)
	full := s.loginsFull(len(s.logins), l.source)
	if full == "" {
		s.logins[key] = l
	}
	s.mu.Unlock()
	if full != "" {
		s.refuse(w, r, FormatDevice, diag.Usage, http.StatusTooManyRequests, "%s", full)
		return
	}
	a, ref := p.StartDevice(r.Context())
	if ref != nil {
		s.mu.Lock()
		delete(s.logins, key)
		s.mu.Unlock()
		why := ref.Message()
		s.log.Warn("device sign-in not started", "request", req.id, "issuer", p.Name, "stage", ref.Name(), "reason", why)
		s.refuse(w, r, FormatDevice, diag.StateIO, http.StatusBadGateway, "the identity provider of issuer %s did not start a sign-in: %s", p.Name, why)
		return
	}
	req.entry.Decision, req.entry.Reason = control.Allowed, "device sign-in started at issuer "+p.Name
	if !s.commitStart(r, req) {
		s.mu.Lock()
		delete(s.logins, key)
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	l.interval, l.expires, l.cancel = a.Interval, now.Add(a.ExpiresIn), cancel
	s.mu.Unlock()
	s.wg.Go(func() { s.waitLogin(ctx, l, a) })
	s.log.Info("device sign-in started", "request", req.id, "issuer", p.Name, "remote", req.entry.Remote, "expires", stamp(l.expires))
	s.reply(w, http.StatusOK, Device{
		Format: FormatDevice, OK: true, Issuer: p.Name, UserCode: a.UserCode, VerificationURI: a.VerificationURI,
		VerificationURIComplete: a.VerificationURIComplete, Handle: handle, IntervalSeconds: int(a.Interval / time.Second),
		ExpiresAt: stamp(l.expires), Diagnostics: []Diagnostic{},
	})
}

// commitStart writes the audit row of a sign-in's start before its
// answer, so a start whose row fails gives back its place, which no
// client could use, instead of holding it until it runs out. It answers
// 503 and returns false when the row cannot be written.
func (s *Server) commitStart(r *http.Request, req *request) bool {
	e := req.entry
	e.Status = http.StatusOK
	req.audited = true
	if err := s.writeRow(r, req, e); err != nil {
		s.unrecorded(r, req, err)
		return false
	}
	return true
}

// waitLogin polls the identity provider for l until the person signs in,
// refuses, or the code runs out, or ctx ends, and records what it found.
func (s *Server) waitLogin(ctx context.Context, l *login, a *oidc.DeviceAuth) {
	tok, ref := l.provider.WaitDevice(ctx, a)
	l.cancel()
	s.mu.Lock()
	l.done, l.tok, l.ref = true, tok, ref
	s.mu.Unlock()
}

// sweepLogins drops the sign-ins of both flows past their life, and stops
// their pollers. A device sign-in still starting stays: its start is
// bounded by oidc.FetchTimeout and ends by filling it in or deleting it.
// The caller holds s.mu.
func (s *Server) sweepLogins(now time.Time) {
	for k, l := range s.logins {
		if l.cancel != nil && !now.Before(l.expires) {
			l.cancel()
			delete(s.logins, k)
		}
	}
	for k, l := range s.browserLogins {
		if !now.Before(l.expires) {
			delete(s.browserLogins, k)
		}
	}
}

// loginsFull says why a new sign-in cannot wait, or is empty when it can:
// waiting sign-ins of its flow already fill maxLogins, or sign-ins of both
// flows from source fill maxLoginsPerSource. The caller holds s.mu and has
// swept.
func (s *Server) loginsFull(waiting int, source string) string {
	if waiting >= maxLogins {
		return fmt.Sprintf("%d sign-ins already wait for their people; try again in a few minutes", maxLogins)
	}
	n := 0
	for _, l := range s.logins {
		if l.source == source {
			n++
		}
	}
	for _, l := range s.browserLogins {
		if l.source == source {
			n++
		}
	}
	if n >= maxLoginsPerSource {
		return fmt.Sprintf("%d sign-ins from %s already wait for their people; finish one, or try again when one runs out", maxLoginsPerSource, source)
	}
	return ""
}

// pollDevice is POST /v1/auth/device/token. It reads what the sign-in's
// poller found; it never asks the identity provider itself.
func (s *Server) pollDevice(w http.ResponseWriter, r *http.Request) {
	if !s.allowAnonymous(w, r, FormatSession) {
		return
	}
	var body DeviceTokenRequest
	if err := s.readBody(r, &body); err != nil {
		s.refuse(w, r, FormatSession, diag.Usage, http.StatusBadRequest, "%v", err)
		return
	}
	req := reqOf(r)
	key := handleKey(body.Handle)
	now := s.now()
	s.mu.Lock()
	l := s.logins[key]
	switch {
	case l == nil || body.Handle == "" || l.cancel == nil:
		s.mu.Unlock()
		s.authFail(w, r, FormatSession, "handle", "the sign-in is unknown, finished, or ran out: run writ login again")
		return
	case !now.Before(l.expires):
		l.cancel()
		delete(s.logins, key)
		s.mu.Unlock()
		s.authFail(w, r, FormatSession, "handle", "the sign-in code ran out before the person signed in: run writ login again")
		return
	case !l.done:
		interval := l.interval
		s.mu.Unlock()
		req.entry.Issuer = l.provider.Name
		s.reply(w, http.StatusOK, SessionDoc{Format: FormatSession, OK: true, Status: SessionPending, IntervalSeconds: int(interval / time.Second), Diagnostics: []Diagnostic{}})
		return
	}
	// A finished sign-in ends here: the handle works once.
	delete(s.logins, key)
	s.mu.Unlock()
	req.entry.Issuer = l.provider.Name
	if l.ref != nil {
		if l.ref.Reason() == oidc.ReasonCanceled {
			s.authFail(w, r, FormatSession, l.ref.Name(), "the sign-in failed: writ serve stopped before the person signed in")
			return
		}
		s.oidcRefused(w, r, FormatSession, "the sign-in failed: ", l.ref)
		return
	}
	s.issueCLISession(w, r, l.tok, now)
}

// issueCLISession starts the session of writ login for the person tok
// names, and answers writ.session/v0 with its token, whichever flow
// signed the person in.
func (s *Server) issueCLISession(w http.ResponseWriter, r *http.Request, tok *oidc.Token, now time.Time) {
	req := reqOf(r)
	e := req.entry
	e.Decision, e.Status = control.Allowed, http.StatusOK
	token, sess, err := s.startSession(r.Context(), tok, control.CLI, now, &e)
	if err != nil {
		s.refuse(w, r, FormatSession, diag.StateIO, http.StatusServiceUnavailable, "%v", err)
		return
	}
	req.audited = true
	info := SessionInfo{ID: sess.ID, Carrier: string(control.CLI), Person: sess.Person, Display: sess.Display, Issuer: sess.Issuer, Subject: sess.Subject,
		Teams: sess.Teams, CreatedAt: stamp(now), EndsAt: stamp(sess.EndsAt)}
	s.reply(w, http.StatusOK, SessionDoc{Format: FormatSession, OK: true, Status: SessionSignedIn, Token: token, Session: &info, Diagnostics: []Diagnostic{}})
}

// endOwnSession is DELETE /v1/auth/session: writ logout ends the session
// it sends.
func (s *Server) endOwnSession(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	p := req.principal
	if p.session == "" || req.sessionHash == nil {
		s.refuse(w, r, FormatSession, diag.Usage, http.StatusBadRequest, "DELETE /v1/auth/session ends the session the request sends, and %s sent no session", p.info.Display)
		return
	}
	now := s.now()
	e := req.entry
	e.At, e.Decision, e.Status, e.Reason, e.Elapsed = req.start, control.Allowed, http.StatusOK, "session "+p.session+" ended by its person", now.Sub(req.start)
	err := s.store.EndSession(r.Context(), req.sessionHash, p.info.Person, now, e)
	if errors.Is(err, control.ErrNotFound) {
		s.authFail(w, r, FormatSession, "session", "the session already ended")
		return
	}
	if err != nil {
		s.log.Error("end a session", "request", req.id, "err", err)
		s.refuse(w, r, FormatSession, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", req.id)
		return
	}
	req.audited = true
	s.log.Info("signed out", "request", req.id, "session", p.session, "person", p.info.Person)
	info := SessionInfo{ID: p.session, Carrier: string(control.CLI), Person: p.info.Person, Display: p.info.Display, Issuer: p.info.Issuer, Subject: p.info.Subject,
		Teams: orEmpty(p.teams), EndsAt: stamp(now)}
	s.reply(w, http.StatusOK, SessionDoc{Format: FormatSession, OK: true, Status: SessionEnded, Session: &info, Diagnostics: []Diagnostic{}})
}

// decideAdmin checks that the principal holds admin. admin is not bound
// to a stack@env: any grant that gives it to one of the principal's teams
// counts.
func (s *Server) decideAdmin(w http.ResponseWriter, r *http.Request, format string) bool {
	req := reqOf(r)
	p := req.principal
	req.entry.Op = "admin"
	grants := s.adminGrants(p)
	if len(grants) == 0 {
		s.refuse(w, r, format, diag.Denied, http.StatusForbidden, "%s may not list or end sessions: no grant gives %s admin", p.info.Display, teamList(p.teams))
		return false
	}
	req.entry.Decision, req.entry.Grants = control.Allowed, oneBased(grants)
	return true
}

// adminGrants are the positions of the grants that give a principal
// admin.
func (s *Server) adminGrants(p *principal) []int {
	var grants []int
	for i, g := range s.cfg.Policy.Grants {
		if slices.ContainsFunc(g.Teams, func(t string) bool { return slices.Contains(p.teams, t) }) && slices.Contains(g.Can, authz.Admin) {
			grants = append(grants, i)
		}
	}
	return grants
}

func teamList(teams []string) string {
	if len(teams) == 0 {
		return "a principal in no team"
	}
	return "the teams " + strings.Join(teams, ", ")
}

// listSessions is GET /v1/sessions: the live sessions, for offboarding.
// ?person= narrows them to one person's.
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	if !s.decideAdmin(w, r, FormatSessions) {
		return
	}
	person := r.URL.Query().Get("person")
	if len(person) > 256 {
		s.refuse(w, r, FormatSessions, diag.Usage, http.StatusBadRequest, "person is over 256 bytes")
		return
	}
	rows, err := s.store.Sessions(r.Context(), person, s.now())
	if err != nil {
		s.log.Error("list sessions", "request", reqOf(r).id, "err", err)
		s.refuse(w, r, FormatSessions, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", reqOf(r).id)
		return
	}
	doc := Sessions{Format: FormatSessions, OK: true, Sessions: []SessionInfo{}, Diagnostics: []Diagnostic{}}
	for _, row := range rows {
		doc.Sessions = append(doc.Sessions, sessionInfo(row))
	}
	s.reply(w, http.StatusOK, doc)
}

// endSession is DELETE /v1/sessions/{id}: an admin ends a person's
// session at once.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if !s.decideAdmin(w, r, FormatSessions) {
		return
	}
	req := reqOf(r)
	id := r.PathValue("id")
	now := s.now()
	e := req.entry
	e.At, e.Status, e.Reason, e.Elapsed = req.start, http.StatusOK, "ended session "+trunc(id), now.Sub(req.start)
	sess, err := s.store.EndSessionByID(r.Context(), id, req.principal.info.Person, now, e)
	if errors.Is(err, control.ErrNotFound) {
		s.refuse(w, r, FormatSessions, diag.Usage, http.StatusNotFound, "no live session %s", trunc(id))
		return
	}
	if err != nil {
		s.log.Error("end a session", "request", req.id, "err", err)
		s.refuse(w, r, FormatSessions, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", req.id)
		return
	}
	req.audited = true
	s.log.Info("session ended by an admin", "request", req.id, "session", sess.ID, "person", sess.Person, "by", req.principal.info.Person)
	s.reply(w, http.StatusOK, Sessions{Format: FormatSessions, OK: true, Sessions: []SessionInfo{sessionInfo(sess)}, Diagnostics: []Diagnostic{}})
}
