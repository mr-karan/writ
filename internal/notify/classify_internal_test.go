package notify

import (
	"errors"
	"net"
	"net/url"
	"testing"
	"time"
)

// A name that does not resolve is permanent and says dns; a lookup that
// timed out or failed for now is tried again. Go wraps the lookup's error
// in the dial's and the request's, as a delivery meets it.
func TestClassifyDNS(t *testing.T) {
	wrap := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://hooks.example.com/x?token=secret", Err: &net.OpError{Op: "dial", Net: "tcp", Err: err}}
	}
	for _, c := range []struct {
		err       error
		retryable bool
		msg       string
	}{
		{&net.DNSError{Err: "no such host", Name: "hooks.example.com", IsNotFound: true}, false, "dns: no such host"},
		{&net.DNSError{Err: "i/o timeout", Name: "hooks.example.com", IsTimeout: true}, true, "dns: lookup timed out"},
		{&net.DNSError{Err: "server misbehaving", Name: "hooks.example.com", IsTemporary: true}, true, "dns: lookup failed"},
		{&net.DNSError{Err: "bad name", Name: "hooks.example.com"}, false, "dns: lookup failed"},
		{errors.New("connection refused"), true, "connection failed"},
	} {
		retryable, msg := classifyClientErr(wrap(c.err))
		if retryable != c.retryable || msg != c.msg {
			t.Errorf("%v: %v %q, want %v %q", c.err, retryable, msg, c.retryable, c.msg)
		}
	}
}

// Retry-After asks for a wait in seconds or until an HTTP date, never
// longer than maxRetryAfter, and a value that can't be read asks for none.
func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		v    string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"7", 7 * time.Second},
		{"86400", maxRetryAfter},
		{"99999999999999999999", 0},
		{now.Add(30 * time.Second).Format(http1123), 30 * time.Second},
		{now.Add(time.Hour).Format(http1123), maxRetryAfter},
		{now.Add(-time.Minute).Format(http1123), 0},
		{"soon", 0},
	} {
		if got := retryAfter(c.v, now); got != c.want {
			t.Errorf("Retry-After %q: %v, want %v", c.v, got, c.want)
		}
	}
}

const http1123 = "Mon, 02 Jan 2006 15:04:05 GMT"
