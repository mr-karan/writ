package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The variables GitHub Actions sets in a job whose permissions include
// id-token: write, with which the job asks for an ID token.
const (
	actionsTokenURL     = "ACTIONS_ID_TOKEN_REQUEST_URL"
	actionsRequestToken = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
)

// tokenActions is where a token fetched from GitHub Actions came from,
// for writ whoami.
const tokenActions = "GitHub Actions' ID token"

// Bounds of the ID token request.
const (
	actionsTimeout = 10 * time.Second
	actionsMaxBody = 64 << 10
)

// actionsToken fetches the job's ID token for audience from GitHub
// Actions, as actions/core's getIDToken does: a GET of
// ACTIONS_ID_TOKEN_REQUEST_URL with audience added to its query, carrying
// ACTIONS_ID_TOKEN_REQUEST_TOKEN as a bearer token. ok is false when the
// job has neither variable. No error holds either token, the request
// URL's query, or the answer's text.
func (a *App) actionsToken(ctx context.Context, audience string) (tok string, ok bool, err error) {
	raw, hasURL := a.opts.LookupEnv(actionsTokenURL)
	bearer, hasToken := a.opts.LookupEnv(actionsRequestToken)
	raw, bearer = strings.TrimSpace(raw), strings.TrimSpace(bearer)
	if !hasURL && !hasToken || raw == "" && bearer == "" {
		return "", false, nil
	}
	if raw == "" || bearer == "" {
		return "", true, usagef("GitHub Actions set only one of %s and %s: give the job permissions: id-token: write", actionsTokenURL, actionsRequestToken)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || !(u.Scheme == "https" || u.Scheme == "http" && loopbackHost(u.Hostname())) {
		return "", true, usagef("%s is not an https:// URL, so writ sends no token to it", actionsTokenURL)
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	where := u.Scheme + "://" + u.Host + u.Path
	ctx, cancel := context.WithTimeout(ctx, actionsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", true, fmt.Errorf("ask GitHub Actions for an ID token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		// The URL's error would name the query; name the endpoint only.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "", true, fmt.Errorf("ask GitHub Actions at %s for an ID token: no answer in %s", where, actionsTimeout)
		}
		return "", true, fmt.Errorf("ask GitHub Actions at %s for an ID token: the request failed", where)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", true, fmt.Errorf("GitHub Actions at %s answered %d to the ID token request: give the job permissions: id-token: write", where, resp.StatusCode)
	}
	var doc struct {
		Value string `json:"value"`
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, actionsMaxBody+1))
	if err != nil || len(b) > actionsMaxBody || json.Unmarshal(b, &doc) != nil || strings.TrimSpace(doc.Value) == "" {
		return "", true, fmt.Errorf("GitHub Actions at %s answered the ID token request with no token", where)
	}
	return strings.TrimSpace(doc.Value), true, nil
}

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}
