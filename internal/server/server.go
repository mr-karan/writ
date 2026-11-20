// Package server is writ serve: the control plane's REST API over HTTPS.
// Every request but GET /v1/server and GET /healthz carries an OIDC token,
// checked before the body is read. Teams and grants decide each request,
// and every other request under the limit of its client's address, to the
// API, the control UI, or the state backend, answers only once its audit
// row is in the control database. Remote
// plans run with state opened read-only, stay on the server, and are named
// by the saved plan's signature.
//
// Each plan and apply runs OpenTofu through a runner.Tofu.
package server

import (
	"compress/gzip"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/notify"
	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/policy"
	"github.com/mr-karan/writ/internal/runner"
	"github.com/mr-karan/writ/internal/tfstate"
)

// Verifier checks tokens; *oidc.Verifier is one.
type Verifier interface {
	Verify(ctx context.Context, raw string) (*oidc.Token, *oidc.Refusal)
	Health() []oidc.Health
}

// Options configures New.
type Options struct {
	Config   *Config
	Store    *control.Store
	Verifier Verifier
	// People are the issuers people sign in to the control UI with.
	People []*oidc.Provider
	// Tofu runs OpenTofu for the stacks of .tf files, in writ serve's
	// own process. Nil refuses them.
	Tofu runner.Tofu
	// Assumer assumes the environments' roles. A config with a role needs
	// one.
	Assumer awsrole.Assumer
	// Reap kills what a provider left running as a uid before the uid
	// serves another plan. Nil means runas.Reap.
	Reap func(uid uint32) error
	// WrapExecutor, when set, wraps the executor of the OpenTofu runs, so
	// a test sees each job that crosses to it.
	WrapExecutor func(runner.Executor) runner.Executor
	// DevToken is the one token a Config.Dev server accepts, and DevUser
	// the name of the user it stands for.
	DevToken, DevUser string
	Version           string
	Log               *slog.Logger
	Now               func() time.Time
	// WithTimeout creates a deadline context. Nil uses context.WithTimeoutCause.
	WithTimeout func(context.Context, time.Duration, error) (context.Context, context.CancelFunc)
	// NotifyQueueSize, NotifyTimeout, and NotifyBackoff override the
	// notification dispatcher's defaults when set.
	NotifyQueueSize int
	NotifyTimeout   time.Duration
	NotifyBackoff   []time.Duration
}

// Server serves the API.
type Server struct {
	cfg         *Config
	store       *control.Store
	verifier    Verifier
	people      []*oidc.Provider
	version     string
	log         *slog.Logger
	now         func() time.Time
	withTimeout func(context.Context, time.Duration, error) (context.Context, context.CancelFunc)
	configID    int64
	// dev is the one identity of a Config.Dev server.
	dev devIdentity

	// limiter counts each principal's requests, and clients each client
	// address's; shed counts what clients refused.
	limiter *limiter
	clients *limiter
	shed    shed
	// api routes every request, and audited is api behind the audit
	// row. Call sends the control UI's reads through audited too.
	api     *apiMux
	audited http.Handler
	// mounts is where each handler mounted beside the API's routes
	// serves, with the answer it fails with.
	mounts []mounted
	// exec runs each OpenTofu run as a job. It is nil when writ serve runs
	// no OpenTofu.
	exec runner.Executor
	// broker serves each plan's role credentials to its providers. It is
	// nil when no environment names a role.
	broker *awsrole.Broker
	// stateOnly is the state backend alone behind the audit row, which
	// the state port of a run's own network serves, and runClients counts
	// each run's requests to it. Both are nil under egress_mode off.
	stateOnly  http.Handler
	runClients *limiter
	// forges are the config's forges with their clients, by name.
	forges map[string]forgeClient
	// hooks is what the webhook route keeps between deliveries.
	hooks *hookState
	// tfstate is the http state backend of the OpenTofu stacks, which
	// keyMu guards the keys of.
	tfstate *tfstate.Backend
	// stateKeys are the keys of each environment's states and saved
	// plans, under keyMu.
	stateKeys map[string]*tfstate.Key
	keyMu     sync.Mutex

	// plans run under ctx; Close cancels it and waits on wg.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// running counts each principal's running plans; total counts all.
	running map[string]int
	total   int
	// done closes when the plan with that ref ends, and runDone when the
	// run with that id does.
	done    map[string]chan struct{}
	runDone map[string]chan struct{}
	// applies counts the running applies, and the starts that wait to be
	// settled. pending holds the runs whose start is in progress.
	applies int
	pending map[string]struct{}
	// boot is this process's id, which each run it begins records as its
	// owner.
	boot string
	// closeKey settles a key, and applyCommit, when set, wraps the commit
	// of a run's start. settleEvery and settleFor pace the settle after
	// a failed commit. Tests change them.
	closeKey    func(ctx context.Context, key string, issued, now time.Time) (control.KeyRow, error)
	applyCommit func(commit func() error) error
	settleEvery time.Duration
	settleFor   time.Duration
	// devCodes are the dev sign-in codes not spent yet, with when each
	// expires.
	devCodes map[string]time.Time
	// logins are the sign-ins of writ login that wait for their people,
	// by the SHA-256 of their handles.
	logins map[[32]byte]*login
	// browserLogins are the sign-ins of writ login in a browser that wait
	// for their callbacks, by the SHA-256 of their handles.
	browserLogins map[[32]byte]*browserLogin
	// policy judges the plans of OpenTofu stacks, nil for no policy.
	policy *policy.Engine
	// notifier delivers asynchronous notifications to configured targets.
	notifier *notify.Dispatcher
	// cursorKey seals the cursors of GET /v1/audit.
	cursorKey cipher.AEAD
}

// New prepares a server: it marks plans a crash left planning and runs it
// left applying as failed, ends the state tokens and locks of runs no
// process runs, and records the config, with its teams and grants, in the
// control database.
func New(ctx context.Context, opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	cfg := opts.Config
	if cfg.Dev && (len(opts.DevToken) < 32 || opts.DevUser == "") {
		return nil, errors.New("writ serve --dev needs a token of at least 32 characters and a user")
	}
	s := &Server{
		dev: devIdentity{token: opts.DevToken, user: opts.DevUser},
		cfg: cfg, store: opts.Store, verifier: opts.Verifier, people: opts.People,
		version: opts.Version, log: opts.Log, now: opts.Now, withTimeout: opts.WithTimeout,
		limiter: newLimiter(cfg.Limits.RequestsPerSecond, maxBuckets, opts.Now),
		clients: newLimiter(cfg.Limits.RequestsPerAddress, maxBuckets, opts.Now),
		running: map[string]int{}, done: map[string]chan struct{}{}, runDone: map[string]chan struct{}{}, devCodes: map[string]time.Time{}, logins: map[[32]byte]*login{}, browserLogins: map[[32]byte]*browserLogin{},
		pending: map[string]struct{}{}, boot: newID(), settleEvery: settleEvery, settleFor: settleFor,
		hooks: newHookState(opts.Now),
	}
	s.closeKey = s.store.CloseKey
	var err error
	if s.cursorKey, err = newCursorKey(); err != nil {
		return nil, err
	}
	if s.policy, err = CompilePolicies(cfg); err != nil {
		return nil, err
	}
	if err := s.initForges(); err != nil {
		return nil, err
	}
	if err := s.initState(); err != nil {
		return nil, err
	}
	if err := s.initProviders(opts); err != nil {
		return nil, err
	}
	if n, err := s.store.FailPlanning(ctx, s.now()); err != nil {
		s.closeProviders()
		return nil, err
	} else if n > 0 {
		s.log.Warn("plans left planning by an earlier process are failed", "plans", n)
	}
	if n, err := s.store.FailDriftChecks(ctx, s.now()); err != nil {
		s.closeProviders()
		return nil, err
	} else if n > 0 {
		s.log.Warn("drift checks left planning by an earlier process are failed", "checks", n)
	}
	if runs, err := s.store.FailApplying(ctx, s.now(), s.boot, s.handled); err != nil {
		s.closeProviders()
		return nil, err
	} else if len(runs) > 0 {
		s.log.Warn("runs left applying by an earlier process are failed; the next plan reads what they left in doubt", "runs", runs)
	}
	if unlocked, err := s.store.EndOrphanState(ctx, s.now()); err != nil {
		s.closeProviders()
		return nil, fmt.Errorf("end the state access of runs an earlier process left: %w", err)
	} else if len(unlocked) > 0 {
		s.log.Warn("state locks of runs an earlier process left are released", "stacks", unlocked)
	}
	host, _ := osHostname()
	id, err := s.store.RecordConfig(ctx, cfg.Text, cfg.URL, host, &cfg.Policy, s.now())
	if err != nil {
		s.closeProviders()
		return nil, fmt.Errorf("record the config: %w", err)
	}
	s.configID = id
	if err := s.store.Audit(ctx, control.Entry{At: s.now(), Request: newID(), Kind: "started", Decision: control.NoDecision, ConfigID: id, Reason: "writ " + s.version}); err != nil {
		s.closeProviders()
		return nil, err
	}
	if err := s.initNotifications(opts); err != nil {
		s.closeProviders()
		return nil, err
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.api = s.routes()
	s.audited = s.audit(s.api)
	s.mount("/v1/tfstate/", http.HandlerFunc(s.serveState), func(w http.ResponseWriter, r *http.Request, status int, d Diagnostic) {
		http.Error(w, d.Message, status)
	}, false)
	s.initEgress()
	s.wg.Go(func() { s.poll(s.ctx) })
	s.wg.Go(func() { s.sweepRuns(s.ctx) })
	if s.exec != nil {
		s.wg.Go(func() { s.refreshIndexes(s.ctx) })
	}
	if s.exec != nil && s.cfg.DriftEvery > 0 {
		s.wg.Go(func() { s.checkDrift(s.ctx) })
	}
	return s, nil
}

// Close stops every running plan and waits for each to record its end.
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
	s.notifier.Close()
	s.closeProviders()
}

// request is what one request's handler learns, for its audit row.
type request struct {
	id        string
	start     time.Time
	route     string
	principal *principal
	entry     control.Entry
	// audited is set when the handler wrote the audit row in its own
	// transaction.
	audited bool
	// fail answers a failure of a request to a mounted handler in its
	// format. It is nil for the API's routes.
	fail FailFunc
	// rec holds the answer until the audit row commits, and out is where
	// it goes then.
	rec *recorder
	out http.ResponseWriter
	// outcome is what a 503 of an apply request's route says of a run:
	// OutcomeNone or OutcomeUnknown. Other routes leave it "".
	outcome string
	// keyClosed is set on a refusal of POST /v1/runs whose key is closed
	// with no run, so a client sends a new key.
	keyClosed bool
	// sessionHash is the SHA-256 of the session token the request sent as
	// its bearer token, so DELETE /v1/auth/session can end it. The token
	// itself is kept nowhere.
	sessionHash []byte
	// envs, when it holds more than one environment, splits the request's
	// audit row into one row for each, as rows says: an approval of plans
	// in several environments.
	envs []envPart
}

// envPart is what one environment's audit row of a request names.
type envPart struct {
	env    string
	stacks []string
	grants []int
}

// part is the envPart of env in req.envs, added when there is none yet.
func (req *request) part(env string) *envPart {
	for i := range req.envs {
		if req.envs[i].env == env {
			return &req.envs[i]
		}
	}
	req.envs = append(req.envs, envPart{env: env})
	return &req.envs[len(req.envs)-1]
}

// rows is e as the request's audit rows: e alone, or with req.envs of
// more than one environment, one row for each, which names only that
// environment's stacks and grants. A row is shown to the readers of its
// one environment, so a row that named the stacks of two would show one
// environment's stacks to the readers of the other.
func (req *request) rows(e control.Entry) []control.Entry {
	if len(req.envs) < 2 {
		return []control.Entry{e}
	}
	out := make([]control.Entry, 0, len(req.envs))
	for _, p := range req.envs {
		row := e
		row.Env, row.Stacks = p.env, p.stacks
		if e.Grants != nil {
			row.Grants = p.grants
		}
		out = append(out, row)
	}
	return out
}

type ctxKey struct{}

func reqOf(r *http.Request) *request { return r.Context().Value(ctxKey{}).(*request) }

// Handler is the API, with the control UI if MountUI added it, behind the
// audit row and the limit on each client address.
func (s *Server) Handler() http.Handler {
	h := s.limitClients(s.audited)
	if s.version == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeader, s.version)
		h.ServeHTTP(w, r)
	})
}

// VersionHeader carries writ serve's version on every answer, so a CLI
// older than the server can say so before an answer it cannot read.
const VersionHeader = "X-Writ-Version"

// UIRoute is one route of the control UI: a ServeMux pattern and its
// handler, a func, so a router cannot be a route.
type UIRoute struct {
	Pattern string
	Handler http.HandlerFunc
}

// mount serves pattern with h beside the API's routes. h does its own
// authentication, and its answer waits for the request's audit row as the
// API's does. fail answers in h's place when the request fails apart from
// h: 503 when its audit row cannot be written, or 500 when h panics. fail
// also answers for the redirects the mux sends to reach pattern, such as
// /ui to /ui/. fromUI is set only by MountUI.
func (s *Server) mount(pattern string, h http.Handler, fail FailFunc, fromUI bool) {
	s.api.handle(pattern, h, fromUI)
	s.mounts = append(s.mounts, newMount(pattern, fail))
}

// FailFunc answers a request with status and one diagnostic, in the
// format of the handler it replaces.
type FailFunc func(w http.ResponseWriter, r *http.Request, status int, d Diagnostic)

// mounted is where a handler that mount added serves.
type mounted struct {
	method   string
	segments []string
	subtree  bool
	fail     FailFunc
}

func newMount(pattern string, fail FailFunc) mounted {
	m := mounted{fail: fail}
	p := pattern
	if method, rest, ok := strings.Cut(pattern, " "); ok {
		m.method, p = method, rest
	}
	m.subtree = !strings.HasSuffix(p, "/{$}") && strings.HasSuffix(p, "/")
	m.segments = segments(strings.TrimSuffix(p, "{$}"))
	return m
}

func segments(p string) []string {
	return strings.FieldsFunc(p, func(c rune) bool { return c == '/' })
}

// failOf is the failure answer of the mount that serves r, or of the
// mount that the mux redirects r to, or nil for the API's routes. It
// reads the path as the mux does: cleaned, then each segment unescaped,
// so an escaped slash stays inside its segment.
func (s *Server) failOf(r *http.Request) FailFunc {
	p := r.URL.EscapedPath()
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	segs := segments(path.Clean(p))
	for i, seg := range segs {
		if u, err := url.PathUnescape(seg); err == nil {
			segs[i] = u
		}
	}
	for _, m := range s.mounts {
		if m.method != "" && m.method != r.Method && !(m.method == http.MethodGet && r.Method == http.MethodHead) {
			continue
		}
		if (m.subtree && len(segs) >= len(m.segments) && slices.Equal(segs[:len(m.segments)], m.segments)) || slices.Equal(segs, m.segments) {
			return m.fail
		}
	}
	return nil
}

// MountUI serves the control UI's table of routes beside the API. Each
// handler does its own authentication: the API's middleware does not wrap
// it. fail answers in a route's place as mount says. MountUI is the only
// way a route under /ui/ reaches the server, and it takes the table once,
// so the table is every route of the control UI and the UI's leak checks,
// which walk it, miss none. A second call panics. It is called before the
// server serves.
func (s *Server) MountUI(routes []UIRoute, fail FailFunc) {
	if s.api.ui {
		panic("writ serve mounts the control UI once")
	}
	s.api.ui = true
	for _, rt := range routes {
		s.mount(rt.Pattern, rt.Handler, fail, true)
	}
}

// apiMux routes every request. A route enters it only through handle,
// which keeps the paths under /ui/ for MountUI.
type apiMux struct {
	mux *http.ServeMux
	// ui is set once MountUI ran.
	ui bool
}

func (m *apiMux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

// Handler is the handler and pattern r would reach.
func (m *apiMux) Handler(r *http.Request) (http.Handler, string) { return m.mux.Handler(r) }

// handle serves pattern with h. Outside MountUI's table it panics on a
// pattern that could match a path under /ui/: one under /ui, one whose
// first segment is a wildcard, and one with a host, which Go's ServeMux
// prefers to every pattern without one.
func (m *apiMux) handle(pattern string, h http.Handler, fromUI bool) {
	// A pattern is [METHOD ][HOST]/[PATH], and the ServeMux unescapes
	// each literal segment of PATH.
	rest := pattern
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		rest = strings.TrimLeft(rest[i:], " \t")
	}
	host, p, _ := strings.Cut(rest, "/")
	first, _, _ := strings.Cut(p, "/")
	if u, err := url.PathUnescape(first); err == nil {
		first = u
	}
	if !fromUI && (host != "" || first == "ui" || strings.HasPrefix(first, "{")) {
		panic("writ serve registers " + pattern + " outside the control UI's table, and it could serve a path under /ui/; add a route of the control UI to the table in internal/ui instead")
	}
	m.mux.Handle(pattern, h)
}

func (s *Server) routes() *apiMux {
	mux := &apiMux{mux: http.NewServeMux()}
	for _, rt := range s.routeTable() {
		mux.handle(rt.pattern, s.wrap(rt.pattern, rt.auth, rt.methods), false)
	}
	return mux
}

// serveState is the state backend, which checks each request's run token
// itself. Its writes, locks, and unlocks commit their own audit rows, and
// a read commits its row through commitAhead before it sends the state.
func (s *Server) serveState(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	row, audited := s.tfstate.Serve(w, r, req.entry, func(e control.Entry) error { return s.commitAhead(r, req, e) })
	req.entry, req.audited = row, req.audited || audited
}

// commitAhead writes e, with the status its handler will answer, as the
// request's audit row before the handler answers, and then lets the
// handler send its answer straight to the client as it writes it, so a
// large answer is never held in memory for the row. When the row cannot
// be written, the request is answered 503 and the handler writes nothing
// more.
func (s *Server) commitAhead(r *http.Request, req *request, e control.Entry) error {
	req.audited = true
	if err := s.writeRow(r, req, e); err != nil {
		s.unrecorded(r, req, err)
		return err
	}
	req.rec.stream(req.out)
	return nil
}

// apiRoute is one route of the API and the handler of each method it
// takes.
type apiRoute struct {
	pattern string
	auth    bool
	methods map[string]http.HandlerFunc
}

func (s *Server) routeTable() []apiRoute {
	return []apiRoute{
		{"/healthz", false, map[string]http.HandlerFunc{"GET": s.health}},
		{"/v1/server", false, map[string]http.HandlerFunc{"GET": s.serverInfo}},
		{"/v1/auth/device", false, map[string]http.HandlerFunc{"POST": s.startDevice}},
		{"/v1/auth/device/token", false, map[string]http.HandlerFunc{"POST": s.pollDevice}},
		{"/v1/auth/browser", false, map[string]http.HandlerFunc{"POST": s.startBrowser}},
		{"/v1/auth/browser/token", false, map[string]http.HandlerFunc{"POST": s.finishBrowser}},
		{"/v1/auth/session", true, map[string]http.HandlerFunc{"DELETE": s.endOwnSession}},
		{"/v1/sessions", true, map[string]http.HandlerFunc{"GET": s.listSessions}},
		{"/v1/sessions/{id}", true, map[string]http.HandlerFunc{"DELETE": s.endSession}},
		{"/v1/whoami", true, map[string]http.HandlerFunc{"GET": s.whoami}},
		{"/v1/plans", true, map[string]http.HandlerFunc{"GET": s.listPlans, "POST": s.createPlan}},
		{"/v1/plans/{id}", true, map[string]http.HandlerFunc{"GET": s.getPlan}},
		{"/v1/plans/{id}/approvals", true, map[string]http.HandlerFunc{"POST": s.approvePlan}},
		{"/v1/plans/{id}/approvals/mine", true, map[string]http.HandlerFunc{"DELETE": s.withdraw}},
		{"/v1/approvals", true, map[string]http.HandlerFunc{"POST": s.approvePipeline}},
		{"/v1/runs/{id}", true, map[string]http.HandlerFunc{"GET": s.getRun}},
		{"/v1/applies", true, map[string]http.HandlerFunc{"GET": s.listRuns}},
		{"/v1/config", true, map[string]http.HandlerFunc{"GET": s.configDoc}},
		{"/v1/policy", true, map[string]http.HandlerFunc{"GET": s.policyDoc}},
		{"/v1/state/adopt", true, map[string]http.HandlerFunc{"POST": s.adoptState}},
		{"/v1/runs", true, map[string]http.HandlerFunc{"POST": s.createRun}},
		{outcomeRoute, true, map[string]http.HandlerFunc{"POST": s.applyOutcome}},
		{"/v1/repos", true, map[string]http.HandlerFunc{"GET": s.listRepos, "POST": s.createRepo}},
		{"/v1/repos/{name}", true, map[string]http.HandlerFunc{"GET": s.getRepo}},
		{"/v1/hooks/{forge}/{repo}", false, map[string]http.HandlerFunc{"POST": s.receiveHook}},
		{"/v1/dev/ui-link", true, map[string]http.HandlerFunc{"POST": s.uiLink}},
		{"/v1/drift", true, map[string]http.HandlerFunc{"GET": s.listDrift}},
		{"/v1/drift/{id}", true, map[string]http.HandlerFunc{"GET": s.getDrift}},
		{"/v1/index", true, map[string]http.HandlerFunc{"GET": s.stateIndex}},
		{"/v1/overview", true, map[string]http.HandlerFunc{"GET": s.overview}},
		{"/v1/audit", true, map[string]http.HandlerFunc{"GET": s.listAudit}},
		{"/", false, map[string]http.HandlerFunc{}},
	}
}

// auditTimeout bounds the write of a request's audit row. The answer
// waits for it, so it also bounds how long a busy control database holds
// each answer.
const auditTimeout = 5 * time.Second

// unaudited reports whether a request writes no audit row: the
// unauthenticated reads of health and discovery, so they answer at once
// on a busy disk. Only their exact paths are exempt, as the client sent
// them, so an escaped or uncleaned path that the mux routes to the same
// handler is audited.
func unaudited(r *http.Request) bool {
	p := r.URL.EscapedPath()
	return r.Method == http.MethodGet && (p == "/healthz" || p == "/v1/server")
}

// audit is the outermost handler under the address limit, so it sees
// every request: the API's routes, the mounted control UI, the state
// backend, and what the mux answers itself, such as the redirect of an
// uncleaned path. It gives each request an id and holds the answer until
// the request's audit row commits, so no client learns an answer that the
// audit log does not record. It picks the request's failure answer from
// its path before the mux runs, so a redirect to a mount fails in the
// mount's format.
func (s *Server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := &request{id: newID(), start: s.now()}
		req.entry = control.Entry{Request: req.id, Kind: "request", Method: auditMethod(r.Method), Remote: s.remoteOf(r), ConfigID: s.configID, Decision: control.NoDecision}
		req.fail, req.rec, req.out = s.failOf(r), &recorder{header: http.Header{}}, w
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, req))
		w.Header().Set("X-Request-Id", req.id)
		defer s.finish(w, req.rec, r, req)
		next.ServeHTTP(req.rec, r)
	})
}

// auditMethod is the method an audit row records of a request. A method
// of no standard set is OTHER: the client chose its text, which can be
// anything an HTTP token allows, and GET /v1/audit shows the row. LOCK
// and UNLOCK are the state backend's.
func auditMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, "LOCK", "UNLOCK":
		return m
	}
	return "OTHER"
}

// wrap is every API route's middleware: the method check, the token check
// before any body is read, and the rate limit.
func (s *Server) wrap(pattern string, auth bool, methods map[string]http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := reqOf(r)
		req.route, req.entry.Route = pattern, pattern
		switch {
		case pattern == "/v1/runs" && r.Method == http.MethodPost:
			req.outcome = OutcomeNone
		case pattern == outcomeRoute:
			req.outcome = OutcomeUnknown
		}
		h, ok := methods[r.Method]
		if !ok {
			if len(methods) == 0 {
				s.refuse(w, r, FormatError, diag.Usage, http.StatusNotFound, "no route %s %s", r.Method, r.URL.Path)
				return
			}
			w.Header().Set("Allow", strings.Join(sortedKeys(methods), ", "))
			s.refuse(w, r, FormatError, diag.Usage, http.StatusMethodNotAllowed, "%s takes %s, not %s", pattern, strings.Join(sortedKeys(methods), " or "), r.Method)
			return
		}
		if auth && !s.sessionAuth(r, req) && !s.authenticate(w, r, req) {
			return
		}
		h(w, r)
	})
}

// finish writes the request's audit row and then sends the answer held in
// rec, or a 503 in its place when the row cannot be written. A panic
// answers 500, audited or not. An answer that already streams after its
// committed row is the handler's to end, and a panic cuts it short.
func (s *Server) finish(w http.ResponseWriter, rec *recorder, r *http.Request, req *request) {
	p := recover()
	if p != nil {
		s.log.Error("handler panicked", "request", req.id, "panic", p)
	}
	if rec.out != nil {
		if p != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if p != nil {
		s.fail(rec, r, req, http.StatusInternalServerError, diag.Internal, "writ serve hit a bug; the request id is "+req.id)
		req.entry.Decision, req.entry.Reason = control.Errored, fmt.Sprint(p)
	}
	if !req.audited && !unaudited(r) {
		e := req.entry
		e.Status = rec.status
		if err := s.writeRow(r, req, e); err != nil {
			s.unrecorded(r, req, err)
		}
	}
	maps.Copy(w.Header(), rec.header)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body.Bytes())
}

// writeRow writes e as the audit row of req, or the rows req.rows splits
// it into, timed from its start.
func (s *Server) writeRow(r *http.Request, req *request, e control.Entry) error {
	if e.Route == "" {
		e.Route = routeOf(r.Pattern)
	}
	e.At, e.Elapsed = req.start, s.now().Sub(req.start)
	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()
	return s.store.Audit(ctx, req.rows(e)...)
}

// unrecorded replaces the answer of a request whose audit row was not
// written with a 503.
func (s *Server) unrecorded(r *http.Request, req *request, err error) {
	s.log.Error("audit row not written; the request is refused", "request", req.id, "err", err)
	s.fail(req.rec, r, req, http.StatusServiceUnavailable, diag.StateIO,
		"the control database did not record this request, so writ serve refused it; try again later; the request id is "+req.id)
}

// fail replaces the answer held in rec with a failure in the format of
// the request's route.
func (s *Server) fail(rec *recorder, r *http.Request, req *request, status int, class diag.Class, msg string) {
	*rec = recorder{header: http.Header{}}
	if req.fail != nil {
		req.fail(rec, r, status, Diagnostic{"error", string(class), msg})
		return
	}
	s.refuse(rec, r, formatOf(req.route, r.Method), class, status, "%s", msg)
}

// routeOf is the path of a mux pattern, without its method.
func routeOf(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}

func sortedKeys(m map[string]http.HandlerFunc) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// authenticate checks the bearer token and the rate limit. It answers the
// request and returns false when either refuses.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, req *request) bool {
	format := formatOf(req.route, r.Method)
	h := r.Header.Get("Authorization")
	raw, ok := strings.CutPrefix(h, "Bearer ")
	switch {
	case h == "":
		w.Header().Set("WWW-Authenticate", `Bearer realm="writ"`)
		s.authFail(w, r, format, "", "no token: sign in with writ login, or send Authorization: Bearer with the OIDC token of a CI job")
		return false
	case !ok:
		w.Header().Set("WWW-Authenticate", `Bearer realm="writ"`)
		s.authFail(w, r, format, "", "the Authorization header is not Bearer TOKEN")
		return false
	case strings.HasPrefix(raw, SessionPrefix):
		return s.sessionBearer(w, r, req, format, strings.TrimSpace(raw))
	case s.cfg.Dev:
		p := s.devPrincipal(raw)
		if p == nil {
			s.authFail(w, r, format, "dev-token", "the token is not this dev control plane's: writ reads it from client.json in the data directory of writ serve --dev")
			return false
		}
		req.principal = p
		e := &req.entry
		e.Issuer, e.Subject, e.Person, e.Principal, e.Teams = p.info.Issuer, p.info.Subject, p.info.Person, p.info.Display, p.teams
		e.PrincipalKind, e.Via = p.info.Kind, p.via()
		return true
	}
	tok, ref := s.verifier.Verify(r.Context(), strings.TrimSpace(raw))
	if ref != nil {
		s.oidcRefused(w, r, format, "the token failed a check: ", ref)
		return false
	}
	p := newPrincipal(tok, s.cfg.identity(tok.Issuer), &s.cfg.Policy)
	req.principal = p
	e := &req.entry
	e.Issuer, e.Subject, e.Person, e.Principal, e.Teams = tok.Issuer, tok.Subject, p.info.Person, p.info.Display, p.teams
	e.PrincipalKind, e.Via = p.info.Kind, p.via()
	s.log.Info("token", "request", req.id, "issuer", tok.Issuer, "sub", tok.Subject, "jti", tok.ID, "teams", p.teams)
	if !s.limiter.allow(p.key()) {
		s.refuse(w, r, format, diag.Usage, http.StatusTooManyRequests, "more than %d requests a second from %s", s.cfg.Limits.RequestsPerSecond, p.info.Display)
		return false
	}
	return true
}

// sessionBearer takes the principal from a session of writ login sent as
// the bearer token. A control UI cookie's session is refused here.
func (s *Server) sessionBearer(w http.ResponseWriter, r *http.Request, req *request, format, raw string) bool {
	sess, err := s.session(r.Context(), raw, control.CLI)
	if errors.Is(err, ErrNoSession) {
		s.authFail(w, r, format, "session", "the session ended, ran out, or is not one writ login gave: sign in again with writ login %s", s.cfg.URL)
		return false
	}
	if err != nil {
		s.log.Error("read a session", "request", req.id, "err", err)
		s.refuse(w, r, format, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", req.id)
		return false
	}
	p := sessionPrincipal(sess, viaCLI)
	req.principal, req.sessionHash = p, hashToken(raw)
	e := &req.entry
	e.Issuer, e.Subject, e.Person, e.Principal, e.Teams = sess.Issuer, sess.Subject, sess.Person, sess.Display, sess.Teams
	e.PrincipalKind, e.Via = p.info.Kind, p.via()
	if !s.limiter.allow(p.key()) {
		s.refuse(w, r, format, diag.Usage, http.StatusTooManyRequests, "more than %d requests a second from %s", s.cfg.Limits.RequestsPerSecond, p.info.Display)
		return false
	}
	return true
}

// oidcRefused answers 401 for a refusal of internal/oidc, and logs and
// audits it. A refusal reaches an answer, a log line, or an audit row only
// through here and SignIn, and only as its Message, which holds no text
// from the identity provider or the token. prefix is Writ's own words.
func (s *Server) oidcRefused(w http.ResponseWriter, r *http.Request, format, prefix string, ref *oidc.Refusal) {
	req := reqOf(r)
	req.entry.Decision, req.entry.Check = control.AuthFailed, ref.Name()
	s.log.Warn("auth", "request", req.id, "issuer", req.entry.Issuer, "check", ref.Name(), "remote", req.entry.Remote, "reason", ref.Message())
	s.refuse(w, r, format, diag.Auth, http.StatusUnauthorized, "%s", prefix+ref.Message())
}

func (s *Server) authFail(w http.ResponseWriter, r *http.Request, format, check, msg string, args ...any) {
	req := reqOf(r)
	req.entry.Decision, req.entry.Check = control.AuthFailed, check
	s.log.Warn("auth", "request", req.id, "check", check, "remote", req.entry.Remote)
	s.refuse(w, r, format, diag.Auth, http.StatusUnauthorized, msg, args...)
}

// formatOf is the document format a route answers with, so a refusal is
// the route's own document with ok false.
func formatOf(route, method string) string {
	switch {
	case route == "/v1/whoami":
		return FormatWhoami
	case route == "/v1/auth/device":
		return FormatDevice
	case route == "/v1/auth/browser":
		return FormatBrowser
	case strings.HasPrefix(route, "/v1/auth/"):
		return FormatSession
	case strings.HasPrefix(route, "/v1/sessions"):
		return FormatSessions
	case route == "/v1/plans" && method == http.MethodGet:
		return FormatRemotePlans
	case strings.HasPrefix(route, "/v1/plans/{id}/approvals") || route == "/v1/approvals":
		return FormatApprove
	case route == "/v1/runs/{id}" || route == "/v1/runs" && method == http.MethodPost:
		return FormatRemoteRun
	case route == "/v1/applies":
		return FormatRemoteRuns
	case route == outcomeRoute:
		return FormatOutcome
	case route == "/v1/config":
		return FormatConfig
	case route == "/v1/drift":
		return FormatDrift
	case route == "/v1/drift/{id}":
		return FormatDriftCheck
	case route == "/v1/index":
		return FormatStateIndex
	case route == "/v1/overview":
		return FormatOverview
	case route == "/v1/audit":
		return FormatAudit
	case strings.HasPrefix(route, "/v1/plans"):
		return FormatRemotePlan
	case route == "/v1/state/adopt":
		return FormatStateAdopt
	case route == "/v1/dev/ui-link":
		return FormatUILink
	case route == "/v1/repos" && method == http.MethodGet:
		return FormatRepos
	case strings.HasPrefix(route, "/v1/repos"):
		return FormatRepo
	case strings.HasPrefix(route, "/v1/hooks/"):
		return FormatHook
	}
	return FormatError
}

// refusal is the shape of every refused request: the route's format, ok
// false, and one diagnostic. Readers of the route's document read the same
// fields they always do.
type refusal struct {
	Format string `json:"format"`
	OK     bool   `json:"ok"`
	// Outcome is set on a 503 of an apply request's route.
	Outcome string `json:"outcome,omitempty"`
	// KeyClosed is true on a refusal of POST /v1/runs whose key is closed
	// with no run: no request with the key starts a run.
	KeyClosed   bool         `json:"key_closed,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// refuse answers with a refusal and records it for the audit row.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, format string, class diag.Class, status int, msg string, args ...any) {
	text := fmt.Sprintf(msg, args...)
	req := reqOf(r)
	if req.entry.Decision == control.NoDecision || req.entry.Decision == control.Allowed {
		switch class {
		case diag.Denied:
			req.entry.Decision = control.Denied
		case diag.Internal, diag.StateIO:
			req.entry.Decision = control.Errored
		default:
			req.entry.Decision = control.Refused
		}
	}
	req.entry.Reason = text
	if class == diag.Denied {
		s.log.Warn("denied", "request", req.id, "op", req.entry.Op, "env", req.entry.Env, "stacks", req.entry.Stacks, "person", req.entry.Person, "reason", text)
	}
	doc := refusal{Format: format, KeyClosed: req.keyClosed, Diagnostics: []Diagnostic{{"error", string(class), text}}}
	if status == http.StatusServiceUnavailable {
		doc.Outcome = req.outcome
	}
	s.reply(w, status, doc)
}

func (s *Server) reply(w http.ResponseWriter, status int, doc any) {
	b, err := json.Marshal(doc)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"format":"writ.error/v0","ok":false,"diagnostics":[{"severity":"error","class":"internal","message":"encode the response"}]}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// decide checks a grant and answers a refusal when there is none.
func (s *Server) decide(w http.ResponseWriter, r *http.Request, format string, op authz.Op, stack, env string) bool {
	req := reqOf(r)
	e := &req.entry
	e.Op, e.Env, e.Stacks = string(op), env, []string{stack}
	d := s.cfg.Policy.Decide(req.principal.teams, op, []string{stack}, env, nil)
	if !d.Allowed {
		s.refuse(w, r, format, diag.Denied, http.StatusForbidden, "%s may not %s %s@%s: %s", req.principal.info.Display, op, stack, env, d.Reason)
		return false
	}
	e.Decision, e.Grants = control.Allowed, oneBased(d.Grants)
	return true
}

func oneBased(idx []int) []int {
	out := make([]int, len(idx))
	for i, x := range idx {
		out[i] = x + 1
	}
	return out
}

func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	doc := ServerInfo{Format: FormatServer, OK: true, Version: s.version, URL: s.cfg.URL, Dev: s.cfg.Dev, Issuers: []IssuerInfo{}, Login: len(s.people) > 0, DeviceIssuers: []string{}, Diagnostics: []Diagnostic{}}
	for _, is := range s.cfg.Issuers {
		doc.Issuers = append(doc.Issuers, IssuerInfo{Name: is.Name, Kind: string(is.Kind), URL: is.URL, Identity: is.Identity})
	}
	for _, p := range s.people {
		if p.Device() {
			doc.DeviceIssuers = append(doc.DeviceIssuers, p.Name)
		}
	}
	s.reply(w, http.StatusOK, doc)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	doc := Health{Format: FormatHealth, OK: true, Database: true, Issuers: []IssuerHealth{}, Diagnostics: []Diagnostic{}}
	if err := s.store.Ping(r.Context()); err != nil {
		doc.OK, doc.Database = false, false
		doc.Diagnostics = append(doc.Diagnostics, Diagnostic{"error", string(diag.StateIO), "the control database does not answer"})
	}
	var healths []oidc.Health
	if s.verifier != nil {
		healths = s.verifier.Health()
	}
	for _, p := range s.people {
		healths = append(healths, p.Health())
	}
	for _, h := range healths {
		ih := IssuerHealth{Name: h.Issuer, KeysAt: stamp(h.GoodAt)}
		if h.HasOffset {
			ms := h.Offset.Milliseconds()
			ih.ClockOffsetMS = &ms
		}
		if s.now().Sub(h.GoodAt) > oidc.MaxKeySetAge {
			doc.OK = false
			doc.Diagnostics = append(doc.Diagnostics, Diagnostic{"error", string(diag.Auth), "issuer " + h.Issuer + " has had no good key set for 24 hours"})
		}
		doc.Issuers = append(doc.Issuers, ih)
	}
	status := http.StatusOK
	if !doc.OK {
		status = http.StatusServiceUnavailable
	}
	s.reply(w, status, doc)
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	p := reqOf(r).principal
	doc := Whoami{Format: FormatWhoami, OK: true, Principal: &p.info, Teams: p.teams, Grants: []GrantRow{}, Diagnostics: []Diagnostic{}}
	for _, row := range s.cfg.Policy.Table(p.teams) {
		can := make([]string, len(row.Can))
		for i, op := range row.Can {
			can[i] = string(op)
		}
		doc.Grants = append(doc.Grants, GrantRow{Grant: row.Grant + 1, Envs: row.Envs, Stacks: row.Stack, Can: can, Risks: orEmpty(row.Risks)})
	}
	reqOf(r).entry.Decision = control.Allowed
	s.reply(w, http.StatusOK, doc)
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// target checks env and stack name a stack@env this server serves, and
// returns the environment.
func (s *Server) target(w http.ResponseWriter, r *http.Request, format, env, stack string) (Env, bool) {
	e, ok := s.envOf(env)
	if !ok {
		if s.cfg.Dev {
			s.refuse(w, r, format, diag.Usage, http.StatusBadRequest, "env %q is not a name: it must match %s", trunc(env), nameRE)
		} else {
			s.refuse(w, r, format, diag.Usage, http.StatusBadRequest, "env %q is not one writ serve plans in; it has %s", trunc(env), strings.Join(s.envNames(), ", "))
		}
		return Env{}, false
	}
	if err := CheckStackName(stack); err != nil {
		s.refuse(w, r, format, diag.Usage, http.StatusBadRequest, "%v", err)
		return Env{}, false
	}
	return e, true
}

func (s *Server) envNames() []string {
	var out []string
	for n := range s.cfg.Envs {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// readBody reads a JSON request body into v: at most Limits.Body bytes,
// counted after gzip decompression, with no unknown field.
func (s *Server) readBody(r *http.Request, v any) error {
	limit := s.cfg.Limits.Body
	body := http.MaxBytesReader(nil, r.Body, limit)
	var rd io.Reader = body
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(body)
		if err != nil {
			return errBadBody{"the body is not gzip"}
		}
		defer gz.Close()
		rd = gz
	default:
		return errBadBody{"Content-Encoding " + trunc(enc) + " is not gzip"}
	}
	lr := &io.LimitedReader{R: rd, N: limit + 1}
	dec := json.NewDecoder(lr)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if lr.N <= 0 || errors.As(err, new(*http.MaxBytesError)) {
		return errTooLarge{limit}
	}
	if err != nil {
		return errBadBody{"the body is not the JSON document this route takes: " + err.Error()}
	}
	if dec.More() {
		return errBadBody{"the body holds more than one JSON document"}
	}
	return nil
}

// refuseBody answers a body that readBody refused: 413 when it is too
// large, 400 otherwise.
func (s *Server) refuseBody(w http.ResponseWriter, r *http.Request, format string, err error) {
	status := http.StatusBadRequest
	if _, ok := err.(errTooLarge); ok {
		status = http.StatusRequestEntityTooLarge
	}
	s.refuse(w, r, format, diag.Usage, status, "%v", err)
}

type errBadBody struct{ msg string }

func (e errBadBody) Error() string { return e.msg }

type errTooLarge struct{ limit int64 }

func (e errTooLarge) Error() string {
	return fmt.Sprintf("the body is over %d bytes after decompression", e.limit)
}

func newID() string { return strings.ToUpper(rand.Text()) }

// outcomeRoute settles an apply request's key.
const outcomeRoute = "/v1/apply-requests/{key}/outcome"
