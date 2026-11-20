package ui_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/sqldb"
	"github.com/mr-karan/writ/internal/ui"
)

// TestIdentityProviderTextNeverLeaks is the property behind internal/oidc's
// typed refusals: whatever an identity provider or a token holds, none of
// it reaches an HTTP answer, a log line, or a row of the control database.
// Each case puts a random marker into one endpoint's answer or one token
// field, drives it through every path that reads it (writ serve's start,
// writ login's device flow, the control UI's sign-in, a CI job's token, and
// a key set refresh), and looks for the marker, its first ten characters,
// and every credential the requests carried, in all three places.
//
// A case whose marker would not fail a check on its own, such as a
// subject, also expires the token, so the case tests a refused token; a
// verified token's subject is meant to be stored.
func TestIdentityProviderTextNeverLeaks(t *testing.T) {
	for _, c := range leakCases() {
		for _, path := range c.paths {
			t.Run(path+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				runLeakCase(t, c, path)
			})
		}
	}
}

// Paths through which writ serve reads what an identity provider sends.
const (
	pathStart   = "start"
	pathDevice  = "device"
	pathUI      = "ui"
	pathCI      = "ci"
	pathRefresh = "refresh"
)

// leakCase is one hostile answer or token. tampers rewrite the answers of
// endpoints; ci, for the ci and refresh paths, edits a CI job's token.
type leakCase struct {
	name    string
	paths   []string
	tampers map[oidctest.Endpoint]func(m string, is *oidctest.Issuer) oidctest.Tamper
	ci      func(m string, header, claims map[string]any) string
}

func tamperAt(e oidctest.Endpoint, fn func(m string, is *oidctest.Issuer) oidctest.Tamper) map[oidctest.Endpoint]func(string, *oidctest.Issuer) oidctest.Tamper {
	return map[oidctest.Endpoint]func(string, *oidctest.Issuer) oidctest.Tamper{e: fn}
}

func leakCases() []leakCase {
	var out []leakCase
	// Answers any endpoint can give.
	answers := map[string]func(m string, is *oidctest.Issuer) oidctest.Tamper{
		"status-and-text": func(m string, _ *oidctest.Issuer) oidctest.Tamper { return writeRaw(500, "text/plain", m) },
		"oauth-error": func(m string, _ *oidctest.Issuer) oidctest.Tamper {
			return writeJSONAnswer(400, map[string]any{"error": m, "error_description": m, "error_uri": "https://example.com/" + m})
		},
		"known-error-with-description": func(m string, _ *oidctest.Issuer) oidctest.Tamper {
			return writeJSONAnswer(400, map[string]any{"error": "invalid_request", "error_description": m})
		},
		"malformed-json": func(m string, _ *oidctest.Issuer) oidctest.Tamper {
			return writeRaw(200, "application/json", `{"`+m+`":`)
		},
		"oversized": func(m string, _ *oidctest.Issuer) oidctest.Tamper {
			return writeRaw(200, "application/json", `{"x":"`+m+strings.Repeat("A", 1<<20)+m+`"}`)
		},
		"html": func(m string, _ *oidctest.Issuer) oidctest.Tamper { return writeRaw(200, "text/html", "<p>"+m+"</p>") },
		"redirect": func(m string, is *oidctest.Issuer) oidctest.Tamper {
			return func(w http.ResponseWriter, _ *http.Request, _ oidctest.Answer) {
				w.Header().Set("Location", is.URL+"/elsewhere?"+m)
				w.WriteHeader(http.StatusFound)
			}
		},
		"status-line": func(m string, _ *oidctest.Issuer) oidctest.Tamper {
			return func(w http.ResponseWriter, _ *http.Request, _ oidctest.Answer) {
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				_, _ = buf.WriteString("HTTP/1.1 " + m + "\r\n\r\n")
				_ = buf.Flush()
				_ = conn.Close()
			}
		},
		"headers": func(m string, _ *oidctest.Issuer) oidctest.Tamper {
			return func(w http.ResponseWriter, _ *http.Request, _ oidctest.Answer) {
				w.Header().Set("WWW-Authenticate", `Bearer error="`+m+`"`)
				w.Header().Set("X-Debug", m)
				w.WriteHeader(http.StatusUnauthorized)
			}
		},
	}
	endpoints := []struct {
		e     oidctest.Endpoint
		paths []string
	}{
		{oidctest.Discovery, []string{pathStart}},
		{oidctest.KeySet, []string{pathStart, pathRefresh}},
		{oidctest.DeviceAuthorization, []string{pathDevice}},
		{oidctest.TokenEndpoint, []string{pathDevice, pathUI}},
		{oidctest.Userinfo, []string{pathDevice, pathUI}},
	}
	for _, ep := range endpoints {
		for name, fn := range answers {
			out = append(out, leakCase{name: string(ep.e) + "/" + name, paths: ep.paths, tampers: tamperAt(ep.e, fn)})
		}
	}

	// Discovery fields.
	for field, value := range map[string]func(m string) any{
		"issuer":                           func(m string) any { return m },
		"jwks_uri":                         func(m string) any { return "https://" + m + ".invalid/keys" },
		"authorization_endpoint":           func(m string) any { return m },
		"token_endpoint":                   func(m string) any { return "http://" + m + ".invalid/token" },
		"userinfo_endpoint":                func(m string) any { return []string{m} },
		"device_authorization_endpoint":    func(m string) any { return "http://" + m + ".invalid/device" },
		"code_challenge_methods_supported": func(m string) any { return m },
	} {
		out = append(out, leakCase{name: "discovery/" + field, paths: []string{pathStart}, tampers: tamperAt(oidctest.Discovery,
			editJSON(func(doc map[string]any, m string) { doc[field] = value(m) }))})
	}

	// Key set fields, on the RSA key every token is signed with.
	for field, edit := range map[string]func(key map[string]any, m string){
		"kid-bad-key": func(k map[string]any, m string) { k["kid"], k["n"] = m, "AQ" },
		"kty":         func(k map[string]any, m string) { k["kty"] = m },
		"crv":         func(k map[string]any, m string) { k["kty"], k["crv"] = "EC", m },
		"n":           func(k map[string]any, m string) { k["n"] = m },
		"e":           func(k map[string]any, m string) { k["e"] = m },
		"alg":         func(k map[string]any, m string) { k["alg"] = m },
		"use":         func(k map[string]any, m string) { k["use"] = m },
		"private":     func(k map[string]any, m string) { k["d"] = m },
		"x5c":         func(k map[string]any, m string) { k["x5c"] = []string{m} },
	} {
		out = append(out, leakCase{name: "key-set/" + field, paths: []string{pathStart, pathRefresh}, tampers: tamperAt(oidctest.KeySet,
			editJSON(func(doc map[string]any, m string) {
				keys, _ := doc["keys"].([]any)
				if len(keys) > 0 {
					if k, ok := keys[0].(map[string]any); ok {
						edit(k, m)
					}
				}
			}))})
	}
	out = append(out,
		leakCase{name: "key-set/kid-twice", paths: []string{pathStart, pathRefresh}, tampers: tamperAt(oidctest.KeySet,
			editJSON(func(doc map[string]any, m string) {
				doc["keys"] = []any{map[string]any{"kty": "RSA", "kid": m, "n": "AQ", "e": "AQAB"}, map[string]any{"kty": "RSA", "kid": m, "n": "AQ", "e": "AQAB"}}
			}))},
		leakCase{name: "key-set/keys-not-a-list", paths: []string{pathStart, pathRefresh}, tampers: tamperAt(oidctest.KeySet,
			editJSON(func(doc map[string]any, m string) { doc["keys"] = m }))},
	)

	// Device authorization fields.
	for field, edit := range map[string]func(doc map[string]any, m string){
		"device_code-oversized":     func(d map[string]any, m string) { d["device_code"] = m + strings.Repeat("A", 1100) },
		"user_code-too-long":        func(d map[string]any, m string) { d["user_code"] = m + m + m },
		"user_code-control":         func(d map[string]any, m string) { d["user_code"] = m + "\x1b[2J" },
		"verification_uri":          func(d map[string]any, m string) { d["verification_uri"] = "https://" + m + ".invalid/device" },
		"verification_uri_complete": func(d map[string]any, m string) { d["verification_uri_complete"] = "https://" + m + ".invalid/device" },
		"expires_in":                func(d map[string]any, m string) { d["expires_in"] = m },
		"interval":                  func(d map[string]any, m string) { d["interval"] = m },
	} {
		out = append(out, leakCase{name: "device-authorization/" + field, paths: []string{pathDevice},
			tampers: tamperAt(oidctest.DeviceAuthorization, editJSON(edit))})
	}

	// Token endpoint answers and ID token fields.
	for field, edit := range map[string]func(doc map[string]any, m string){
		"access_token-without-id_token": func(d map[string]any, m string) { d["access_token"] = m; delete(d, "id_token") },
		"id_token-not-a-jwt":            func(d map[string]any, m string) { d["id_token"] = m },
		"id_token-header-not-base64":    func(d map[string]any, m string) { d["id_token"] = m + ".e30.AA" },
		"id_token-oversized":            func(d map[string]any, m string) { d["id_token"] = m + strings.Repeat(".", 17<<10) },
		"id_token-number":               func(d map[string]any, _ string) { d["id_token"] = 7 },
		"token_type":                    func(d map[string]any, m string) { d["token_type"] = m },
	} {
		out = append(out, leakCase{name: "token/" + field, paths: []string{pathDevice, pathUI}, tampers: tamperAt(oidctest.TokenEndpoint, editJSON(edit))})
	}
	for field, edit := range tokenEdits() {
		out = append(out, leakCase{name: "id-token/" + field, paths: []string{pathDevice, pathUI},
			tampers: tamperAt(oidctest.TokenEndpoint, editIDToken(edit))})
		out = append(out, leakCase{name: "ci-token/" + field, paths: []string{pathCI}, ci: func(m string, h, c map[string]any) string {
			edit.edit(m, h, c)
			return ""
		}})
	}
	out = append(out,
		leakCase{name: "id-token/stranger-signature", paths: []string{pathDevice, pathUI}, tampers: tamperAt(oidctest.TokenEndpoint,
			editIDTokenWith(func(m string, h, c map[string]any) { c["x-"+m] = m }, true))},
		leakCase{name: "id-token/member-twice", paths: []string{pathDevice, pathUI}, tampers: tamperAt(oidctest.TokenEndpoint,
			func(m string, is *oidctest.Issuer) oidctest.Tamper {
				return replaceIDToken(func(string) string {
					return oidctest.SignBytes(is.Keys()[0], []byte(`{"alg":"RS256","kid":"rsa-1"}`), []byte(`{"sub":"`+m+`","sub":"`+m+`"}`))
				})
			})},
		leakCase{name: "id-token/unknown-kid-refresh", paths: []string{pathDevice, pathUI}, tampers: map[oidctest.Endpoint]func(string, *oidctest.Issuer) oidctest.Tamper{
			oidctest.TokenEndpoint: editIDTokenWith(func(m string, h, c map[string]any) { h["kid"] = "rotated" }, false),
			oidctest.KeySet: editJSON(func(doc map[string]any, m string) {
				doc["keys"] = []any{map[string]any{"kty": "RSA", "kid": m, "n": m, "e": "AQAB"}}
			}),
		}},
	)

	// Userinfo fields.
	for field, edit := range map[string]func(doc map[string]any, m string){
		"sub":    func(d map[string]any, m string) { d["sub"] = m },
		"groups": func(d map[string]any, m string) { d["groups"] = []string{m} },
		"name":   func(d map[string]any, m string) { d["preferred_username"], d["name"] = m, m },
		"no-sub": func(d map[string]any, m string) { clear(d); d[m] = []any{m} },
	} {
		out = append(out, leakCase{name: "userinfo/" + field, paths: []string{pathDevice, pathUI}, tampers: tamperAt(oidctest.Userinfo, editJSON(edit))})
	}

	// CI tokens that are not JWTs, and a key refresh an unknown kid causes.
	for name, raw := range map[string]func(m string) string{
		"raw-text":    func(m string) string { return m },
		"three-parts": func(m string) string { return "a." + m + ".c" },
		"header-json": func(m string) string { return b64url(`{"alg":"`+m+`"`) + ".e30.AA" },
		"payload-json": func(m string) string {
			return b64url(`{"alg":"RS256","kid":"rsa-1"}`) + "." + b64url(`{"`+m+`"`) + ".AA"
		},
		"oversized":  func(m string) string { return strings.Repeat(m, 1000) },
		"five-parts": func(m string) string { return m + "." + m + "." + m + "." + m + "." + m },
	} {
		out = append(out, leakCase{name: "ci-raw/" + name, paths: []string{pathCI}, ci: func(m string, _, _ map[string]any) string { return raw(m) }})
	}
	return out
}

// tokenEdit changes an ID token's or a CI token's header and claims. An
// edit whose marker alone would not refuse the token also expires it.
type tokenEdit struct {
	edit func(m string, header, claims map[string]any)
}

func tokenEdits() map[string]tokenEdit {
	expired := func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }
	return map[string]tokenEdit{
		"header-alg":   {func(m string, h, _ map[string]any) { h["alg"] = m }},
		"header-kid":   {func(m string, h, _ map[string]any) { h["kid"] = m }},
		"header-typ":   {func(m string, h, c map[string]any) { h["typ"] = m; expired(c) }},
		"header-jku":   {func(m string, h, _ map[string]any) { h["jku"] = "https://" + m + ".invalid/" }},
		"header-x5c":   {func(m string, h, _ map[string]any) { h["x5c"] = []string{m} }},
		"header-crit":  {func(m string, h, _ map[string]any) { h["crit"] = []string{m} }},
		"iss":          {func(m string, _, c map[string]any) { c["iss"] = m }},
		"iss-url":      {func(m string, _, c map[string]any) { c["iss"] = "https://" + m + ".invalid" }},
		"aud":          {func(m string, _, c map[string]any) { c["aud"] = m }},
		"aud-list":     {func(m string, _, c map[string]any) { c["aud"] = []any{m, c["aud"]} }},
		"azp":          {func(m string, _, c map[string]any) { c["azp"] = m }},
		"sub":          {func(m string, _, c map[string]any) { c["sub"] = m; expired(c) }},
		"sub-number":   {func(m string, _, c map[string]any) { c["sub"] = map[string]any{m: m} }},
		"jti":          {func(m string, _, c map[string]any) { c["jti"] = m; expired(c) }},
		"nonce":        {func(m string, _, c map[string]any) { c["nonce"] = m; expired(c) }},
		"groups":       {func(m string, _, c map[string]any) { c["groups"] = []string{m}; expired(c) }},
		"name":         {func(m string, _, c map[string]any) { c["preferred_username"], c["name"] = m, m; expired(c) }},
		"exp":          {func(m string, _, c map[string]any) { c["exp"] = m }},
		"iat":          {func(m string, _, c map[string]any) { c["iat"] = m }},
		"nbf":          {func(m string, _, c map[string]any) { c["nbf"] = m }},
		"project_id":   {func(m string, _, c map[string]any) { c["project_id"] = map[string]any{m: 1} }},
		"ref":          {func(m string, _, c map[string]any) { c["ref_protected"] = m }},
		"claim-twice":  {func(m string, _, c map[string]any) { c[m] = m; expired(c) }},
		"many-members": {func(m string, _, c map[string]any) { manyMembers(c, m) }},
	}
}

func manyMembers(c map[string]any, m string) {
	for i := range 300 {
		c[fmt.Sprintf("%s-%d", m, i)] = m
	}
}

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func writeRaw(status int, ctype, body string) oidctest.Tamper {
	return func(w http.ResponseWriter, _ *http.Request, _ oidctest.Answer) {
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func writeJSONAnswer(status int, doc any) oidctest.Tamper {
	return func(w http.ResponseWriter, _ *http.Request, _ oidctest.Answer) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// editJSON rewrites a JSON object answer with edit, keeping its status. An
// answer that is not an object, such as authorization_pending, passes.
func editJSON(edit func(doc map[string]any, m string)) func(string, *oidctest.Issuer) oidctest.Tamper {
	return func(m string, _ *oidctest.Issuer) oidctest.Tamper {
		return func(w http.ResponseWriter, _ *http.Request, a oidctest.Answer) {
			var doc map[string]any
			if json.Unmarshal(a.Body, &doc) != nil || doc["error"] != nil {
				w.Header().Set("Content-Type", a.Header.Get("Content-Type"))
				w.WriteHeader(a.Status)
				_, _ = w.Write(a.Body)
				return
			}
			edit(doc, m)
			writeJSONAnswer(a.Status, doc)(w, nil, a)
		}
	}
}

// replaceIDToken swaps the id_token of a token answer for make's.
func replaceIDToken(make func(raw string) string) oidctest.Tamper {
	return func(w http.ResponseWriter, r *http.Request, a oidctest.Answer) {
		editJSON(func(doc map[string]any, _ string) {
			if raw, ok := doc["id_token"].(string); ok {
				doc["id_token"] = make(raw)
			}
		})("", nil)(w, r, a)
	}
}

func editIDToken(e tokenEdit) func(string, *oidctest.Issuer) oidctest.Tamper {
	return editIDTokenWith(e.edit, false)
}

// editIDTokenWith re-signs the ID token of a token answer after edit, with
// the issuer's key, or a stranger's.
func editIDTokenWith(edit func(m string, header, claims map[string]any), stranger bool) func(string, *oidctest.Issuer) oidctest.Tamper {
	return func(m string, is *oidctest.Issuer) oidctest.Tamper {
		return replaceIDToken(func(raw string) string {
			parts := strings.Split(raw, ".")
			b, _ := base64.RawURLEncoding.DecodeString(parts[1])
			claims := map[string]any{}
			_ = json.Unmarshal(b, &claims)
			key := is.Keys()[0]
			header := map[string]any{"alg": "RS256", "kid": key.ID, "typ": "JWT"}
			edit(m, header, claims)
			if stranger {
				key = leakKeys().stranger
			}
			return oidctest.SignWith(key, header, claims)
		})
	}
}

type sharedKeys struct {
	rsa, ec, stranger, rotated oidctest.Key
}

// leakKeys are made once: every case's issuer publishes the same keys.
var leakKeys = sync.OnceValue(func() sharedKeys {
	rsaKey := func(id string) oidctest.Key {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		return oidctest.Key{ID: id, RSA: k}
	}
	e, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return sharedKeys{rsa: rsaKey("rsa-1"), ec: oidctest.Key{ID: "ec-1", EC: e}, stranger: rsaKey("rsa-1"), rotated: rsaKey("rotated")}
})

// sinks collects every place text can leave writ serve in a case.
type sinks struct {
	mu      sync.Mutex
	answers strings.Builder
	log     lockedLog
	secrets []string
	reached map[oidctest.Endpoint]bool
}

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (s *sinks) answer(status int, h http.Header, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(&s.answers, "%d\n", status)
	for k, vs := range h {
		fmt.Fprintf(&s.answers, "%s: %s\n", k, strings.Join(vs, ", "))
	}
	s.answers.WriteString(body + "\n")
}

func (s *sinks) secret(v ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range v {
		if len(x) >= 8 {
			s.secrets = append(s.secrets, x)
		}
	}
}

// recordCredentials wraps a tamper to note each credential its request
// carried and each token its answer holds.
func recordCredentials(s *sinks, e oidctest.Endpoint, fn oidctest.Tamper) oidctest.Tamper {
	return func(w http.ResponseWriter, r *http.Request, a oidctest.Answer) {
		s.mu.Lock()
		s.reached[e] = true
		s.mu.Unlock()
		if _, secret, ok := r.BasicAuth(); ok {
			if v, err := url.QueryUnescape(secret); err == nil {
				s.secret(v)
			}
		}
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			s.secret(tok)
		}
		for _, k := range []string{"code", "device_code", "code_verifier", "refresh_token", "client_secret"} {
			s.secret(r.PostForm.Get(k))
		}
		var doc map[string]any
		if json.Unmarshal(a.Body, &doc) == nil {
			for _, k := range []string{"access_token", "refresh_token", "id_token", "device_code"} {
				if v, ok := doc[k].(string); ok {
					s.secret(v)
				}
			}
		}
		fn(w, r, a)
	}
}

func runLeakCase(t *testing.T, c leakCase, path string) {
	marker := "MK" + rand.Text()
	keys := leakKeys()
	is := oidctest.NewWithKeys(t, keys.rsa, keys.ec)
	is.SetClient("writ-client", clientSec)
	is.SetPerson(oidctest.GitLabPerson())
	is.SetDevice(time.Second, 5*time.Minute)
	s := &sinks{reached: map[oidctest.Endpoint]bool{}}
	s.secret(marker, marker[:10], clientSec)
	tamper := func() {
		for e, fn := range c.tampers {
			is.SetTamper(e, recordCredentials(s, e, fn(marker, is)))
		}
	}
	log := slog.New(slog.NewTextHandler(&s.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if path == pathStart {
		tamper()
	}
	lf, startErr := newLeakFixture(t, is, log)
	switch {
	case path == pathStart:
		if startErr != nil {
			s.answer(0, nil, startErr.Error())
		}
	case startErr != nil:
		t.Fatal(startErr)
	default:
		tamper()
		lf.drive(t, c, path, marker, s)
	}
	for e := range c.tampers {
		if !s.reached[e] {
			t.Errorf("the case never reached the %s", e)
		}
	}
	db := ""
	if lf != nil {
		db = dumpControl(t, lf.loc)
	}
	for sink, text := range map[string]string{"an answer": s.answers.String(), "the log": s.log.String(), "the control database": db} {
		for _, secret := range s.secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s holds %q:\n%s", sink, secret, excerpt(text, secret))
				break
			}
		}
	}
}

func excerpt(text, secret string) string {
	i := strings.Index(text, secret)
	lo, hi := max(0, i-300), min(len(text), i+len(secret)+100)
	return text[lo:hi]
}

// leakFixture is writ serve with the control UI, over HTTPS, trusting
// is as both a CI issuer and the people issuer.
type leakFixture struct {
	*fixture
}

func newLeakFixture(t *testing.T, is *oidctest.Issuer, log *slog.Logger) (*leakFixture, error) {
	t.Helper()
	var handler http.Handler = http.NotFoundHandler()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	ts.StartTLS()
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ca := write("issuer.pem", string(is.CAPEM()))
	secret := write("client_secret", clientSec+"\n")
	store, loc := controltest.Open(t, controltest.Default())
	db := loc
	if !sqldb.IsPostgres(loc) {
		db = "postgres://writ@localhost/unused"
	}
	text := fmt.Sprintf(`url      = %q
listen   = "127.0.0.1:1"
database = %q
data     = %q

limits {
  requests_per_second  = 1000
  requests_per_address = 1000
}

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = %q
  ca_file  = %q
}

issuer "sso" {
  kind               = "people"
  identity           = "gitlab"
  url                = %q
  ca_file            = %q
  client_id          = "writ-client"
  client_secret_file = %q
  groups             = "userinfo"
}

team "infra_mr" {
  issuer = "gitlab"
  match  = { project_id = ["4242"] }
}

team "platform" {
  issuer = "sso"
  match  = { groups = ["acme/platform"] }
}

grant {
  teams  = ["infra_mr", "platform"]
  stacks = ["*"]
  envs   = ["prod"]
  can    = ["read"]
}

env "prod" {
  key = "prod.key"
}

`, ts.URL, db, dir, is.URL, ca, is.URL, ca, secret)
	cfg, diags := server.ParseConfig(filepath.Join(dir, "serve"), []byte(text))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	ctx := context.Background()
	v, err := server.NewVerifier(ctx, cfg, log, nil)
	if err != nil {
		return nil, err
	}
	people, err := server.People(ctx, cfg, log, nil)
	if err != nil {
		return nil, err
	}
	srv, err := server.New(ctx, server.Options{Config: cfg, Store: store, Verifier: v, People: people, Version: "test", Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	u, err := ui.New(ui.Options{Server: srv, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	tr := watch(t, u)
	u.Mount()
	handler = srv.Handler()
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	roots.AppendCertsFromPEM(is.CAPEM())
	return &leakFixture{fixture: &fixture{t: t, is: is, https: ts, url: ts.URL, srv: srv, store: store, loc: loc, clock: &clock{now: time.Now()}, roots: roots, traffic: tr}}, nil
}

func (lf *leakFixture) client() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: lf.roots}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// call sends one API request and records the answer.
func (lf *leakFixture) call(t *testing.T, s *sinks, method, path, token, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(method, lf.url+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := lf.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	s.answer(resp.StatusCode, resp.Header, string(b))
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	return doc
}

func (lf *leakFixture) drive(t *testing.T, c leakCase, path, marker string, s *sinks) {
	t.Helper()
	switch path {
	case pathDevice:
		doc := lf.call(t, s, "POST", "/v1/auth/device", "", `{}`)
		if doc["ok"] != true {
			return
		}
		code, _ := doc["user_code"].(string)
		lf.is.ApproveDevice(code)
		handle, _ := doc["handle"].(string)
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			doc := lf.call(t, s, "POST", "/v1/auth/device/token", "", `{"handle":"`+handle+`"}`)
			if doc["status"] != server.SessionPending {
				return
			}
		}
		t.Fatal("the sign-in did not end")
	case pathUI:
		b := lf.browser()
		cb := startSignIn(t, b, "/ui/")
		p := b.get(cb)
		s.answer(p.status, p.header, p.body)
		u, _ := url.Parse(cb)
		q := u.Query()
		q.Del("code")
		q.Set("error", marker)
		b2 := lf.browser()
		cb2 := startSignIn(t, b2, "/ui/")
		u2, _ := url.Parse(cb2)
		q.Set("state", u2.Query().Get("state"))
		p = b2.get("/ui/callback?" + q.Encode())
		s.answer(p.status, p.header, p.body)
	case pathCI, pathRefresh:
		key := lf.is.Keys()[0]
		header := map[string]any{"alg": "RS256", "kid": key.ID, "typ": "JWT"}
		claims := oidctest.GitLabJob(lf.is.URL, lf.url, time.Now())
		raw := ""
		if c.ci != nil {
			raw = c.ci(marker, header, claims)
		}
		if path == pathRefresh {
			// An unknown kid makes the verifier fetch the key set again.
			key = leakKeys().rotated
			header["kid"] = key.ID
		}
		if raw == "" {
			raw = oidctest.SignWith(key, header, claims)
		}
		s.secret(raw)
		lf.call(t, s, "GET", "/v1/whoami", raw, "")
	}
}

// dumpControl is every row of every table of the control database, as
// text.
func dumpControl(t *testing.T, loc string) string {
	t.Helper()
	raw := controltest.Raw(t, loc)
	ctx := context.Background()
	q := `SELECT name FROM sqlite_master WHERE type = 'table'`
	if sqldb.IsPostgres(loc) {
		q = `SELECT table_name FROM information_schema.tables WHERE table_schema = '` + control.Schema + `'`
	}
	rows, err := raw.QueryContext(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var b strings.Builder
	for _, table := range tables {
		rows, err := raw.QueryContext(ctx, `SELECT * FROM "`+table+`"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			b.WriteString(table + ":")
			for _, v := range vals {
				if bs, ok := v.([]byte); ok {
					v = string(bs)
				}
				fmt.Fprintf(&b, "\t%v", v)
			}
			b.WriteString("\n")
		}
		rows.Close()
	}
	return b.String()
}

// A value OpenTofu marks sensitive never reaches an answer of the control
// UI, on any route and from any template, nor the documents its pages
// offer as JSON, nor the drift a drift check of the recorded plan found:
// the recorded plans carry the stack's two canaries in a replaced pet's
// keepers, a file's content, its drift, and an output, and each page shows
// only the mask's (sensitive) in their place. A plan of a state with no
// secret set, which writ serve keeps nothing of, plans the canaries too.
func TestTofuSensitiveValuesNeverRender(t *testing.T) {
	recorded := ""
	for _, name := range []string{second.plan, third.plan} {
		b, err := os.ReadFile(filepath.Join("testdata", "tofu", name))
		if err != nil {
			t.Fatal(err)
		}
		recorded += string(b)
	}
	for _, c := range []string{canaryFirst, canarySecond} {
		if !strings.Contains(recorded, c) {
			t.Fatalf("the recorded plans do not hold %s, so this test would prove nothing", c)
		}
	}
	x := newTofuUI(t)
	p := x.plan(second)
	run := x.apply(p.Plan.ID)
	p3 := x.plan(third)
	failed := x.apply(p3.Plan.ID)
	x.noSecretSet("staging")
	unsealed := x.planEnded(second, "staging")
	if unsealed.Plan.Status != "failed" {
		t.Fatalf("the plan of a state with no secret set: %+v", unsealed.Plan)
	}
	x.driftCheck(second)
	driftStacks, _, err := x.store.DriftStacks(context.Background(), server.DevEnv, "app", 1, func(string, string) bool { return true })
	if err != nil || len(driftStacks) != 1 || driftStacks[0].Last == nil {
		t.Fatalf("stored drift check: %+v %v", driftStacks, err)
	}
	driftRef := driftStacks[0].Last.Ref
	adopt := "writ state adopt app --env staging --version 1 --reason REASON"
	for _, path := range []string{"/ui/plans/" + unsealed.Plan.Ref, "/ui/stack?stack=app&env=staging"} {
		if body, _ := x.page(path); !strings.Contains(body, adopt) {
			t.Errorf("%s lacks %q", path, adopt)
		}
	}
	planPage, _ := x.page("/ui/plans/" + p.Plan.ID)
	if !strings.Contains(planPage, "(sensitive)") {
		t.Fatal("the plan's page shows no (sensitive) marker, so the values were not on it")
	}
	for _, path := range []string{
		"/ui/plans/" + p.Plan.ID + "?risk=replace", "/ui/plans/" + p3.Plan.ID,
		"/ui/runs/" + run.Run.ID, "/ui/runs/" + failed.Run.ID, "/ui/runs", "/ui/runs?env=dev&stack=app", "/ui/plans?env=dev",
		"/ui/stack?stack=app&env=dev", "/ui/stack?stack=app&env=dev&view=drift", "/ui/drift/" + driftRef, "/ui/approvals", "/ui/access", "/ui/whoami", "/ui/start", "/ui/repos", "/ui/repos/demo",
		"/ui/repos/new", "/ui/settings",
	} {
		x.page(path)
	}
	gate := make(chan struct{})
	x.tofu.mu.Lock()
	x.tofu.gate = gate
	x.tofu.mu.Unlock()
	planning := x.planStarted(second, server.DevEnv)
	_, home := x.page("/ui/")
	if !strings.Contains(words(home), "Planning now") {
		t.Errorf("the overview lists no plan that plans: %s", words(home))
	}
	close(gate)
	var ended server.RemotePlan
	x.wait("/v1/plans/"+planning, "plan", "planning", &ended)
	for _, path := range staticLinks(home) {
		visit(t, x.c, "GET", x.base+path, nil)
	}
	for _, path := range []string{
		"/ui/plans/" + p.Plan.ID + "?as=json", "/ui/runs/" + run.Run.ID + "?as=json", "/ui/runs/" + failed.Run.ID + "?as=json",
		"/ui/plans/" + p3.Plan.ID + "?as=json", "/ui/plans/" + unsealed.Plan.Ref + "?as=json", "/ui/drift/" + driftRef + "?as=json", "/ui/stack?stack=app&env=dev&as=json", "/ui/?as=json", "/ui/no-such-page", "/",
		"/ui/login/start?issuer=sso", "/ui/callback?state=s&code=c",
	} {
		visit(t, x.c, "GET", x.base+path, nil)
	}
	visit(t, x.c, "POST", x.base+"/ui/plans/"+p.Plan.ID+"/approve", url.Values{})
	visit(t, x.c, "POST", x.base+"/ui/repos/new", url.Values{"name": {"other"}, "forge": {"dir"}, "project": {"/nonexistent"}})
	visit(t, x.c, "POST", x.base+"/ui/settings", url.Values{"theme": {"dark"}})
	visit(t, x.c, "POST", x.base+"/ui/logout", url.Values{})
	visit(t, x.c, "GET", x.base+"/ui/login", nil)
	x.traffic.covered(t)
	pages := x.traffic.bodies()
	if status, drift := x.srv.Call(httptest.NewRequest("GET", "/", nil), devSession(t, x.srv), "/v1/drift?env=dev"); status != http.StatusOK || !strings.Contains(string(drift), "local_file.note") {
		t.Fatalf("GET /v1/drift: %d %s", status, drift)
	} else {
		pages = append(pages, string(drift))
	}
	for i, page := range pages {
		for _, c := range []string{canaryFirst, canarySecond} {
			for _, form := range []string{c, c[:12], base64.StdEncoding.EncodeToString([]byte(c)), base64.RawURLEncoding.EncodeToString([]byte(c)),
				fmt.Sprintf("%x", c), url.QueryEscape(c)} {
				if strings.Contains(page, form) {
					t.Errorf("page %d holds %q, a form of %s: %s", i, form, c, excerpt(page, form))
				}
			}
		}
	}
}
