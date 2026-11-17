package oidc

import "golang.org/x/oauth2"

// NewVerifier returns a new PKCE code verifier, RFC 7636, for a sign-in
// whose client keeps it until the code comes back.
func NewVerifier() string { return oauth2.GenerateVerifier() }

// Challenge is the S256 code challenge of verifier.
func Challenge(verifier string) string { return oauth2.S256ChallengeFromVerifier(verifier) }
