package oidc_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
)

// TestStartDropsEchoedAnswers makes the discovery document and the key
// set, which writ serve fetches as it starts, answer with an error body.
// The error and the log keep the endpoint and the status, never the body.
func TestStartDropsEchoedAnswers(t *testing.T) {
	for _, tc := range []struct {
		endpoint oidctest.Endpoint
		want     string
	}{
		{oidctest.Discovery, "discovery: status 400"},
		{oidctest.KeySet, "key set: status 400"},
	} {
		starts := map[string]func(is *oidctest.Issuer, log *slog.Logger) *oidc.Refusal{
			"people": func(is *oidctest.Issuer, log *slog.Logger) *oidc.Refusal {
				_, ref := oidc.NewProvider(context.Background(), oidc.ProviderOptions{
					Name: "sso", URL: is.URL, ClientID: "writ-client", ClientSecret: "secret", Client: is.Client(), Log: log,
				})
				return ref
			},
			"ci": func(is *oidctest.Issuer, log *slog.Logger) *oidc.Refusal {
				return oidc.New(oidc.Options{Log: log}).Add(context.Background(),
					oidc.Issuer{Name: "gitlab", Kind: oidc.GitLabCI, URL: is.URL, Audience: "https://writ.example.com", Client: is.Client()})
			},
		}
		for kind, start := range starts {
			t.Run(kind+"/"+string(tc.endpoint), func(t *testing.T) {
				is := oidctest.New(t)
				is.SetEcho(tc.endpoint)
				var log bytes.Buffer
				err := start(is, slog.New(slog.NewTextHandler(&log, nil)))
				if err == nil {
					t.Fatal("the issuer started")
				}
				for sink, text := range map[string]string{"error": err.Message(), "log": log.String()} {
					if strings.Contains(text, oidctest.EchoMarker) {
						t.Errorf("the %s holds the %s's answer: %s", sink, tc.endpoint, text)
					}
				}
				if !strings.Contains(err.Message(), tc.want) {
					t.Errorf("the error does not say %q: %v", tc.want, err)
				}
			})
		}
	}
}
