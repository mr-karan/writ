package forge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rest is the HTTP side of a forge's REST API: the read token, bounded
// GETs, and the checks that keep the token out of every error and answer.
// GitLab and GitHub differ in the header that carries the token, in what
// a 403 says the token needs, and in whether a rate limit holds calls back.
type rest struct {
	api       string
	tokenFile string
	http      *http.Client
	// auth puts the token on a request's headers.
	auth func(h http.Header, tok string)
	// need is what the token needs, which a 403 names.
	need string
	// limitOf reads when a rate limit an answer reports ends, and nil
	// means the forge's rate limits fail only the call that met them.
	limitOf func(resp *http.Response, now time.Time) (time.Time, bool)

	mu sync.Mutex
	// limitedUntil is when the forge's last rate limit ends. No call goes
	// to it before then.
	limitedUntil time.Time
}

// maxLimitWait bounds how long one rate-limit answer holds calls back, so
// a broken or hostile forge cannot stop reads for longer.
const maxLimitWait = time.Hour

// newHTTPClient is a forge's HTTP client: bounded, following no redirect,
// trusting the roots in caFile besides the system's when it is set.
func newHTTPClient(caFile string) (*http.Client, error) {
	client := &http.Client{Timeout: CallTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		// A redirect could carry the token to another host.
		return http.ErrUseLastResponse
	}}
	if caFile == "" {
		return client, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read the forge's ca_file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the forge's ca_file %s holds no certificate", caFile)
	}
	client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return client, nil
}

func (r *rest) token() (string, error) {
	b, err := os.ReadFile(r.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read the forge's read_token_file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("the forge's read_token_file %s is empty", r.tokenFile)
	}
	return t, nil
}

// call runs fn with the current token and removes the token from any
// error fn returns. Errors carry no text the forge sent, so this is the
// second line of defence.
func (r *rest) call(fn func(tok string) error) error {
	tok, err := r.token()
	if err != nil {
		return err
	}
	return redact(fn(tok), tok)
}

// redacted is an error whose text had the token removed. It unwraps to
// ErrNotFound only, so no caller can reach the text it replaced.
type redacted struct {
	msg      string
	notFound bool
}

func (e *redacted) Error() string { return e.msg }

func (e *redacted) Unwrap() error {
	if e.notFound {
		return ErrNotFound
	}
	return nil
}

func redact(err error, tok string) error {
	if err == nil || tok == "" || !strings.Contains(err.Error(), tok) {
		return err
	}
	return &redacted{msg: strings.ReplaceAll(err.Error(), tok, "[REDACTED]"), notFound: errors.Is(err, ErrNotFound)}
}

// echoed reports that an answer holds the token. A forge never sends the
// token back, so such an answer is refused whole.
func echoed(what string) error {
	return fmt.Errorf("%s: the forge's answer holds the read token, so writ serve refuses it; rotate the token if the forge is not yours", what)
}

// get sends one GET to the API and returns the answer's body and headers,
// reading at most max bytes. A status that is not 200 is an *Error, or a
// *RateLimitError for an answer that limits the rate of calls.
func (r *rest) get(ctx context.Context, tok, what, p string, q url.Values, accept string, max int64) ([]byte, http.Header, error) {
	if until := r.limited(); !until.IsZero() {
		return nil, nil, &RateLimitError{What: what, Until: until}
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	target := r.api + p
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, err
	}
	r.auth(req.Header, tok)
	req.Header.Set("Accept", accept)
	resp, err := r.http.Do(req)
	if err != nil {
		// The error names the URL, which holds no token.
		return nil, nil, fmt.Errorf("%s: %w", what, err)
	}
	defer resp.Body.Close()
	if r.limitOf != nil {
		if until, ok := r.limitOf(resp, time.Now()); ok {
			r.limit(until)
			if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
				return nil, nil, &RateLimitError{What: what, Status: resp.StatusCode, Until: until}
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		// The body is the forge's own text, which is never read.
		return nil, nil, &Error{What: what, Status: resp.StatusCode, Need: r.need}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: read the answer: %w", what, err)
	}
	if int64(len(body)) > max {
		return nil, nil, &TooLargeError{What: what + ": the answer", Max: max}
	}
	if bytes.Contains(body, []byte(tok)) {
		return nil, nil, echoed(what)
	}
	return body, resp.Header, nil
}

// getJSON reads one JSON answer of at most max bytes into v.
func (r *rest) getJSON(ctx context.Context, tok, what, p string, q url.Values, max int64, v any) (http.Header, error) {
	b, h, err := r.get(ctx, tok, what, p, q, "application/json", max)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, fmt.Errorf("%s: the answer is not the JSON the forge sends", what)
	}
	// JSON can spell the token with escapes the raw check misses; the
	// decoded values, encoded again, spell it plainly.
	if again, err := json.Marshal(v); err != nil || bytes.Contains(again, []byte(tok)) {
		return nil, echoed(what)
	}
	return h, nil
}

// secondaryLimitRead bounds what githubLimit reads of a 403's body to
// tell a secondary rate limit from a refusal of the token.
const secondaryLimitRead = 4 << 10

// githubLimit reads when a rate limit a GitHub answer reports ends, as
// GitHub's documentation of rate limits gives them: after Retry-After
// seconds on a 403 or 429, or at X-RateLimit-Reset when
// X-RateLimit-Remaining is 0 on any answer. A 403 whose body says it is a
// secondary rate limit, and a 429, with neither header hold calls back for
// a minute. The body is read only for that, bounded, and discarded: its
// text never reaches an error. The wait is at least a second and at most
// maxLimitWait.
func githubLimit(resp *http.Response, now time.Time) (time.Time, bool) {
	limiting := resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests
	var until time.Time
	if s, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); err == nil && s >= 0 && limiting {
		until = now.Add(time.Duration(min(s, int64(maxLimitWait/time.Second))) * time.Second)
	} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			until = time.Unix(reset, 0)
		}
	}
	if until.IsZero() && resp.StatusCode == http.StatusForbidden {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, secondaryLimitRead))
		if bytes.Contains(bytes.ToLower(b), []byte("secondary rate limit")) {
			until = now.Add(time.Minute)
		}
	}
	if until.IsZero() && resp.StatusCode == http.StatusTooManyRequests {
		until = now.Add(time.Minute)
	}
	if until.IsZero() {
		return time.Time{}, false
	}
	if floor := now.Add(time.Second); until.Before(floor) {
		until = floor
	}
	if ceil := now.Add(maxLimitWait); until.After(ceil) {
		until = ceil
	}
	return until, true
}

// limited is when the forge's rate limit ends, or zero when calls may go.
func (r *rest) limited() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Now().Before(r.limitedUntil) {
		return r.limitedUntil
	}
	return time.Time{}
}

func (r *rest) limit(until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if until.After(r.limitedUntil) {
		r.limitedUntil = until
	}
}
