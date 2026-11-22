package server

import "testing"

// TestGitHubForgeOf checks which GitHub instance a GitHub Actions issuer
// binds tokens to. Any issuer that is not github.com's or a GitHub
// Enterprise Server's names no forge, so its tokens plan nothing.
func TestGitHubForgeOf(t *testing.T) {
	for _, tc := range []struct{ issuer, want string }{
		{"https://token.actions.githubusercontent.com", "https://github.com"},
		{"https://token.actions.githubusercontent.com/acme-enterprise", "https://github.com"},
		{"https://ghe.example.com/_services/token", "https://ghe.example.com"},
		{"https://ghe.example.com:8443/_services/token", "https://ghe.example.com:8443"},
		{"https://ghe.example.com", ""},
		{"https://ghe.example.com/_services/token/more", ""},
		{"http://ghe.example.com/_services/token", ""},
		{"http://token.actions.githubusercontent.com", ""},
		{"https://token.actions.githubusercontent.com.evil.example", ""},
		{"https://gitlab.com", ""},
	} {
		if got := githubForgeOf(tc.issuer); got != tc.want {
			t.Errorf("githubForgeOf(%q) = %q, want %q", tc.issuer, got, tc.want)
		}
	}
}
