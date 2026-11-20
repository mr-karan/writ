package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
)

// devPersons starts the person id of the one user of writ serve --dev,
// dev/USER. No issuer's identity may be named dev, so no token of a
// configured issuer names a dev person.
const devPersons = "dev"

// DevEnv is the environment of a stack that declares none, under writ
// serve --dev.
const DevEnv = "dev"

// DevTeam is the one team of writ serve --dev. Its grant allows every
// operation and every risk class on every stack@env.
const DevTeam = "dev"

// devIssuer is the issuer of the one user of writ serve --dev, which has
// no issuer of a config.
const devIssuer = "dev"

// DevOptions describe writ serve --dev.
type DevOptions struct {
	// URL is the address clients reach it at: http:// on loopback.
	URL    string
	Listen string
	// Database is control.db's path, or a postgres:// URL.
	Database string
	// Data holds control.db, state/, keys/, and each plan's work files.
	Data string
	// Extra is the config writ serve --dev --config read: forge and repo
	// blocks, poll_every, a tofu block, and a policy block. Nil for none.
	Extra *Config
}

// DevConfig is the config of writ serve --dev: one user, every
// environment served with a key under keys/, OpenTofu from PATH with its
// directories under tofu/, and no issuer. Its text is what control.db records for it.
func DevConfig(o DevOptions) *Config {
	cfg := &Config{
		Dev: true, URL: o.URL, Listen: o.Listen, Database: o.Database, Data: o.Data,
		PlanTTL: DefaultPlanTTL, SessionTTL: DefaultSessionTTL,
		Limits: Limits{RequestsPerSecond: 100, RequestsPerAddress: 100, PlansPerPrincipal: DefaultPlans, Plans: DefaultPlans, Applies: DefaultApplies, Body: DefaultBody,
			PlanTimeout: DefaultPlanTimeout, ApplyStartTimeout: DefaultApplyStartTimeout, ApplyTimeout: DefaultApplyTimeout},
		Envs:               map[string]Env{},
		ProviderRegistries: defaultProviderRegistries,
		Policy: authz.Policy{
			// The team matches no claim: only the dev token joins it.
			Teams:  []authz.Team{{Name: DevTeam, Issuer: devIssuer}},
			Grants: []authz.Grant{{Teams: []string{DevTeam}, Stacks: []string{"**"}, Envs: []string{"*"}, Can: authz.Ops, Risks: authz.Risks}},
		},
	}
	cfg.Text = fmt.Sprintf("dev  = true\nurl  = %q\ndata = %q\n", o.URL, o.Data)
	cfg.PollEvery, cfg.UI = DefaultPollEvery, true
	cfg.Tofu = &Tofu{Binary: "tofu", PluginCache: filepath.Join(o.Data, "tofu", "plugins"), ScratchDir: filepath.Join(o.Data, "tofu", "runs")}
	if x := o.Extra; x != nil {
		cfg.Forges, cfg.Repos, cfg.PinRepos, cfg.PollEvery = x.Forges, x.Repos, x.PinRepos, x.PollEvery
		if x.Tofu != nil {
			cfg.Tofu = x.Tofu
		}
		cfg.Policies = x.Policies
		cfg.Text += x.Text
	}
	return cfg
}

// DevKeyPath is the state key of env under writ serve --dev, which state
// makes the first time a plan opens that environment.
func DevKeyPath(data, env string) string { return filepath.Join(data, "keys", env+".key") }

// envOf returns the environment a request names, and whether this server
// serves it. Under --dev every valid name is served, and a request with
// no name is DevEnv.
func (s *Server) envOf(name string) (Env, bool) {
	if !s.cfg.Dev {
		e, ok := s.cfg.Envs[name]
		return e, ok
	}
	if name == "" {
		name = DevEnv
	}
	if !nameRE.MatchString(name) {
		return Env{}, false
	}
	return Env{Name: name, Key: DevKeyPath(s.cfg.Data, name)}, true
}

// devPrincipal is the one user of writ serve --dev, when raw is its
// token, and nil otherwise. The token does not expire: it lives as long as
// the process that wrote it.
func (s *Server) devPrincipal(raw string) *principal {
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(raw)), []byte(s.dev.token)) != 1 {
		return nil
	}
	user := s.dev.user
	return &principal{
		info:  Principal{Kind: "dev", Issuer: devIssuer, Subject: user, Person: devPersons + "/" + user, Display: user + " (dev)"},
		teams: []string{DevTeam}, matched: map[string][]string{},
	}
}

// devIdentity is the token and user of writ serve --dev.
type devIdentity struct {
	token, user string
}

// devCodeTTL is how long a dev sign-in link works. It works once.
const devCodeTTL = 10 * time.Minute

// FormatUILink is POST /v1/dev/ui-link.
const FormatUILink = "writ.ui-link/v0"

// UILink is a sign-in link to the control UI of writ serve --dev.
type UILink struct {
	Format      string       `json:"format"`
	OK          bool         `json:"ok"`
	URL         string       `json:"url"`
	ExpiresAt   string       `json:"expires_at"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// DevLink makes a sign-in link to the control UI of writ serve --dev: a
// code that works once, for 10 minutes, and gives the browser a session
// of the dev user.
func (s *Server) DevLink() UILink {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	raw := base32.StdEncoding.EncodeToString(b)
	code := raw[:4] + "-" + raw[4:8]
	expires := s.now().Add(devCodeTTL)
	s.mu.Lock()
	for c, at := range s.devCodes {
		if !s.now().Before(at) {
			delete(s.devCodes, c)
		}
	}
	s.devCodes[code] = expires
	s.mu.Unlock()
	return UILink{Format: FormatUILink, OK: true, URL: s.cfg.URL + "/ui/login/dev?code=" + code, ExpiresAt: stamp(expires), Diagnostics: []Diagnostic{}}
}

// uiLink is POST /v1/dev/ui-link, which writ ui calls. Only the dev
// control plane has it.
func (s *Server) uiLink(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Dev {
		s.refuse(w, r, FormatUILink, diag.Usage, http.StatusNotFound, "only writ serve --dev makes sign-in links; sign in to this control plane at %s/ui/", s.cfg.URL)
		return
	}
	reqOf(r).entry.Decision = control.Allowed
	s.reply(w, http.StatusOK, s.DevLink())
}

// DevSignIn spends a dev sign-in code and starts a browser session of the
// dev user. It returns the session's token, which only the cookie holds.
// An allowed sign-in commits the request's audit row with the session.
func (s *Server) DevSignIn(ctx context.Context, r *http.Request, code string) (string, *Session, error) {
	now := s.now()
	req := reqOf(r)
	e := &req.entry
	e.Kind, e.Route, e.Issuer, e.PrincipalKind, e.Via, e.Decision = "sign-in", "/ui/login/dev", devIssuer, "dev", viaBrowser, control.AuthFailed
	s.mu.Lock()
	var expires time.Time
	ok := false
	for c, at := range s.devCodes {
		if subtle.ConstantTimeCompare([]byte(c), []byte(code)) == 1 {
			expires, ok = at, true
			delete(s.devCodes, c)
		}
	}
	s.mu.Unlock()
	if !s.cfg.Dev || !ok || !now.Before(expires) {
		e.Reason = "no such dev sign-in code, or it was spent or expired"
		return "", nil, errors.New("this sign-in link was used already, or it is older than 10 minutes; run writ ui for a new one")
	}
	user := s.dev.user
	sess := &Session{ID: newID(), Display: user + " (dev)", Person: devPersons + "/" + user, Issuer: devIssuer, Subject: user, Teams: []string{DevTeam}, EndsAt: now.Add(s.cfg.SessionTTL)}
	e.Subject, e.Person, e.Principal, e.Teams = user, sess.Person, sess.Display, sess.Teams
	row := *e
	row.At, row.Decision, row.Status, row.Reason = now, control.Allowed, http.StatusSeeOther, "session "+sess.ID
	token, err := s.createSession(ctx, sess, control.Browser, now, row)
	if err != nil {
		s.log.Error("store a session", "request", e.Request, "err", err)
		e.Decision, e.Reason = control.Errored, "store session "+sess.ID+": "+err.Error()
		return "", nil, errors.New("the control database failed; the request id is " + e.Request)
	}
	req.audited = true
	s.log.Info("signed in", "request", e.Request, "session", sess.ID, "person", sess.Person, "carrier", control.Browser)
	return token, sess, nil
}
