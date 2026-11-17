package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// capture is a server that records the Authorization header of every
// request it gets.
type capture struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen = append(c.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *capture) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

// redirectTo answers every request with a 307 to the same path at target.
func redirectTo(target string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
	})
}

// TestRemoteRefusesRedirects sends a session token to a control plane that
// redirects to another port of the same host. Go's client would keep the
// Authorization header there; writ must not follow the redirect at all.
func TestRemoteRefusesRedirects(t *testing.T) {
	const token = "writs_must-not-leave-the-control-plane"
	ctx := context.Background()

	t.Run("a command with a session, over HTTP to another port", func(t *testing.T) {
		evil := newCapture(t)
		front := httptest.NewServer(redirectTo(evil.URL))
		defer front.Close()
		a := precedenceApp(t, t.TempDir(), front.URL, nil)
		keep(t, a, map[string]savedSession{front.URL: {Token: token, ID: "S1", Display: "bob", EndsAt: t0.Add(time.Hour)}})
		err := a.Whoami(ctx, true)
		if err == nil || !strings.Contains(err.Error(), "redirect to "+evil.URL) {
			t.Fatalf("whoami through a redirect: %v", err)
		}
		if got := evil.requests(); len(got) != 0 {
			t.Fatalf("the redirect's target got %v", got)
		}
	})

	t.Run("writ logout, over HTTP to another port", func(t *testing.T) {
		evil := newCapture(t)
		front := httptest.NewServer(redirectTo(evil.URL))
		defer front.Close()
		a := precedenceApp(t, t.TempDir(), "", nil)
		keep(t, a, map[string]savedSession{front.URL: {Token: token, ID: "S1", Display: "bob", EndsAt: t0.Add(time.Hour)}})
		_ = a.Logout(ctx, LogoutOptions{JSON: true, URL: front.URL})
		if got := evil.requests(); len(got) != 0 {
			t.Fatalf("the redirect's target got %v", got)
		}
	})

	t.Run("HTTPS to HTTP on another port", func(t *testing.T) {
		evil := newCapture(t)
		front := httptest.NewTLSServer(redirectTo(evil.URL))
		defer front.Close()
		client := apiClient()
		client.Transport = front.Client().Transport
		r := &remote{base: front.URL, token: token, http: client}
		start := time.Now()
		_, err := r.get(ctx, "/v1/whoami", remoteTimeout)
		if err == nil || !strings.Contains(err.Error(), "redirect to "+evil.URL) {
			t.Fatalf("a GET through a redirect: %v", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("a refused redirect was retried for %s", took)
		}
		if got := evil.requests(); len(got) != 0 {
			t.Fatalf("the redirect's target got %v", got)
		}
	})
}
