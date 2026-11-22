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

// actionsEndpoint is a stand-in for GitHub Actions' ID token endpoint, as
// actions/core asks it: GET of the request URL with audience in its
// query and the request token as a bearer token, answered with
// {"value": TOKEN}.
type actionsEndpoint struct {
	*httptest.Server
	mu        sync.Mutex
	audiences []string
	status    int
	body      string
}

const (
	actionsBearer = "actions-request-token-0123456789"
	// actionsIDToken has a header of alg RS256 and kid k1, a payload, and
	// a signature writ serve would check.
	actionsIDToken = "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0.eyJzdWIiOiJyZXBvOmFjbWUvd2ViIn0.c2lnbmF0dXJl"
)

func newActionsEndpoint(t *testing.T) *actionsEndpoint {
	t.Helper()
	e := &actionsEndpoint{status: http.StatusOK, body: `{"count":1,"value":"` + actionsIDToken + `"}`}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+actionsBearer || r.URL.Query().Get("api-version") != "2.0" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		e.audiences = append(e.audiences, r.URL.Query().Get("audience"))
		w.WriteHeader(e.status)
		_, _ = w.Write([]byte(e.body))
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *actionsEndpoint) env() map[string]string {
	return map[string]string{actionsTokenURL: e.URL + "/_apis/token?api-version=2.0", actionsRequestToken: actionsBearer}
}

// TestActionsToken checks that a GitHub Actions job with no WRIT_ID_TOKEN
// asks Actions for an ID token whose audience is the control plane's URL,
// and sends that token, and that WRIT_ID_TOKEN still comes first.
func TestActionsToken(t *testing.T) {
	ctx := context.Background()
	writ := newCapture(t)
	actions := newActionsEndpoint(t)

	a := precedenceApp(t, t.TempDir(), writ.URL, actions.env())
	r, err := a.remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.token != actionsIDToken || r.tokenFrom != tokenActions {
		t.Fatalf("token from %s", r.tokenFrom)
	}
	if _, err := r.get(ctx, "/v1/whoami", remoteTimeout); err != nil {
		t.Fatal(err)
	}
	if got := writ.requests(); len(got) != 1 || got[0] != "GET /v1/whoami Bearer "+actionsIDToken {
		t.Fatalf("writ serve got %v", got)
	}
	if len(actions.audiences) != 1 || actions.audiences[0] != writ.URL {
		t.Fatalf("audiences asked for %v, want %s", actions.audiences, writ.URL)
	}

	env := actions.env()
	env[tokenCI] = "from-writ-id-token"
	r, err = precedenceApp(t, t.TempDir(), writ.URL, env).remote(ctx)
	if err != nil || r.token != "from-writ-id-token" || len(actions.audiences) != 1 {
		t.Fatalf("WRIT_ID_TOKEN did not come first: %v, %d requests to Actions", err, len(actions.audiences))
	}
}

// TestActionsTokenRefusals checks each way the request can fail. No error
// holds the request token, the ID token, the request URL's query, or the
// answer's text.
func TestActionsTokenRefusals(t *testing.T) {
	ctx := context.Background()
	writ := newCapture(t)
	for _, tc := range []struct {
		name   string
		edit   func(e *actionsEndpoint, env map[string]string)
		want   string
		noCall bool
	}{
		{"refused", func(e *actionsEndpoint, _ map[string]string) {
			e.status, e.body = http.StatusForbidden, `{"message":"no id-token permission `+actionsIDToken+`"}`
		}, "answered 403 to the ID token request: give the job permissions: id-token: write", false},
		{"no value", func(e *actionsEndpoint, _ map[string]string) { e.body = `{"count":0}` }, "with no token", false},
		{"the request token sent back", func(e *actionsEndpoint, _ map[string]string) { e.body = `{"value":"` + actionsBearer + `"}` }, "with a value that is not an ID token", false},
		{"not a compact JWT", func(e *actionsEndpoint, _ map[string]string) { e.body = `{"value":"eyJ.only-two"}` }, "with a value that is not an ID token", false},
		{"a JWT with no signature", func(e *actionsEndpoint, _ map[string]string) { e.body = `{"value":"eyJ.eyJ."}` }, "with a value that is not an ID token", false},
		{"three parts that are not a JWT", func(e *actionsEndpoint, _ map[string]string) { e.body = `{"value":"a.b.c"}` }, "with a value that is not an ID token", false},
		{"a header with no alg", func(e *actionsEndpoint, _ map[string]string) {
			e.body = `{"value":"eyJraWQiOiJrMSJ9.eyJzdWIiOiJ4In0.c2ln"}`
		}, "with a value that is not an ID token", false},
		{"not JSON", func(e *actionsEndpoint, _ map[string]string) { e.body = "<html>ACTIONS-INTERNAL</html>" }, "with no token", false},
		{"only the URL", func(_ *actionsEndpoint, env map[string]string) { delete(env, actionsRequestToken) }, "set only one of", true},
		{"plain http off loopback", func(_ *actionsEndpoint, env map[string]string) {
			env[actionsTokenURL] = "http://actions.example.com/_apis/token?api-version=2.0&secret=x"
		}, "is not an https:// URL", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newActionsEndpoint(t)
			env := e.env()
			tc.edit(e, env)
			_, err := precedenceApp(t, t.TempDir(), writ.URL, env).remote(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			for _, leak := range []string{actionsBearer, actionsIDToken, "api-version", "secret=x", "ACTIONS-INTERNAL", "no id-token permission"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the error holds %q: %v", leak, err)
				}
			}
			if tc.noCall && len(e.audiences) != 0 {
				t.Fatalf("asked Actions anyway")
			}
		})
	}
	// A job with neither variable gets the error that names both CIs.
	_, err := precedenceApp(t, t.TempDir(), writ.URL, nil).remote(ctx)
	if err == nil || !strings.Contains(err.Error(), "in GitHub Actions give the job permissions: id-token: write; in GitLab CI set id_tokens") {
		t.Fatalf("no token: %v", err)
	}
	// An endpoint that does not answer in time is a refusal, not a hang.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err = precedenceApp(t, t.TempDir(), writ.URL, map[string]string{actionsTokenURL: slow.URL + "/?api-version=2.0", actionsRequestToken: actionsBearer}).remote(ctx)
	if err == nil || strings.Contains(err.Error(), "api-version") {
		t.Fatalf("a slow endpoint: %v", err)
	}
}
