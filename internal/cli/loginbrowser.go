package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/server"
)

// writ login in a browser: the authorization code flow with PKCE and a
// loopback redirect (RFC 8252, RFC 7636), for an identity provider with no
// device flow. The CLI listens on 127.0.0.1, makes the state and the PKCE
// code verifier, and asks writ serve for the identity provider's URL. The
// browser comes back to the loopback listener with the code, which serves
// that one request and closes. writ serve trades the code with its client
// secret, so the CLI never holds the secret or a token of the identity
// provider.

// browserFallback is how long writ login waits for the browser when writ
// serve names no end for the sign-in.
const browserFallback = 10 * time.Minute

// browserLogin runs the sign-in in a browser and returns the session.
func (a *App) browserLogin(ctx context.Context, r *remote, o LoginOptions, prompt out) (*server.SessionDoc, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on 127.0.0.1 for the identity provider's redirect: %w", err)
	}
	defer ln.Close()
	host := ln.Addr().String()
	verifier, state := oidc.NewVerifier(), rand.Text()
	body, _ := json.Marshal(server.BrowserRequest{
		Issuer: o.Issuer, RedirectURI: "http://" + host + server.CallbackPath, State: state, CodeChallenge: oidc.Challenge(verifier),
	})
	status, resp, err := r.call(ctx, http.MethodPost, "/v1/auth/browser", body, remoteTimeout)
	if err != nil {
		return nil, err
	}
	var b server.Browser
	bad := json.Unmarshal(resp, &b) != nil || b.Format != server.FormatBrowser
	switch {
	case status == http.StatusNotFound && bad:
		return nil, fmt.Errorf("%s has no POST /v1/auth/browser, so its writ serve is too old for writ login in a browser: upgrade it, or sign in with writ login --device", r.base)
	case status != http.StatusOK:
		return nil, a.remoteFailed(o.JSON, resp)
	case bad || b.Handle == "":
		return nil, fmt.Errorf("%s answered with a body that is not %s", r.base, server.FormatBrowser)
	}
	authURL, err := url.Parse(b.AuthURL)
	if err != nil || authURL.Scheme != "https" || authURL.Host == "" {
		return nil, fmt.Errorf("%s sent a sign-in URL that is not https://, and writ opens no other", r.base)
	}
	deadline, err := time.Parse(time.RFC3339, b.ExpiresAt)
	if err != nil {
		deadline = a.opts.Now().Add(browserFallback)
	}
	fmt.Fprintf(prompt, "Signing in to %s through issuer %s.\n", r.base, b.Issuer)
	fmt.Fprintf(prompt, "To sign in, open %s in a browser on this computer.\n", authURL)
	fmt.Fprintf(prompt, "The sign-in expires in %s.\n", minutes(deadline.Sub(a.opts.Now())))
	a.openBrowser(o.JSON, authURL.String())

	code, err := waitCallback(ctx, ln, host, state, deadline.Sub(a.opts.Now()))
	if err != nil {
		return nil, err
	}
	body, _ = json.Marshal(server.BrowserTokenRequest{Handle: b.Handle, Code: code, State: state, CodeVerifier: verifier})
	status, resp, err = r.once(ctx, http.MethodPost, "/v1/auth/browser/token", body, remoteTimeout, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, a.remoteFailed(o.JSON, resp)
	}
	return signedIn(r, resp)
}

// waitCallback serves the identity provider's redirect on ln, whose
// address is host, and returns its code. It serves one request, whatever
// it holds, then closes the listener, and it gives up after wait.
func waitCallback(ctx context.Context, ln net.Listener, host, state string, wait time.Duration) (string, error) {
	type result struct {
		code string
		err  error
	}
	got := make(chan result, 1)
	var once sync.Once
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		first := false
		once.Do(func() {
			first = true
			ln.Close()
		})
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !first {
			w.WriteHeader(http.StatusGone)
			io.WriteString(w, "This sign-in already ended. Return to the terminal.\n")
			return
		}
		code, err := checkCallback(req, host, state)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "The sign-in failed: %s. Return to the terminal.\n", err)
		} else {
			io.WriteString(w, "Writ has the sign-in. Return to the terminal.\n")
		}
		got <- result{code, err}
	})}
	srv.SetKeepAlivesEnabled(false)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(ln)
	}()
	defer func() {
		// Shutdown lets the one answer reach the browser before the
		// connection closes.
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		<-served
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case res := <-got:
		return res.code, res.err
	case <-timer.C:
		return "", errors.New("the browser did not come back with the sign-in in time: run writ login again")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// checkCallback reads the identity provider's redirect to the loopback
// listener at host. It refuses one from off the machine, one whose Host is
// not the listener's, as a DNS rebinding page would send, a wrong path or
// state, and the identity provider's error.
func checkCallback(req *http.Request, host, state string) (string, error) {
	addr, _, err := net.SplitHostPort(req.RemoteAddr)
	ip := net.ParseIP(addr)
	switch {
	case err != nil || ip == nil || !ip.IsLoopback():
		return "", fmt.Errorf("the callback came from %s, not from this computer", req.RemoteAddr)
	case req.Host != host:
		return "", fmt.Errorf("the callback named host %s, not %s", strconv.Quote(req.Host), host)
	case req.Method != http.MethodGet || req.URL.Path != server.CallbackPath:
		return "", fmt.Errorf("the callback was %s %s, not GET %s", req.Method, strconv.Quote(req.URL.Path), server.CallbackPath)
	}
	q := req.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		return "", errors.New("the callback's state is not the one this sign-in made, so another page may have sent it")
	}
	if e := q.Get("error"); e != "" {
		return "", errors.New("the identity provider refused the sign-in: " + server.CallbackRefusal(e))
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("the callback carried no authorization code")
	}
	return code, nil
}

// openBrowser opens url in the person's browser, as best it can, when a
// person reads the prompts at a terminal on a desktop. The printed URL is
// enough when it cannot.
func (a *App) openBrowser(asJSON bool, url string) {
	w := a.opts.Stdout
	if asJSON {
		w = a.opts.Stderr
	}
	if !isTerminal(w) {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		x, _ := a.opts.LookupEnv("DISPLAY")
		wl, _ := a.opts.LookupEnv("WAYLAND_DISPLAY")
		if x == "" && wl == "" {
			return
		}
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		a.opts.Log.Debug("open a browser", "err", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}
