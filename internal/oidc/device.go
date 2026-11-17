package oidc

import (
	"context"
	"errors"
	"net/url"
	"time"

	"golang.org/x/oauth2"
)

// The device authorization grant, RFC 8628, through golang.org/x/oauth2,
// with writ serve as the confidential client. The CLI never talks to the
// identity provider: writ serve starts the flow, keeps the device code,
// and polls the token endpoint for the CLI.

// Bounds of a device authorization response. A provider that answers
// outside them is refused or clamped.
const (
	// DefaultDeviceInterval is the polling interval when the provider
	// gives none (RFC 8628, section 3.2).
	DefaultDeviceInterval = 5 * time.Second
	minDeviceInterval     = time.Second
	maxDeviceInterval     = time.Minute
	maxDeviceLife         = 30 * time.Minute
	maxUserCode           = 64
	maxDeviceCode         = 1024
)

// DeviceAuth is a started device sign-in. Its device code is a credential:
// it stays inside, and never leaves writ serve.
type DeviceAuth struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               time.Duration
	Interval                time.Duration

	resp *oauth2.DeviceAuthResponse
}

// Device reports whether the provider offers the device flow.
func (p *Provider) Device() bool { return p.device }

// StartDevice asks the provider for a device code and a user code, with
// the scopes a sign-in in the browser asks for.
func (p *Provider) StartDevice(ctx context.Context) (*DeviceAuth, *Refusal) {
	if !p.device {
		return nil, refuse(StageDeviceAuthorization, ReasonNoDeviceFlow)
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	cctx, c := with(ctx, p.deviceClient)
	resp, err := p.oauth.DeviceAuth(cctx)
	if err != nil {
		return nil, remoteRefusal(StageDeviceAuthorization, c, err)
	}
	switch {
	case resp.DeviceCode == "" || len(resp.DeviceCode) > maxDeviceCode:
		return nil, refuse(StageDeviceAuthorization, ReasonUnusable).of(FieldDeviceCode)
	case resp.UserCode == "" || len(resp.UserCode) > maxUserCode || !printable(resp.UserCode):
		return nil, refuse(StageDeviceAuthorization, ReasonUnusable).of(FieldUserCode)
	case resp.Expiry.IsZero() || !time.Now().Before(resp.Expiry):
		return nil, refuse(StageDeviceAuthorization, ReasonUnusable).of(FieldExpiresIn)
	}
	if !p.verificationURI(resp.VerificationURI) {
		return nil, refuse(StageDeviceAuthorization, ReasonNotHTTPSOnIssuerHost).of(FieldVerificationURI)
	}
	if resp.VerificationURIComplete != "" && !p.verificationURI(resp.VerificationURIComplete) {
		resp.VerificationURIComplete = ""
	}
	life := min(time.Until(resp.Expiry), maxDeviceLife)
	interval := DefaultDeviceInterval
	if resp.Interval > 0 {
		interval = min(max(time.Duration(resp.Interval)*time.Second, minDeviceInterval), maxDeviceInterval)
	}
	// oauth2 polls at resp.Interval and stops at resp.Expiry, so the
	// bounds hold there too.
	resp.Expiry, resp.Interval = time.Now().Add(life), int64(interval/time.Second)
	return &DeviceAuth{
		UserCode: resp.UserCode, VerificationURI: resp.VerificationURI, VerificationURIComplete: resp.VerificationURIComplete,
		ExpiresIn: life, Interval: interval, resp: resp,
	}, nil
}

// verificationURI accepts an HTTPS URL on the issuer's host: the CLI
// prints it for a person to open, so it must not send them elsewhere.
func (p *Provider) verificationURI(u string) bool {
	vu, ok := httpsURL(u)
	iu, _ := url.Parse(p.URL)
	return ok && vu.Host == iu.Host && printable(u)
}

// printable reports whether s holds only printable ASCII, so a code or a
// URL that the CLI prints cannot move the terminal's cursor.
func printable(s string) bool {
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// WaitDevice polls the token endpoint through oauth2, at the interval
// and with the slow_down steps RFC 8628 sets, until the person signs in,
// refuses, or the code runs out, or ctx ends. It returns the checked ID
// token, with the groups from userinfo when the provider is set up to read
// them, or a refusal: ReasonDenied when the person refused,
// ReasonCodeExpired when the code ran out, and ReasonCanceled or
// ReasonDeadline when ctx ended. The device flow sends no nonce, so the ID
// token's is not checked.
func (p *Provider) WaitDevice(ctx context.Context, a *DeviceAuth) (*Token, *Refusal) {
	cctx, c := with(ctx, p.client)
	tok, err := p.oauth.DeviceAccessToken(cctx, a.resp)
	if err != nil {
		if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
			switch parseOAuthCode(re.ErrorCode) {
			case CodeAccessDenied:
				return nil, refuse(StageTokenEndpoint, ReasonDenied)
			case CodeExpiredToken:
				return nil, refuse(StageTokenEndpoint, ReasonCodeExpired)
			}
		}
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return nil, refuse(StageTokenEndpoint, ReasonCanceled)
		case ctx.Err() != nil:
			return nil, refuse(StageTokenEndpoint, ReasonDeadline)
		}
		// oauth2 stops at the code's expiry with its own deadline.
		if !time.Now().Before(a.resp.Expiry) {
			return nil, refuse(StageTokenEndpoint, ReasonCodeExpired)
		}
		return nil, remoteRefusal(StageTokenEndpoint, c, err)
	}
	return p.finish(ctx, tok, nil)
}
