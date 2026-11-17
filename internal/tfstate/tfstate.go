// Package tfstate serves OpenTofu's http state backend to the runs of writ
// serve. Each run gets a token that the control database keeps only as a
// hash: a plan's token reads the state of its one stack@env, and an apply's
// token also locks and writes it. Every state is sealed under its
// environment's key before it is stored, and the server reads nothing of a
// state but the serial and lineage that OpenTofu keeps outside its own
// encryption.
package tfstate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/control"
)

const (
	// DefaultKeep is how many versions of each stack's state are kept.
	DefaultKeep = 20
	// DefaultMaxState bounds the size of one state.
	DefaultMaxState = 64 << 20
	// maxLock bounds the JSON of a lock.
	maxLock = 64 << 10
	// username is the basic-auth user of every run. The password, the
	// run's token, is what proves the run.
	username = "writ"
)

// Options configures New.
type Options struct {
	Store *control.Store
	// Key returns the key that seals the states of an environment.
	Key func(env string) (*Key, error)
	// Keep is how many versions of each stack's state to keep. Zero means
	// DefaultKeep.
	Keep int
	// MaxState bounds one state in bytes. Zero means DefaultMaxState.
	MaxState int64
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log takes the errors the server answers 500 for. Nil discards them.
	Log *slog.Logger
}

// Backend is the http state backend of the routes under /v1/tfstate/.
type Backend struct {
	opts Options
	mux  *http.ServeMux
}

// New returns a Backend.
func New(opts Options) (*Backend, error) {
	if opts.Store == nil || opts.Key == nil {
		return nil, errors.New("tfstate: Options.Store and Options.Key are required")
	}
	if opts.Keep < 0 || opts.MaxState < 0 {
		return nil, errors.New("tfstate: Options.Keep and Options.MaxState must not be negative")
	}
	if opts.Keep == 0 {
		opts.Keep = DefaultKeep
	}
	if opts.MaxState == 0 {
		opts.MaxState = DefaultMaxState
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	b := &Backend{opts: opts, mux: http.NewServeMux()}
	// A stack's name may hold a slash, which its address escapes, so the
	// name is always one path segment.
	b.mux.HandleFunc("GET /v1/tfstate/{stack}", b.get)
	b.mux.HandleFunc("POST /v1/tfstate/{stack}", b.post)
	b.mux.HandleFunc("LOCK /v1/tfstate/{stack}/lock", b.lock)
	b.mux.HandleFunc("UNLOCK /v1/tfstate/{stack}/lock", b.unlock)
	return b, nil
}

// Serve answers one request. e is the request's audit row as the server
// began it, and Serve returns it with what the request did: the run, its
// stack@env, the operation, and the decision, never any state. audited is
// true when the row is already in the control database: committed with
// the state write, lock, or unlock it records, or by commit. A read that
// serves a state calls commit with its row, with the status it will
// answer, before it reads the state, and sends nothing when commit fails,
// since the server has then answered in its place.
func (b *Backend) Serve(w http.ResponseWriter, r *http.Request, e control.Entry, commit func(control.Entry) error) (row control.Entry, audited bool) {
	c := &call{e: e, commit: commit}
	b.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callKey{}, c)))
	return c.e, c.audited
}

// call is what one request's handler learns, for its audit row.
type call struct {
	e       control.Entry
	audited bool
	commit  func(control.Entry) error
}

type callKey struct{}

func callOf(r *http.Request) *call { return r.Context().Value(callKey{}).(*call) }

// begin names the route and the operation of a request on its row.
func begin(r *http.Request, route, op string) *control.Entry {
	e := &callOf(r).e
	e.Route, e.Op = route, op
	return e
}

// allowed is the row of a request that commits its row before it
// answers: a change commits it with the change, and a read before it
// reads the state.
func allowed(r *http.Request, at time.Time) control.Entry {
	e := callOf(r).e
	e.At, e.Status, e.Decision = at, http.StatusOK, control.Allowed
	return e
}

// refuse answers a refused request with msg and records it for the row.
func refuse(w http.ResponseWriter, r *http.Request, msg string, status int) {
	e := &callOf(r).e
	e.Reason = msg
	switch {
	case status == http.StatusUnauthorized:
		e.Decision = control.AuthFailed
	case status == http.StatusForbidden:
		e.Decision = control.Denied
	case status >= 500:
		e.Decision = control.Errored
	default:
		e.Decision = control.Refused
	}
	http.Error(w, msg, status)
}

const (
	stateRoute = "/v1/tfstate/{stack}"
	lockRoute  = "/v1/tfstate/{stack}/lock"
)

// Issue makes a token for one run on one stack@env, stores its hash, and
// returns the token. The token works until t.ExpiresAt or until End.
func (b *Backend) Issue(ctx context.Context, t control.StateToken) (string, error) {
	if _, err := b.opts.Key(t.Env); err != nil {
		return "", fmt.Errorf("the state key of env %s: %w", t.Env, err)
	}
	if t.RunID == "" || t.Stack == "" {
		return "", errors.New("a state token needs a run and a stack")
	}
	now := b.opts.Now()
	if !t.ExpiresAt.After(now) {
		return "", errors.New("a state token must expire after now")
	}
	token := rand.Text()
	if err := b.opts.Store.AddStateToken(ctx, hashToken(token), t, now); err != nil {
		return "", fmt.Errorf("store the state token of run %s: %w", t.RunID, err)
	}
	return token, nil
}

// End ends a run's access to state: its tokens stop working and the locks
// it holds are released.
func (b *Backend) End(ctx context.Context, runID string) error {
	if err := b.opts.Store.EndStateRun(ctx, runID); err != nil {
		return fmt.Errorf("end the state access of run %s: %w", runID, err)
	}
	return nil
}

// ClientEnv returns the environment that points tofu's http backend, an
// empty backend "http" {} block, at the state of stack served under base,
// with token as its credential.
func ClientEnv(base, stack, token string) []string {
	addr := strings.TrimSuffix(base, "/") + "/v1/tfstate/" + url.PathEscape(stack)
	return []string{
		"TF_HTTP_ADDRESS=" + addr,
		"TF_HTTP_LOCK_ADDRESS=" + addr + "/lock",
		"TF_HTTP_UNLOCK_ADDRESS=" + addr + "/lock",
		"TF_HTTP_USERNAME=" + username,
		"TF_HTTP_PASSWORD=" + token,
	}
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// authorize returns the token of a request that may act on the stack in
// its path. A request with no live token is 401; a live token for another
// stack, or a plan's token on a route that writes, is 403.
func (b *Backend) authorize(w http.ResponseWriter, r *http.Request, write bool) (control.StateToken, control.StateAuth, bool) {
	_, password, ok := r.BasicAuth()
	if !ok || password == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="writ state"`)
		refuse(w, r, "this route needs a run's state token", http.StatusUnauthorized)
		return control.StateToken{}, control.StateAuth{}, false
	}
	auth := control.StateAuth{Hash: hashToken(password), Now: b.opts.Now()}
	t, err := b.opts.Store.StateTokenByHash(r.Context(), auth.Hash, auth.Now)
	if err == nil {
		e := &callOf(r).e
		e.Env, e.Stacks, e.Run = t.Env, []string{t.Stack}, t.RunID
	}
	if errors.Is(err, control.ErrNotFound) {
		w.Header().Set("WWW-Authenticate", `Basic realm="writ state"`)
		refuse(w, r, "the state token is unknown, expired, or its run ended", http.StatusUnauthorized)
		return control.StateToken{}, control.StateAuth{}, false
	}
	if err != nil {
		b.fail(w, r, err)
		return control.StateToken{}, control.StateAuth{}, false
	}
	if t.Stack != r.PathValue("stack") {
		refuse(w, r, "the state token is for another stack", http.StatusForbidden)
		return control.StateToken{}, control.StateAuth{}, false
	}
	if write && t.Scope != control.StateApply {
		refuse(w, r, "a plan's state token only reads state", http.StatusForbidden)
		return control.StateToken{}, control.StateAuth{}, false
	}
	auth.RunID = t.RunID
	return t, auth, true
}

func (b *Backend) fail(w http.ResponseWriter, r *http.Request, err error) {
	b.opts.Log.Error("state backend", "method", r.Method, "path", r.URL.Path, "err", err)
	refuse(w, r, "the state backend failed; see the server's log", http.StatusInternalServerError)
}

// get serves the newest state. An apply reads it too, so both scopes may.
// Its row commits before the state is read from the control database, so
// no state is held while the row waits, and the state then goes straight
// to the client.
func (b *Backend) get(w http.ResponseWriter, r *http.Request) {
	e := begin(r, stateRoute, "state-read")
	t, _, ok := b.authorize(w, r, false)
	if !ok {
		return
	}
	version, err := b.opts.Store.LatestVersion(r.Context(), t.Env, t.Stack)
	if errors.Is(err, control.ErrNotFound) {
		e.Decision, e.Reason = control.Allowed, "no state yet"
		http.Error(w, "no state yet", http.StatusNotFound)
		return
	}
	if err != nil {
		b.fail(w, r, err)
		return
	}
	key, err := b.opts.Key(t.Env)
	if err != nil {
		b.fail(w, r, fmt.Errorf("the state key of env %s: %w", t.Env, err))
		return
	}
	row := allowed(r, b.opts.Now())
	row.Reason = fmt.Sprintf("state version %d", version)
	c := callOf(r)
	if err := c.commit(row); err != nil {
		return
	}
	c.audited = true
	sealed, err := b.opts.Store.SealedState(r.Context(), t.Env, t.Stack, version)
	var plain []byte
	if err == nil {
		plain, err = key.OpenState(sealed, t.Env, t.Stack, version)
	}
	if err != nil {
		b.fail(w, r, fmt.Errorf("its audit row records state version %d as served: %w", version, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(plain)
}

// envelope is all the server reads of a state. OpenTofu keeps these two
// keys at the top level of a plain state and of an encrypted one alike.
type envelope struct {
	Serial  *int64 `json:"serial"`
	Lineage string `json:"lineage"`
}

func (b *Backend) post(w http.ResponseWriter, r *http.Request) {
	begin(r, stateRoute, "state-write").Kind = "state-write"
	t, auth, ok := b.authorize(w, r, true)
	if !ok {
		return
	}
	lockID := r.URL.Query().Get("ID")
	if lockID == "" {
		refuse(w, r, "a state write needs the id of the lock it holds", http.StatusConflict)
		return
	}
	body, ok := readBody(w, r, b.opts.MaxState)
	if !ok {
		return
	}
	var head envelope
	if err := json.Unmarshal(body, &head); err != nil || head.Serial == nil || *head.Serial < 0 || head.Lineage == "" {
		refuse(w, r, "the body is not an OpenTofu state with a serial and a lineage", http.StatusBadRequest)
		return
	}
	key, err := b.opts.Key(t.Env)
	if err != nil {
		b.fail(w, r, fmt.Errorf("the state key of env %s: %w", t.Env, err))
		return
	}
	sum := sha256.Sum256(body)
	auth.Now = b.opts.Now()
	row := allowed(r, auth.Now)
	err = b.opts.Store.PutState(r.Context(), control.StatePut{
		Env: t.Env, Stack: t.Stack, Auth: auth, LockID: lockID, Keep: b.opts.Keep,
		Version: control.StateVersion{
			Serial: *head.Serial, Lineage: head.Lineage, SHA256: hex.EncodeToString(sum[:]),
			RunID: t.RunID, At: auth.Now,
		},
		Seal:  func(version int64) []byte { return key.seal(body, t.Env, t.Stack, version) },
		Audit: row,
	})
	switch {
	case errors.Is(err, control.ErrStateRunEnded):
		runEnded(w, r)
	case errors.Is(err, control.ErrStateNotLocked), errors.Is(err, control.ErrStateStale):
		refuse(w, r, err.Error(), http.StatusConflict)
	case err != nil:
		b.fail(w, r, err)
	default:
		callOf(r).audited = true
	}
}

// lockBody is the part of OpenTofu's lock JSON the server reads.
type lockBody struct {
	ID string `json:"ID"`
}

func (b *Backend) lock(w http.ResponseWriter, r *http.Request) {
	begin(r, lockRoute, "state-lock")
	t, auth, ok := b.authorize(w, r, true)
	if !ok {
		return
	}
	body, ok := readBody(w, r, maxLock)
	if !ok {
		return
	}
	var l lockBody
	if err := json.Unmarshal(body, &l); err != nil || l.ID == "" {
		refuse(w, r, "the body is not an OpenTofu lock with an ID", http.StatusBadRequest)
		return
	}
	auth.Now = b.opts.Now()
	held, err := b.opts.Store.LockState(r.Context(), auth, t.Env, t.Stack, control.StateLock{ID: l.ID, RunID: t.RunID, Info: string(body), At: auth.Now}, allowed(r, auth.Now))
	b.lockAnswer(w, r, held, err)
}

func (b *Backend) unlock(w http.ResponseWriter, r *http.Request) {
	begin(r, lockRoute, "state-unlock")
	t, auth, ok := b.authorize(w, r, true)
	if !ok {
		return
	}
	body, ok := readBody(w, r, maxLock)
	if !ok {
		return
	}
	var l lockBody
	if len(body) > 0 {
		if err := json.Unmarshal(body, &l); err != nil {
			refuse(w, r, "the body is not an OpenTofu lock", http.StatusBadRequest)
			return
		}
	}
	auth.Now = b.opts.Now()
	held, err := b.opts.Store.UnlockState(r.Context(), auth, t.Env, t.Stack, l.ID, allowed(r, auth.Now))
	b.lockAnswer(w, r, held, err)
}

// lockAnswer answers a refused LOCK or UNLOCK with 423 and the held lock's
// JSON, which tofu prints as the lock's holder.
func (b *Backend) lockAnswer(w http.ResponseWriter, r *http.Request, held control.StateLock, err error) {
	e := &callOf(r).e
	switch {
	case errors.Is(err, control.ErrStateRunEnded):
		runEnded(w, r)
	case errors.Is(err, control.ErrStateLocked):
		e.Decision, e.Reason = control.Refused, "the state is locked by run "+held.RunID
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		io.WriteString(w, held.Info)
	case err != nil:
		b.fail(w, r, err)
	default:
		callOf(r).audited = true
	}
}

// runEnded answers a request whose run ended, or whose token expired,
// while the request was read.
func runEnded(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Basic realm="writ state"`)
	refuse(w, r, "the state token is unknown, expired, or its run ended", http.StatusUnauthorized)
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		refuse(w, r, fmt.Sprintf("the body is larger than %d bytes", limit), http.StatusRequestEntityTooLarge)
		return nil, false
	case err != nil:
		refuse(w, r, "cannot read the body", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}
