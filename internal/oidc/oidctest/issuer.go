// Package oidctest runs an OIDC issuer for tests: an HTTPS server with a
// discovery document and a key set, keys generated in the test, and tokens
// the test signs. It stands in for GitLab and GitHub, which no test
// contacts.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Key is a signing key the issuer publishes.
type Key struct {
	ID  string
	RSA *rsa.PrivateKey
	EC  *ecdsa.PrivateKey
}

// Issuer is a running test issuer.
type Issuer struct {
	srv *httptest.Server
	mux *http.ServeMux
	// URL is the issuer: https://127.0.0.1:PORT.
	URL string

	mu           sync.Mutex
	keys         []Key
	cacheControl string
	fail         bool
	fetches      int
	// DiscoveryIssuer, when set, is the issuer the discovery document names.
	discoveryIssuer string
	jwksURI         string

	// The code flow, for people issuers.
	clientID, clientSecret string
	person                 map[string]any
	userinfo               map[string]any
	noS256                 bool
	grants                 map[string]*grant // by code
	access                 map[string]map[string]any
	idTokenEdit            func(map[string]any)
	redirects              []string

	// The device flow.
	noDevice       bool
	deviceInterval time.Duration
	deviceLife     time.Duration
	devices        map[string]*device // by device code
	deviceStarts   int
	devicePolls    int
	slowDownNext   bool
	gate           *gate

	echo   Endpoint
	echoed []string

	tamper map[Endpoint]Tamper
}

// gate holds device authorization requests until n of them wait, or for
// at most wait.
type gate struct {
	n       int
	wait    time.Duration
	mu      sync.Mutex
	arrived int
	open    chan struct{}
}

func (g *gate) pass() {
	g.mu.Lock()
	g.arrived++
	if g.arrived == g.n {
		close(g.open)
	}
	g.mu.Unlock()
	select {
	case <-g.open:
	case <-time.After(g.wait):
	}
}

// device is one device code the issuer handed out, and what became of it.
type device struct {
	clientID, userCode string
	expires            time.Time
	lastPoll           time.Time
	// state is pending, approved, or denied.
	state            string
	claims, userinfo map[string]any
}

// grant is one authorization code the issuer handed out.
type grant struct {
	clientID, redirectURI, nonce, challenge, method string
	claims, userinfo                                map[string]any
}

// New starts an issuer with an RSA key "rsa-1" and a P-256 key "ec-1". It
// stops when the test ends.
func New(t testing.TB) *Issuer {
	t.Helper()
	return NewWithKeys(t, NewRSAKey(t, "rsa-1", 2048), NewECKey(t, "ec-1"))
}

// NewWithKeys starts an issuer that publishes keys, so tests that start
// many issuers can share keys made once.
func NewWithKeys(t testing.TB, keys ...Key) *Issuer {
	t.Helper()
	is := &Issuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", is.discovery)
	mux.HandleFunc("GET /oauth/discovery/keys", is.jwks)
	mux.HandleFunc("GET /oauth/authorize", is.authorize)
	mux.HandleFunc("POST /oauth/token", is.token)
	mux.HandleFunc("GET /oauth/userinfo", is.userinfoHandler)
	mux.HandleFunc("POST /oauth/authorize_device", is.authorizeDevice)
	is.grants, is.access, is.devices, is.mux = map[string]*grant{}, map[string]map[string]any{}, map[string]*device{}, mux
	is.deviceInterval, is.deviceLife = time.Second, 5*time.Minute
	is.srv = httptest.NewTLSServer(is.tampering(is.echoing(mux)))
	t.Cleanup(is.srv.Close)
	is.URL = is.srv.URL
	is.keys = keys
	return is
}

// Mount serves pattern with h on the issuer's own host, as a GitLab
// instance serves its API beside its OIDC endpoints.
func (is *Issuer) Mount(pattern string, h http.Handler) { is.mux.Handle(pattern, h) }

// NewRSAKey makes an RSA key of bits bits.
func NewRSAKey(t testing.TB, id string, bits int) Key {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return Key{ID: id, RSA: k}
}

// NewECKey makes a P-256 key.
func NewECKey(t testing.TB, id string) Key {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Key{ID: id, EC: k}
}

// Client trusts the issuer's certificate.
func (is *Issuer) Client() *http.Client { return is.srv.Client() }

// CAPEM is the issuer's certificate, for a config's ca_file.
func (is *Issuer) CAPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: is.srv.Certificate().Raw})
}

// SetKeys replaces the published keys, as a rotation does.
func (is *Issuer) SetKeys(keys ...Key) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.keys = keys
}

// Keys returns the published keys.
func (is *Issuer) Keys() []Key {
	is.mu.Lock()
	defer is.mu.Unlock()
	return append([]Key(nil), is.keys...)
}

// SetCacheControl sets the Cache-Control header of the key set.
func (is *Issuer) SetCacheControl(v string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.cacheControl = v
}

// SetFail makes the key set answer 500 while fail is true.
func (is *Issuer) SetFail(fail bool) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.fail = fail
}

// SetDiscovery makes the discovery document name another issuer or
// jwks_uri; "" keeps the true one.
func (is *Issuer) SetDiscovery(issuer, jwksURI string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.discoveryIssuer, is.jwksURI = issuer, jwksURI
}

// Fetches counts key set requests.
func (is *Issuer) Fetches() int {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.fetches
}

func (is *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	is.mu.Lock()
	iss, jwks := is.URL, is.URL+"/oauth/discovery/keys"
	if is.discoveryIssuer != "" {
		iss = is.discoveryIssuer
	}
	if is.jwksURI != "" {
		jwks = is.jwksURI
	}
	is.mu.Unlock()
	doc := map[string]any{
		"issuer":                                iss,
		"jwks_uri":                              jwks,
		"authorization_endpoint":                is.URL + "/oauth/authorize",
		"token_endpoint":                        is.URL + "/oauth/token",
		"userinfo_endpoint":                     is.URL + "/oauth/userinfo",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported":              []string{"code", "id_token"},
		"subject_types_supported":               []string{"public", "pairwise"},
		"code_challenge_methods_supported":      []string{"plain", "S256"},
	}
	if is.noS256 {
		delete(doc, "code_challenge_methods_supported")
	}
	is.mu.Lock()
	if !is.noDevice {
		doc["device_authorization_endpoint"] = is.URL + "/oauth/authorize_device"
		doc["grant_types_supported"] = []string{"authorization_code", "urn:ietf:params:oauth:grant-type:device_code"}
	}
	is.mu.Unlock()
	writeJSON(w, doc)
}

func (is *Issuer) jwks(w http.ResponseWriter, _ *http.Request) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.fetches++
	if is.fail {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	if is.cacheControl != "" {
		w.Header().Set("Cache-Control", is.cacheControl)
	}
	var keys []map[string]string
	for _, k := range is.keys {
		keys = append(keys, JWK(k))
	}
	writeJSON(w, map[string]any{"keys": keys})
}

// JWK is k's public half as GitLab publishes it: kty, kid, use, alg, and
// the key's numbers.
func JWK(k Key) map[string]string {
	enc := base64.RawURLEncoding.EncodeToString
	if k.RSA != nil {
		return map[string]string{
			"kty": "RSA", "kid": k.ID, "use": "sig", "alg": "RS256",
			"e": enc(big.NewInt(int64(k.RSA.E)).Bytes()), "n": enc(k.RSA.N.Bytes()),
		}
	}
	pub, err := k.EC.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	return map[string]string{
		"kty": "EC", "kid": k.ID, "use": "sig", "alg": "ES256", "crv": "P-256",
		"x": enc(pub[1:33]), "y": enc(pub[33:]),
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	_ = json.NewEncoder(w).Encode(v)
}

// Sign signs claims with k, with header {"alg", "kid", "typ": "JWT"}.
func Sign(k Key, claims map[string]any) string {
	alg := "RS256"
	if k.EC != nil {
		alg = "ES256"
	}
	return SignWith(k, map[string]any{"alg": alg, "kid": k.ID, "typ": "JWT"}, claims)
}

// SignWith signs claims with k under any header, for tests of hostile
// headers. The signature algorithm follows k's type, not the header.
func SignWith(k Key, header, claims map[string]any) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	return SignBytes(k, h, c)
}

// SignBytes signs a header and a payload given as bytes.
func SignBytes(k Key, header, payload []byte) string {
	enc := base64.RawURLEncoding.EncodeToString
	signed := enc(header) + "." + enc(payload)
	sum := sha256.Sum256([]byte(signed))
	var sig []byte
	var err error
	if k.RSA != nil {
		sig, err = rsa.SignPKCS1v15(rand.Reader, k.RSA, crypto.SHA256, sum[:])
	} else {
		var r, s *big.Int
		r, s, err = ecdsa.Sign(rand.Reader, k.EC, sum[:])
		if err == nil {
			sig = make([]byte, 64)
			r.FillBytes(sig[:32])
			s.FillBytes(sig[32:])
		}
	}
	if err != nil {
		panic(err)
	}
	return signed + "." + enc(sig)
}

// GitLabJob returns the claims of a GitLab CI id_token in the shape GitLab's
// document shows (docs.gitlab.com/ci/secrets/id_token_authentication/, read
// 2026-11-10): every id is a string, and so is ref_protected. The job is a
// merge request pipeline of project 4242 started by user 1001, alice.
func GitLabJob(issuer, aud string, now time.Time) map[string]any {
	return map[string]any{
		"namespace_id":          "72",
		"namespace_path":        "acme",
		"project_id":            "4242",
		"project_path":          "acme/infra",
		"user_id":               "1001",
		"user_login":            "alice",
		"user_email":            "alice@example.com",
		"user_access_level":     "developer",
		"pipeline_id":           "1849213",
		"pipeline_source":       "merge_request_event",
		"job_id":                "9922101",
		"ref":                   "add-subnet-d",
		"ref_type":              "branch",
		"ref_path":              "refs/heads/add-subnet-d",
		"ref_protected":         "false",
		"runner_id":             1,
		"runner_environment":    "gitlab-hosted",
		"sha":                   "5d1e9a0c7b2f4e1d8a6c3b9e0f7a2d4c6b8e1f3a",
		"project_visibility":    "private",
		"ci_config_ref_uri":     "gitlab.com/acme/infra//.gitlab-ci.yml@refs/heads/add-subnet-d",
		"ci_config_sha":         "5d1e9a0c7b2f4e1d8a6c3b9e0f7a2d4c6b8e1f3a",
		"jti":                   rand.Text(),
		"iss":                   issuer,
		"iat":                   now.Unix(),
		"nbf":                   now.Add(-5 * time.Second).Unix(),
		"exp":                   now.Add(5 * time.Minute).Unix(),
		"sub":                   "project_path:acme/infra:ref_type:branch:ref:add-subnet-d",
		"aud":                   aud,
		"environment_protected": "false",
	}
}

// GitHubJob returns the claims of a GitHub Actions token in the shape
// GitHub's document shows (docs.github.com/en/actions/reference/security/oidc,
// read 2026-11-10).
func GitHubJob(issuer, aud string, now time.Time) map[string]any {
	return map[string]any{
		"jti":                   rand.Text(),
		"sub":                   "repo:acme/infra:pull_request",
		"aud":                   aud,
		"ref":                   "refs/pull/42/merge",
		"sha":                   "5d1e9a0c7b2f4e1d8a6c3b9e0f7a2d4c6b8e1f3a",
		"repository":            "acme/infra",
		"repository_owner":      "acme",
		"actor_id":              "2002",
		"repository_visibility": "private",
		"repository_id":         "781234567",
		"repository_owner_id":   "65",
		"run_id":                "6182739104",
		"run_number":            "10",
		"run_attempt":           "1",
		"runner_environment":    "github-hosted",
		"actor":                 "bob",
		"workflow":              "writ-plan",
		"head_ref":              "add-subnet-d",
		"base_ref":              "main",
		"event_name":            "pull_request",
		"ref_type":              "branch",
		"workflow_ref":          "acme/infra/.github/workflows/writ-plan.yml@refs/pull/42/merge",
		"job_workflow_ref":      "acme/infra/.github/workflows/writ-plan.yml@refs/pull/42/merge",
		"iss":                   issuer,
		"nbf":                   now.Add(-5 * time.Second).Unix(),
		"exp":                   now.Add(5 * time.Minute).Unix(),
		"iat":                   now.Unix(),
	}
}

// SetClient registers the one OAuth client the issuer knows, as an
// operator registers writ serve with GitLab.
func (is *Issuer) SetClient(id, secret string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.clientID, is.clientSecret = id, secret
}

// SetPerson sets who signs in at the authorization endpoint from now on:
// the claims of the ID token, beyond iss, aud, iat, exp, and nonce, and
// the userinfo answer. Nil claims make the endpoint refuse consent.
func (is *Issuer) SetPerson(claims, userinfo map[string]any) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.person, is.userinfo = claims, userinfo
}

// SetNoS256 leaves code_challenge_methods_supported out of discovery.
func (is *Issuer) SetNoS256(v bool) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.noS256 = v
}

// EditIDToken changes the claims of every ID token the token endpoint
// signs from now on, for tests of a bad nonce or audience.
func (is *Issuer) EditIDToken(edit func(map[string]any)) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.idTokenEdit = edit
}

// RegisterRedirects sets the redirect URIs of the client, as an operator
// registers them with the application. From then on the authorization
// endpoint refuses any other, but takes a loopback URI on any port (RFC
// 8252, section 7.3), as GitLab's Doorkeeper does. With none registered
// it takes any redirect URI.
func (is *Issuer) RegisterRedirects(uris ...string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.redirects = uris
}

// registered reports whether redirect matches a registered redirect URI.
// The caller holds is.mu.
func (is *Issuer) registered(redirect *url.URL) bool {
	if len(is.redirects) == 0 {
		return true
	}
	for _, raw := range is.redirects {
		reg, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if raw == redirect.String() || loopback(reg) && loopback(redirect) && reg.Scheme == redirect.Scheme &&
			reg.Hostname() == redirect.Hostname() && reg.Path == redirect.Path && reg.RawQuery == redirect.RawQuery && reg.User == nil && redirect.User == nil {
			return true
		}
	}
	return false
}

func loopback(u *url.URL) bool {
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && ip != nil && ip.IsLoopback()
}

// authorize stands in for the identity provider's sign-in page: it signs in
// the person SetPerson named at once and redirects back with a code. A
// loopback redirect URI is a native app's (RFC 8252), which must send an
// S256 PKCE challenge (section 8.1).
func (is *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	is.mu.Lock()
	defer is.mu.Unlock()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if q.Get("client_id") == "" || q.Get("client_id") != is.clientID || err != nil || redirect.Scheme == "" || !is.registered(redirect) {
		http.Error(w, "unknown client or redirect_uri", http.StatusBadRequest)
		return
	}
	back := redirect.Query()
	back.Set("state", q.Get("state"))
	switch {
	case loopback(redirect) && (q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == ""):
		back.Set("error", "invalid_request")
	case q.Get("response_type") != "code" || is.person == nil:
		back.Set("error", "access_denied")
	default:
		code := rand.Text()
		is.grants[code] = &grant{
			clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"), nonce: q.Get("nonce"),
			challenge: q.Get("code_challenge"), method: q.Get("code_challenge_method"),
			claims: is.person, userinfo: is.userinfo,
		}
		back.Set("code", code)
	}
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// token trades a code for an ID token, once, for the client that asked for
// it, with the PKCE verifier its challenge names.
func (is *Issuer) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	if !ok || id != is.clientID || secret != is.clientSecret {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	if r.PostForm.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code" {
		is.deviceToken(w, id, r.PostForm.Get("device_code"))
		return
	}
	code := r.PostForm.Get("code")
	g := is.grants[code]
	delete(is.grants, code)
	if r.PostForm.Get("grant_type") != "authorization_code" || g == nil || g.clientID != id || g.redirectURI != r.PostForm.Get("redirect_uri") {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	if g.challenge != "" {
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if g.method != "S256" || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
	}
	is.issue(w, id, map[string]any{"nonce": g.nonce}, g.claims, g.userinfo)
}

// issue answers a token request with an ID token for claims and an access
// token whose userinfo is userinfo. The caller holds is.mu.
func (is *Issuer) issue(w http.ResponseWriter, clientID string, extra, person, userinfo map[string]any) {
	now := time.Now()
	claims := map[string]any{"iss": is.URL, "aud": clientID, "iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix()}
	for k, v := range extra {
		claims[k] = v
	}
	for k, v := range person {
		claims[k] = v
	}
	if is.idTokenEdit != nil {
		is.idTokenEdit(claims)
	}
	access := rand.Text()
	is.access[access] = userinfo
	writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 7200, "id_token": Sign(is.keys[0], claims)})
}

// SetNoDevice leaves device_authorization_endpoint out of discovery.
func (is *Issuer) SetNoDevice(v bool) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.noDevice = v
}

// SetDevice sets the interval and the life of the device codes the
// issuer hands out from now on. The defaults are one second and five
// minutes.
func (is *Issuer) SetDevice(interval, life time.Duration) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.deviceInterval, is.deviceLife = interval, life
}

// DeviceStarts counts device authorization requests.
func (is *Issuer) DeviceStarts() int {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.deviceStarts
}

// DevicePolls counts device access token requests.
func (is *Issuer) DevicePolls() int {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.devicePolls
}

// SlowDownNext makes the next device access token request answer
// slow_down.
func (is *Issuer) SlowDownNext() {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.slowDownNext = true
}

// GateDeviceStarts holds each device authorization request from now on
// until n requests wait together, or for at most wait, so requests that a
// client sends at once reach the issuer at once.
func (is *Issuer) GateDeviceStarts(n int, wait time.Duration) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.gate = &gate{n: n, wait: wait, open: make(chan struct{})}
}

// authorizeDevice is the device authorization endpoint (RFC 8628, section
// 3.1), for the one client SetClient registered.
func (is *Issuer) authorizeDevice(w http.ResponseWriter, r *http.Request) {
	is.mu.Lock()
	g := is.gate
	is.mu.Unlock()
	if g != nil {
		g.pass()
	}
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	is.deviceStarts++
	if !ok || id != is.clientID || secret != is.clientSecret || r.PostForm.Get("client_id") != id {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	code := rand.Text() + rand.Text()
	user := rand.Text()[:4] + "-" + rand.Text()[:4]
	is.devices[code] = &device{clientID: id, userCode: user, expires: time.Now().Add(is.deviceLife), state: "pending"}
	writeJSON(w, map[string]any{
		"device_code": code, "user_code": user,
		"verification_uri":          is.URL + "/oauth/device",
		"verification_uri_complete": is.URL + "/oauth/device?user_code=" + user,
		"expires_in":                int64(is.deviceLife / time.Second), "interval": int64(is.deviceInterval / time.Second),
	})
}

// ApproveDevice stands in for the person who opens the verification page,
// types userCode, and consents: from now on the device code's token
// request returns an ID token for the person SetPerson names. It reports
// whether a pending code had that user code.
func (is *Issuer) ApproveDevice(userCode string) bool { return is.decideDevice(userCode, "approved") }

// DenyDevice is a person who refuses consent for userCode.
func (is *Issuer) DenyDevice(userCode string) bool { return is.decideDevice(userCode, "denied") }

func (is *Issuer) decideDevice(userCode, state string) bool {
	is.mu.Lock()
	defer is.mu.Unlock()
	for _, d := range is.devices {
		if d.userCode == userCode && d.state == "pending" && time.Now().Before(d.expires) {
			d.state, d.claims, d.userinfo = state, is.person, is.userinfo
			return true
		}
	}
	return false
}

// deviceToken answers a device access token request (RFC 8628, section
// 3.5). The caller holds is.mu and checked the client.
func (is *Issuer) deviceToken(w http.ResponseWriter, clientID, code string) {
	d := is.devices[code]
	now := time.Now()
	is.devicePolls++
	slowDown := is.slowDownNext
	is.slowDownNext = false
	switch {
	case d == nil || d.clientID != clientID:
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	case now.After(d.expires):
		delete(is.devices, code)
		oauthError(w, http.StatusBadRequest, "expired_token")
		return
	case d.state == "denied":
		delete(is.devices, code)
		oauthError(w, http.StatusBadRequest, "access_denied")
		return
	// A poll counts as too early only under half the interval, so the
	// jitter of a client that keeps the interval never trips it.
	case slowDown || !d.lastPoll.IsZero() && now.Sub(d.lastPoll) < is.deviceInterval/2:
		d.lastPoll = now
		oauthError(w, http.StatusBadRequest, "slow_down")
		return
	case d.state == "pending":
		d.lastPoll = now
		oauthError(w, http.StatusBadRequest, "authorization_pending")
		return
	}
	delete(is.devices, code)
	is.issue(w, clientID, nil, d.claims, d.userinfo)
}

func (is *Issuer) userinfoHandler(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	is.mu.Lock()
	info, known := is.access[tok]
	is.mu.Unlock()
	if !ok || !known {
		oauthError(w, http.StatusUnauthorized, "invalid_token")
		return
	}
	writeJSON(w, info)
}

// GitLabPerson returns the claims of an ID token from GitLab for user 2002,
// bob, and the userinfo answer with the groups GitLab puts there, in the
// shape of docs.gitlab.com/integration/openid_connect_provider/, read
// 2026-11-10.
func GitLabPerson() (claims, userinfo map[string]any) {
	claims = map[string]any{
		"sub": "2002", "auth_time": time.Now().Unix(), "sub_legacy": "3c5b1a",
		"name": "Bob Example", "nickname": "bob", "preferred_username": "bob",
		"email": "bob@example.com", "email_verified": true, "groups_direct": []string{"acme/platform"},
	}
	userinfo = map[string]any{
		"sub": "2002", "name": "Bob Example", "nickname": "bob", "preferred_username": "bob",
		"email": "bob@example.com", "email_verified": true, "groups": []string{"acme", "acme/platform"},
	}
	return claims, userinfo
}

// Endpoint is one of the issuer's endpoints, for SetEcho.
type Endpoint string

const (
	Discovery           Endpoint = "discovery"
	KeySet              Endpoint = "key set"
	DeviceAuthorization Endpoint = "device authorization"
	TokenEndpoint       Endpoint = "token endpoint"
	Userinfo            Endpoint = "userinfo"
)

var endpointPaths = map[string]Endpoint{
	"/.well-known/openid-configuration": Discovery,
	"/oauth/discovery/keys":             KeySet,
	"/oauth/authorize_device":           DeviceAuthorization,
	"/oauth/token":                      TokenEndpoint,
	"/oauth/userinfo":                   Userinfo,
}

// EchoMarker is in every answer an echoing endpoint gives, so a test can
// find the answer's body even where the request carried no credential.
const EchoMarker = "oidctest-echoed-answer-body"

// SetEcho makes endpoint e refuse every request with an answer that
// repeats each credential the request carried, as a provider or a proxy
// that reflects its request into an error does: in an RFC 6749 error's
// error_description and error_uri at the device authorization and token
// endpoints, and as the plain text body elsewhere. "" turns it off.
func (is *Issuer) SetEcho(e Endpoint) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.echo, is.echoed = e, nil
}

// Echoed returns the credentials the echoing endpoint received and put in
// its answers: bearer tokens, client secrets, codes, code verifiers, and
// device codes.
func (is *Issuer) Echoed() []string {
	is.mu.Lock()
	defer is.mu.Unlock()
	return append([]string(nil), is.echoed...)
}

func (is *Issuer) echoing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		is.mu.Lock()
		on := is.echo != "" && endpointPaths[r.URL.Path] == is.echo
		is.mu.Unlock()
		if !on {
			next.ServeHTTP(w, r)
			return
		}
		var creds []string
		auth := r.Header.Get("Authorization")
		if tok, ok := strings.CutPrefix(auth, "Bearer "); ok {
			creds = append(creds, tok)
		}
		if _, secret, ok := r.BasicAuth(); ok {
			secret, _ = url.QueryUnescape(secret)
			creds = append(creds, secret)
		}
		_ = r.ParseForm()
		for _, k := range []string{"client_secret", "code", "code_verifier", "device_code", "refresh_token"} {
			if v := r.PostForm.Get(k); v != "" {
				creds = append(creds, v)
			}
		}
		is.mu.Lock()
		is.echoed = append(is.echoed, creds...)
		is.mu.Unlock()
		// The credentials come first, so a message that keeps only the
		// start of the answer still holds them.
		echo := strings.Join(creds, " ") + " " + EchoMarker + " Authorization: " + auth + " form: " + r.PostForm.Encode()
		switch is.echo {
		case DeviceAuthorization, TokenEndpoint:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "invalid_request", "error_description": echo, "error_uri": "https://example.com/" + url.PathEscape(echo),
			})
		case Userinfo:
			http.Error(w, echo, http.StatusUnauthorized)
		default:
			http.Error(w, echo, http.StatusBadRequest)
		}
	})
}

// Answer is what an endpoint of the issuer would have answered.
type Answer struct {
	Status int
	Header http.Header
	Body   []byte
}

// Tamper writes the answer to r in place of the issuer's own, which it
// gets as a. It may write anything, or hijack the connection.
type Tamper func(w http.ResponseWriter, r *http.Request, a Answer)

// SetTamper makes every answer of endpoint e go through fn, as a hostile
// or broken identity provider or proxy would answer. Nil turns it off.
func (is *Issuer) SetTamper(e Endpoint, fn Tamper) {
	is.mu.Lock()
	defer is.mu.Unlock()
	if is.tamper == nil {
		is.tamper = map[Endpoint]Tamper{}
	}
	is.tamper[e] = fn
}

func (is *Issuer) tampering(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		is.mu.Lock()
		fn := is.tamper[endpointPaths[r.URL.Path]]
		is.mu.Unlock()
		if fn == nil {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		fn(w, r, Answer{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()})
	})
}
