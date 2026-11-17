package oidc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

// Key set timing. A set is refreshed every hour, or when Cache-Control
// max-age says so, but never more often than every five minutes. A token
// with an unknown kid causes at most one refresh a minute. A failed refresh
// keeps the last good set for a day.
const (
	DefaultRefresh = time.Hour
	MinRefresh     = 5 * time.Minute
	UnknownKidWait = time.Minute
	RetryAfterFail = time.Minute
	MaxKeySetAge   = 24 * time.Hour
	FetchTimeout   = 10 * time.Second
)

// keySet is one issuer's keys, cached in memory. Its mutex is held during
// a fetch, so concurrent tokens of one issuer wait for one fetch.
type keySet struct {
	mu        sync.Mutex
	issuer    string
	jwksURL   string
	client    *http.Client
	now       func() time.Time
	log       *slog.Logger
	keys      map[string]key
	goodAt    time.Time // the last successful fetch
	due       time.Time // the next scheduled fetch
	forcedAt  time.Time // the last fetch an unknown kid caused
	offset    time.Duration
	hasOffset bool
}

// discovery is what Writ reads from an issuer's discovery document.
type discovery struct {
	provider *gooidc.Provider
	JWKSURI  string   `json:"jwks_uri"`
	Device   string   `json:"device_authorization_endpoint"`
	Methods  []string `json:"code_challenge_methods_supported"`
}

// discover reads the issuer's discovery document through go-oidc, which
// requires its issuer to equal the configured URL exactly. Writ also
// requires the key set to be served over HTTPS from the issuer's host.
func discover(ctx context.Context, client *http.Client, issuer string) (discovery, *Refusal) {
	var doc discovery
	client, c := observe(client)
	ctx, cancel := context.WithTimeout(gooidc.ClientContext(ctx, client), FetchTimeout)
	defer cancel()
	p, err := gooidc.NewProvider(ctx, issuer)
	if _, ok := errors.AsType[*gooidc.IssuerMismatchError](err); ok {
		return doc, refuse(StageDiscovery, ReasonIssuerMismatch)
	}
	if err != nil {
		return doc, remoteRefusal(StageDiscovery, c, err)
	}
	if err := p.Claims(&doc); err != nil {
		return doc, refuse(StageDiscovery, ReasonNotJSON)
	}
	doc.provider = p
	iu, _ := url.Parse(issuer)
	if ju, ok := httpsURL(doc.JWKSURI); !ok || ju.Host != iu.Host {
		return doc, refuse(StageDiscovery, ReasonNotHTTPSOnIssuerHost).of(FieldJWKSURI)
	}
	return doc, nil
}

// fetch GETs url, the stage's endpoint, with the fetch timeout and a body
// limit. It follows no redirect: the client the caller builds refuses
// them.
func fetch(ctx context.Context, client *http.Client, stage Stage, u string, limit int64) ([]byte, http.Header, *Refusal) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	client, c := observe(client)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, remoteRefusal(stage, c, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, remoteRefusal(stage, c, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r := refuse(stage, ReasonStatus)
		r.status = resp.StatusCode
		return nil, nil, r
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, remoteRefusal(stage, c, err)
	}
	if int64(len(body)) > limit {
		return nil, nil, refuse(stage, ReasonTooLarge).counted(0, limit)
	}
	return body, resp.Header, nil
}

// refresh fetches the key set. The caller holds ks.mu. On failure the old
// keys stay, and the next scheduled try is a minute away.
func (ks *keySet) refresh(ctx context.Context) *Refusal {
	now := ks.now()
	body, header, ref := fetch(ctx, ks.client, StageKeySet, ks.jwksURL, MaxKeySetBytes)
	var keys map[string]key
	if ref == nil {
		keys, ref = parseKeySet(body)
	}
	if ref != nil {
		ks.due = now.Add(RetryAfterFail)
		ks.log.Warn("key set refresh failed", "issuer", ks.issuer, "stage", ref.Stage().String(), "reason", ref.Message())
		return ref
	}
	ks.keys, ks.goodAt = keys, now
	ks.due = now.Add(refreshAfter(header.Get("Cache-Control")))
	if d, err := http.ParseTime(header.Get("Date")); err == nil {
		ks.offset, ks.hasOffset = d.Sub(now), true
	}
	return nil
}

// refreshAfter reads max-age from a Cache-Control header and bounds it.
func refreshAfter(cc string) time.Duration {
	for part := range strings.SplitSeq(cc, ",") {
		name, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(name, "max-age") {
			continue
		}
		sec, err := strconv.ParseInt(strings.Trim(val, `"`), 10, 64)
		if err != nil || sec < 0 {
			break
		}
		return min(max(time.Duration(sec)*time.Second, MinRefresh), DefaultRefresh)
	}
	return DefaultRefresh
}

// key returns the key with kid, refreshing the set when it is due, or once
// a minute when kid is unknown.
func (ks *keySet) key(ctx context.Context, kid string) (key, *Refusal) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	now := ks.now()
	if !now.Before(ks.due) {
		_ = ks.refresh(ctx)
	}
	if ks.goodAt.IsZero() || now.Sub(ks.goodAt) > MaxKeySetAge {
		return key{}, failed(CheckSignature, ReasonNoGoodKeySet)
	}
	if k, ok := ks.keys[kid]; ok {
		return k, nil
	}
	if ks.forcedAt.IsZero() || now.Sub(ks.forcedAt) >= UnknownKidWait {
		ks.forcedAt = now
		if ks.refresh(ctx) == nil {
			if k, ok := ks.keys[kid]; ok {
				return k, nil
			}
		}
	}
	return key{}, failed(CheckSignature, ReasonUnknownKid)
}

// status reports the last good fetch and the clock offset its Date header
// showed, for /healthz.
func (ks *keySet) status() (goodAt time.Time, offset time.Duration, ok bool) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.goodAt, ks.offset, ks.hasOffset
}
