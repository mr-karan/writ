package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// Bounds of the client.
const (
	remoteTimeout  = 30 * time.Second
	remotePollWait = 60 * time.Second
	remoteDeadline = 30 * time.Minute
	remoteTries    = 5
	maxResponse    = 64 << 20
)

type remote struct {
	base  string
	token string
	// from says how the server was found, and tokenFrom where the token
	// came from, for writ whoami.
	from, tokenFrom string
	// dev is set when the server is the dev control plane, which plans a
	// stack with no environment in its environment dev.
	dev  bool
	http *http.Client
	// seen takes writ serve's version from each answer that names it.
	seen func(version string)
	// refresh asks for a new token where the token came from GitHub
	// Actions, whose ID tokens expire while a long plan runs; nil
	// otherwise.
	refresh func(ctx context.Context) (string, error)
}

// Where a remote command's token came from.
const (
	tokenCI      = "WRIT_ID_TOKEN"
	tokenSession = "session"
	tokenDev     = "dev token"
)

// remote finds the control plane and the token. A token goes only over
// HTTPS, or over HTTP to a loopback address.
func (a *App) remote(ctx context.Context) (*remote, error) {
	base, from, err := a.serverURL()
	if err != nil {
		return nil, err
	}
	dir, err := a.devDir()
	if err != nil {
		return nil, err
	}
	clientPath := filepath.Join(dir, DevClientFile)
	ciToken, _ := a.opts.LookupEnv(tokenCI)
	ciToken = strings.TrimSpace(ciToken)
	if base == "" || base == "dev" {
		c, err := readDevClient(clientPath)
		switch {
		case errors.Is(err, fs.ErrNotExist) && base == "":
			return nil, usagef("no control plane: start one with writ serve --dev, or sign in to your team's with writ login URL")
		case errors.Is(err, fs.ErrNotExist):
			return nil, usagef("--server dev: no dev control plane runs: %s is missing; start one with writ serve --dev", clientPath)
		case err != nil:
			return nil, usagef("%v", err)
		case !answersDev(ctx, c):
			return nil, usagef("the dev control plane in %s does not answer at %s: start it with writ serve --dev", dir, c.URL)
		}
		r := a.newRemote(strings.TrimSuffix(c.URL, "/"))
		r.token, r.from, r.tokenFrom, r.dev = c.Token, "the dev control plane", tokenDev, true
		if from == "--server" {
			r.from = "--server dev"
		}
		if ciToken != "" {
			r.token, r.tokenFrom = ciToken, tokenCI
		}
		return r, nil
	}
	if base, err = checkServer(base, from); err != nil {
		return nil, err
	}
	r := a.newRemote(base)
	r.from = from
	if ciToken != "" {
		r.token, r.tokenFrom = ciToken, tokenCI
		return r, nil
	}
	// A GitHub Actions job may ask for its ID token instead of setting
	// WRIT_ID_TOKEN, for the audience writ serve takes: its own URL.
	if tok, ok, err := a.actionsToken(ctx, base); ok {
		if err != nil {
			return nil, err
		}
		r.token, r.tokenFrom = tok, tokenActions
		r.refresh = func(ctx context.Context) (string, error) {
			tok, _, err := a.actionsToken(ctx, base)
			return tok, err
		}
		return r, nil
	}
	_, doc, err := a.readSessions()
	if err != nil {
		return nil, err
	}
	if s, ok := doc.Sessions[base]; ok {
		if !a.opts.Now().Before(s.EndsAt) {
			return nil, usagef("your session for %s ended at %s: sign in again with writ login %s", base, s.EndsAt.UTC().Format(time.RFC3339), base)
		}
		r.token, r.tokenFrom = s.Token, tokenSession
		return r, nil
	}
	if c, err := readDevClient(clientPath); err == nil && strings.TrimSuffix(c.URL, "/") == base && answersDev(ctx, c) {
		r.token, r.tokenFrom, r.dev = c.Token, tokenDev, true
		return r, nil
	}
	return nil, usagef("no token for %s: sign in with writ login %s; in GitHub Actions give the job permissions: id-token: write; in GitLab CI set id_tokens: { WRIT_ID_TOKEN: { aud: %s } } on the job", base, base, base)
}

// newRemote is a client of the control plane at base, with no token, that
// tells the App writ serve's version.
func (a *App) newRemote(base string) *remote {
	return &remote{base: base, http: apiClient(), seen: a.noteServerVersion}
}

// apiClient is the HTTP client of every request to a control plane. It
// follows no redirect. Go's client keeps the Authorization header on a
// redirect to the same host name, whatever the port or the scheme, so a
// redirect could carry the token over plain HTTP or to another server on
// that host. writ serve never redirects its API.
func apiClient() *http.Client {
	return &http.Client{CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		return &redirectError{to: req.URL.Scheme + "://" + req.URL.Host}
	}}
}

// redirectError is a control plane that answered with a redirect.
type redirectError struct{ to string }

func (e *redirectError) Error() string {
	return "the server answered with a redirect to " + e.to + ", and writ follows none, so a token never leaves the address it was meant for: name the address writ serve answers on with --server or WRIT_SERVER"
}

// remoteError is a refusal from writ serve, whose document was printed.
type remoteError struct {
	status int
	doc    []byte
}

func (e *remoteError) Error() string { return fmt.Sprintf("writ serve answered %d", e.status) }

// call sends one request and returns the status and the body. It retries
// 429 and 503 with backoff, and a GET also after a network error.
func (r *remote) call(ctx context.Context, method, path string, body []byte, timeout time.Duration) (int, []byte, error) {
	var lastErr error
	for try := range remoteTries {
		if try > 0 {
			select {
			case <-time.After(time.Duration(1<<try) * time.Second):
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			}
		}
		status, b, err := r.once(ctx, method, path, body, timeout, nil)
		if err == nil && status == http.StatusUnauthorized && method == http.MethodGet && r.refresh != nil {
			// The Actions token may have expired since it was fetched:
			// fetch another and read once more. A write is never resent.
			tok, rerr := r.refresh(ctx)
			if rerr != nil {
				return 0, nil, rerr
			}
			r.token = tok
			status, b, err = r.once(ctx, method, path, body, timeout, nil)
		}
		_, redirected := errors.AsType[*redirectError](err)
		switch {
		case err != nil && method == http.MethodGet && ctx.Err() == nil && !redirected:
			lastErr = err
			continue
		case err != nil:
			return 0, nil, err
		case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
			lastErr = fmt.Errorf("writ serve answered %d", status)
			if try < remoteTries-1 {
				continue
			}
		}
		return status, b, nil
	}
	return 0, nil, lastErr
}

// once sends one request, with the headers in hdr besides the token's.
func (r *remote) once(ctx context.Context, method, path string, body []byte, timeout time.Duration, hdr http.Header) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, r.base+path, errors.Unwrap(err))
	}
	defer resp.Body.Close()
	if v := resp.Header.Get(server.VersionHeader); v != "" && r.seen != nil {
		r.seen(v[:min(len(v), 64)])
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, b, nil
}

// get fetches a document and refuses a body that is not one.
func (r *remote) get(ctx context.Context, path string, timeout time.Duration) ([]byte, error) {
	status, b, err := r.call(ctx, http.MethodGet, path, nil, timeout)
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("writ serve answered %d with a body that is not JSON", status)
	}
	if status >= 300 {
		return b, &remoteError{status: status, doc: b}
	}
	return b, nil
}
