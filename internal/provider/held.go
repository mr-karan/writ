package provider

import (
	"fmt"

	"google.golang.org/grpc/status"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

// hold records that this process was sent, or answered with, a value that
// holds a secret: a known value marked secret or under a sensitive
// attribute, a key of a secret map, or an unknown value under a sensitive
// attribute, which the provider fills in itself. A provider process keeps
// what it is sent for the whole run, so from then on its text may quote the
// secret in a call about any object. Each Diag and ProcessError the client
// makes records whether the process held a secret then.
func (c *Client) hold(b schema.Block, vals ...value.Value) {
	if c.held.Load() {
		return
	}
	for _, v := range vals {
		if holds(&b, v, false) {
			c.held.Store(true)
			return
		}
	}
}

// ProcessError is a provider call or start that failed without
// diagnostics: the gRPC status, and the tail of the provider's stderr,
// where a provider that crashes explains why. Both are the provider's own
// text.
type ProcessError struct {
	// What is Writ's text: the provider, the call, and the resource type.
	// It is empty for a provider that failed to start.
	What string
	// Err is the call's error. A gRPC status's message is the provider's
	// text; any other error is Writ's.
	Err error
	// Stderr is the tail of what the provider wrote to stderr.
	Stderr string
	// Exited says the provider process had exited when the call failed.
	Exited bool
	// Held says the process held a secret when the call failed.
	Held bool
}

func (e *ProcessError) Error() string {
	s := e.Err.Error()
	if e.What != "" {
		s = e.What + ": " + s
	}
	if e.Stderr != "" {
		s += "\nprovider stderr:\n" + e.Stderr
	}
	return s
}

func (e *ProcessError) Unwrap() error { return e.Err }

// withheld is Error without the provider's text: the gRPC code stays, and
// so do the facts that the provider wrote to stderr and that it exited.
func (e *ProcessError) withheld() string {
	s := e.Err.Error()
	if st, ok := status.FromError(e.Err); ok {
		s = fmt.Sprintf("rpc error: code = %s, and Writ withholds the provider's message", st.Code())
	}
	if e.What != "" {
		s = e.What + ": " + s
	}
	if e.Exited {
		s += "; the provider exited"
	}
	if e.Stderr != "" {
		s += "; the provider wrote to stderr, and Writ withholds it"
	}
	return s
}
