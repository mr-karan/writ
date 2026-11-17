package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/oidc"
)

// Timeouts of the HTTP server. WriteTimeout covers the longest ?wait=.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	writeTimeout      = MaxWait + 30*time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
	pruneEvery        = time.Hour
)

func osHostname() (string, error) {
	h, err := os.Hostname()
	return fmt.Sprintf("%s pid %d", h, os.Getpid()), err
}

// Issuers builds the verifier's issuers, the ones of CI jobs, with a
// client that trusts each issuer's ca_file as well as the system roots.
func Issuers(cfg *Config) ([]oidc.Issuer, error) {
	var out []oidc.Issuer
	for _, is := range cfg.Issuers {
		if is.Kind == oidc.People {
			continue
		}
		client, err := issuerClient(is)
		if err != nil {
			return nil, err
		}
		out = append(out, oidc.Issuer{Name: is.Name, Kind: is.Kind, URL: is.URL, Audience: cfg.URL, Client: client})
	}
	return out, nil
}

// NewVerifier fetches the discovery document and key set of each issuer of
// CI jobs and returns the verifier that checks their tokens. It fails at
// the first issuer that cannot be trusted, with the issuer's name and the
// refusal's message.
func NewVerifier(ctx context.Context, cfg *Config, log *slog.Logger, now func() time.Time) (*oidc.Verifier, error) {
	issuers, err := Issuers(cfg)
	if err != nil {
		return nil, err
	}
	v := oidc.New(oidc.Options{Log: log, Now: now})
	for _, is := range issuers {
		if ref := v.Add(ctx, is); ref != nil {
			return nil, errors.New("issuer " + is.Name + ": " + ref.Message())
		}
	}
	return v, nil
}

func issuerClient(is Issuer) (*http.Client, error) {
	client := &http.Client{Timeout: oidc.FetchTimeout}
	if is.CAFile == "" {
		return client, nil
	}
	pem, err := os.ReadFile(is.CAFile)
	if err != nil {
		return nil, fmt.Errorf("issuer %s: %w", is.Name, err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("issuer %s: ca_file %s holds no PEM certificate", is.Name, is.CAFile)
	}
	client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	return client, nil
}

// People reads each people issuer's client secret from its file and
// fetches the issuer's discovery document and key set. It logs a warning
// for an issuer whose discovery document does not list S256; the
// challenge is sent anyway.
func People(ctx context.Context, cfg *Config, log *slog.Logger, now func() time.Time) ([]*oidc.Provider, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var out []*oidc.Provider
	for _, is := range cfg.Issuers {
		if is.Kind != oidc.People {
			continue
		}
		client, err := issuerClient(is)
		if err != nil {
			return nil, err
		}
		secret, err := os.ReadFile(is.ClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("issuer %s: read client_secret_file: %w", is.Name, err)
		}
		p, ref := oidc.NewProvider(ctx, oidc.ProviderOptions{
			Name: is.Name, URL: is.URL, ClientID: is.ClientID, ClientSecret: strings.TrimSpace(string(secret)),
			GroupsFromUserinfo: is.GroupsFromUserinfo, Client: client, Now: now, Log: log,
		})
		if ref != nil {
			return nil, errors.New("issuer " + is.Name + ": " + ref.Message())
		}
		if !p.S256 {
			log.Warn("the issuer's discovery document does not list S256 in code_challenge_methods_supported; sign-in sends the challenge anyway", "issuer", is.Name)
		}
		out = append(out, p)
	}
	return out, nil
}

// Serve listens on the config's address and serves until ctx ends, then
// stops taking requests, waits up to 30 seconds for the ones running, and
// stops the running plans. It prunes the control database every hour.
func (s *Server) Serve(ctx context.Context, ready func(addr string)) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	return s.ServeOn(ctx, ln, ready)
}

// ServeOn is Serve on a listener the caller opened, as writ serve --dev
// opens one to learn its port before it writes the config's URL.
func (s *Server) ServeOn(ctx context.Context, ln net.Listener, ready func(addr string)) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	if s.cfg.TLS != nil {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLS.Cert, s.cfg.TLS.Key)
		if err != nil {
			ln.Close()
			return fmt.Errorf("tls: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	s.log.Info("writ serve listening", "addr", ln.Addr().String(), "url", s.cfg.URL, "version", s.version, "tls", s.cfg.TLS != nil)
	if ready != nil {
		ready(ln.Addr().String())
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()
	s.prune(ctx)
	for {
		select {
		case err := <-errc:
			s.Close()
			return err
		case <-prune.C:
			s.prune(ctx)
		case <-ctx.Done():
			s.log.Info("writ serve stopping")
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := srv.Shutdown(sctx)
			cancel()
			s.Close()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			return err
		}
	}
}

func (s *Server) prune(ctx context.Context) {
	got, err := s.store.Prune(ctx, s.now(), s.cfg.Retention)
	if err != nil {
		s.log.Error("prune the control database", "err", err)
		return
	}
	if got != (control.Pruned{}) {
		s.log.Info("pruned the control database", "expired_plans", got.Expired, "deleted_plans", got.Plans, "audit_rows", got.Audit, "token_ids", got.Tokens,
			"role_sessions", got.RoleSessions)
	}
}
