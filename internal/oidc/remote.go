package oidc

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"

	"golang.org/x/oauth2"
)

// An identity provider's answer can repeat what the request carried: the
// access token at userinfo, the client secret, a code, or a code verifier
// at the token endpoint. A library's error can hold that answer: go-oidc
// puts the body of a refusal in its message, and oauth2 the body or the
// error_description. So no library error leaves a call to an issuer:
// remoteRefusal reads only what the call saw on the wire and the error's
// type, and keeps the HTTP status and a known RFC 6749 error code.

// call records what one call to an issuer's endpoint saw on the wire:
// the last answer's status, or the transport's failure.
type call struct {
	mu        sync.Mutex
	status    int
	transport error
	tooLarge  bool
}

// observe returns a copy of client whose requests c records.
func observe(client *http.Client) (*http.Client, *call) {
	c := &call{}
	out := *client
	base := out.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out.Transport = recorder{base: base, call: c}
	return &out, c
}

type recorder struct {
	base http.RoundTripper
	call *call
}

func (r recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	r.call.mu.Lock()
	defer r.call.mu.Unlock()
	if err != nil {
		r.call.status, r.call.transport = 0, err
		return nil, err
	}
	r.call.status, r.call.transport = resp.StatusCode, nil
	resp.Body = recordedBody{ReadCloser: resp.Body, call: r.call}
	return resp, nil
}

type recordedBody struct {
	io.ReadCloser
	call *call
}

func (b recordedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, errTooLarge) {
		b.call.mu.Lock()
		b.call.tooLarge = true
		b.call.mu.Unlock()
	}
	return n, err
}

// remoteRefusal says why a call to the stage's endpoint failed, from what
// c saw and err's type. err's text is never read: it can hold the answer.
func remoteRefusal(stage Stage, c *call, err error) *Refusal {
	c.mu.Lock()
	status, transport, tooLarge := c.status, c.transport, c.tooLarge
	c.mu.Unlock()
	re, isRetrieve := errors.AsType[*oauth2.RetrieveError](err)
	r := refuse(stage, 0)
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(transport, context.Canceled):
		r.reason = ReasonCanceled
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(transport, context.DeadlineExceeded):
		r.reason = ReasonDeadline
	case isRetrieve && re.Response != nil:
		r.reason, r.status, r.code = ReasonOAuthError, re.Response.StatusCode, parseOAuthCode(re.ErrorCode)
	case transport != nil:
		r.reason = transportFailure(transport)
	case status >= 300 && status < 400:
		r.reason, r.status = ReasonRedirect, status
	case status != 0 && (status < 200 || status > 299):
		r.reason, r.status = ReasonStatus, status
	case tooLarge:
		r.reason, r.limit = ReasonTooLarge, maxAnswer
	case status != 0:
		r.reason, r.status = ReasonUnreadable, status
	default:
		r.reason = ReasonNotSent
	}
	return r
}

// transportFailure names the kind of a failed round trip. Its text is
// not read: a malformed answer's status line, for one, is in it.
func transportFailure(err error) Reason {
	var dns *net.DNSError
	var op *net.OpError
	var cert *tls.CertificateVerificationError
	var ne net.Error
	switch {
	case errors.As(err, &dns):
		return ReasonUnresolved
	case errors.As(err, &cert):
		return ReasonUntrustedTLS
	case errors.As(err, &ne) && ne.Timeout():
		return ReasonTransportTimeout
	case errors.As(err, &op) && op.Op == "dial":
		return ReasonConnectFailed
	}
	return ReasonTransportFailed
}
