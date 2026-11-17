package awsrole

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Defaults of BrokerOptions.
const (
	// DefaultRefreshBefore is how long before credentials expire the
	// broker assumes the role again. The AWS SDKs fetch again within a few
	// minutes of expiry.
	DefaultRefreshBefore = 15 * time.Minute
	assumeTimeout        = 30 * time.Second
	// minLeft is the least life credentials may have when the broker
	// serves them after a failed refresh.
	minLeft = time.Minute
)

// BrokerOptions configures NewBroker.
type BrokerOptions struct {
	Assumer       Assumer
	RefreshBefore time.Duration
	Now           func() time.Time
	Log           *slog.Logger
	// Assumed is told of each session the broker assumes, before its
	// credentials are served. When it fails at a run's start the run does
	// not start; when it fails at a refresh the credentials are served
	// anyway.
	Assumed func(ctx context.Context, s AssumedSession) error
}

// Broker serves each run's credentials on a loopback HTTP endpoint, as the
// AWS SDKs' container credentials provider reads them: a GET with the
// run's token in the Authorization header.
type Broker struct {
	opts BrokerOptions
	srv  *http.Server
	url  string

	mu   sync.Mutex
	runs map[[32]byte]*Run
}

// NewBroker listens on a free loopback port and serves until Close.
func NewBroker(o BrokerOptions) (*Broker, error) {
	if o.Assumer == nil {
		return nil, errors.New("awsrole: a broker needs an Assumer")
	}
	if o.RefreshBefore <= 0 {
		o.RefreshBefore = DefaultRefreshBefore
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the credential endpoint: %w", err)
	}
	b := &Broker{opts: o, runs: map[[32]byte]*Run{}, url: "http://" + ln.Addr().String() + "/v1/credentials"}
	b.srv = &http.Server{
		Handler:           http.HandlerFunc(b.serve),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      assumeTimeout + 5*time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() { _ = b.srv.Serve(ln) }()
	return b, nil
}

// URL is the endpoint providers fetch credentials from.
func (b *Broker) URL() string { return b.url }

// Close stops the endpoint.
func (b *Broker) Close() error { return b.srv.Close() }

// Run is one run's credentials and its token.
type Run struct {
	b     *Broker
	key   [32]byte
	token string
	sess  Session
	creds *Keeper
}

// Start assumes s's role at once, so a run whose role STS refuses fails
// before any provider starts, and registers a fresh token for the run.
func (b *Broker) Start(ctx context.Context, s Session) (*Run, error) {
	creds, err := b.assume(ctx, s, "start")
	if err != nil {
		return nil, err
	}
	if err := b.assumed(ctx, s, creds); err != nil {
		return nil, err
	}
	refresh := func(ctx context.Context) (Credentials, error) {
		c, err := b.assume(ctx, s, "refresh")
		if err == nil {
			if err := b.assumed(ctx, s, c); err != nil {
				b.opts.Log.Error("the run's changes after its old credentials expire may alert as made outside writ", "session", s.Name, "err", err)
			}
		}
		return c, err
	}
	token := rand.Text() + rand.Text()
	r := &Run{b: b, key: sha256.Sum256([]byte(token)), token: token, sess: s,
		creds: &Keeper{assume: refresh, refreshBefore: b.opts.RefreshBefore, now: b.opts.Now, creds: creds}}
	b.mu.Lock()
	b.runs[r.key] = r
	b.mu.Unlock()
	return r, nil
}

func (b *Broker) assume(ctx context.Context, s Session, why string) (Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, assumeTimeout)
	defer cancel()
	c, err := b.opts.Assumer.Assume(ctx, s)
	if err != nil {
		b.opts.Log.Error("assume role", "role", s.Role, "session", s.Name, "source_identity", s.SourceIdentity, "why", why, "err", err)
		return Credentials{}, err
	}
	b.opts.Log.Info("assumed role", "role", s.Role, "session", s.Name, "source_identity", s.SourceIdentity, "why", why,
		"access_key_id", c.AccessKeyID, "expires", c.Expires.UTC().Format(time.RFC3339))
	return c, nil
}

// assumed tells Assumed of a session's new credentials.
func (b *Broker) assumed(ctx context.Context, s Session, c Credentials) error {
	if b.opts.Assumed == nil {
		return nil
	}
	rec := AssumedSession{Role: s.Role, Name: s.Name, SourceIdentity: s.SourceIdentity, From: b.opts.Now(), Until: c.Expires}
	if err := b.opts.Assumed(ctx, rec); err != nil {
		return fmt.Errorf("record role session %s of %s: %w", s.Name, s.Role, err)
	}
	return nil
}

// Env is what a provider needs to fetch the run's credentials. The AWS
// SDKs read these two variables.
func (r *Run) Env() []string {
	return []string{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI=" + r.b.url,
		"AWS_CONTAINER_AUTHORIZATION_TOKEN=" + r.token,
	}
}

// End stops answering the run's token.
func (r *Run) End() {
	r.b.mu.Lock()
	delete(r.b.runs, r.key)
	r.b.mu.Unlock()
}

// endpointDoc is the container credentials document the AWS SDKs read.
type endpointDoc struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
	AccountID       string `json:"AccountId,omitempty"`
}

// endpointError is the error document the AWS SDKs read.
type endpointError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (b *Broker) serve(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, doc any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(doc)
	}
	if r.Method != http.MethodGet {
		reply(http.StatusMethodNotAllowed, endpointError{"MethodNotAllowed", "the credential endpoint takes GET"})
		return
	}
	b.mu.Lock()
	run := b.runs[sha256.Sum256([]byte(r.Header.Get("Authorization")))]
	b.mu.Unlock()
	if run == nil {
		b.opts.Log.Warn("credential request refused", "remote", r.RemoteAddr)
		reply(http.StatusUnauthorized, endpointError{"AccessDenied", "no running writ run has this token"})
		return
	}
	c, err := run.creds.Get(r.Context())
	if err != nil {
		reply(http.StatusServiceUnavailable, endpointError{"AssumeRoleFailed", err.Error()})
		return
	}
	b.opts.Log.Debug("served credentials", "session", run.sess.Name, "access_key_id", c.AccessKeyID)
	reply(http.StatusOK, endpointDoc{
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, Token: c.SessionToken,
		Expiration: c.Expires.UTC().Format(time.RFC3339), AccountID: AccountOf(run.sess.Role),
	})
}
