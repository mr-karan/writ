package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// TestSignInDropsEchoedCredentials makes each identity provider endpoint a
// sign-in calls answer with an error that repeats the credentials it
// received: the client secret, the code and its verifier, the device code,
// or the access token. Neither writ login nor the control UI's sign-in may
// carry any of them, or any of the answer's body, into the answer, the
// log, or the control database.
func TestSignInDropsEchoedCredentials(t *testing.T) {
	type signIn func(t *testing.T, pf *peopleFixture) string
	cli := func(t *testing.T, pf *peopleFixture) string {
		r := pf.do("POST", "/v1/auth/device", "", jsonBody(`{"issuer":""}`))
		if r.status != http.StatusOK {
			return string(r.body)
		}
		pf.is.ApproveDevice(r.doc["user_code"].(string))
		r = pf.settle(r.doc["handle"].(string))
		if r.status == http.StatusOK {
			t.Fatalf("the sign-in succeeded: %s", r.body)
		}
		return string(r.body)
	}
	ui := func(t *testing.T, pf *peopleFixture) string {
		const redirect = "https://writ.example.com/ui/callback"
		authURL, verifier, err := pf.srv.AuthURL("sso", redirect, "state-1", "nonce-1")
		if err != nil {
			t.Fatal(err)
		}
		c := pf.is.Client()
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		res, err := c.Get(authURL)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		back, err := url.Parse(res.Header.Get("Location"))
		if err != nil || back.Query().Get("code") == "" {
			t.Fatalf("the issuer sent back %q", res.Header.Get("Location"))
		}
		// The control UI shows this error's text on its sign-in page.
		pf.srv.MountUI([]server.UIRoute{{Pattern: "GET /ui/callback", Handler: func(w http.ResponseWriter, r *http.Request) {
			_, _, err = pf.srv.SignIn(r.Context(), r, "sso", back.Query().Get("code"), verifier, redirect, "nonce-1")
		}}}, func(http.ResponseWriter, *http.Request, int, server.Diagnostic) {})
		pf.srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", redirect, nil))
		if err == nil {
			t.Fatal("the sign-in succeeded")
		}
		return err.Error()
	}
	for _, path := range []struct {
		name string
		run  signIn
	}{{"writ login", cli}, {"control UI", ui}} {
		for _, tc := range []struct {
			endpoint oidctest.Endpoint
			// want is the diagnostic the answer keeps.
			want string
			// creds is false where the request carries no credential.
			creds bool
		}{
			{oidctest.DeviceAuthorization, "device authorization: status 400, invalid_request", true},
			{oidctest.TokenEndpoint, "token endpoint: status 400, invalid_request", true},
			{oidctest.Userinfo, "userinfo: status 401", true},
			{oidctest.KeySet, "the issuer's key set has no key with the token's kid", false},
		} {
			if tc.endpoint == oidctest.DeviceAuthorization && path.name == "control UI" {
				continue
			}
			t.Run(path.name+"/"+string(tc.endpoint), func(t *testing.T) {
				pf := newPeopleFixture(t)
				if tc.endpoint == oidctest.KeySet {
					// The ID token names a key writ serve has not fetched,
					// so the sign-in fetches the key set.
					pf.is.SetKeys(oidctest.NewRSAKey(t, "rotated", 2048))
				}
				pf.is.SetEcho(tc.endpoint)
				answer := path.run(t, pf)
				echoed := pf.is.Echoed()
				if tc.creds && len(echoed) == 0 {
					t.Fatalf("the %s received no credential to echo", tc.endpoint)
				}
				sinks := map[string]string{"answer": answer, "log": pf.log.String(), "database": pf.dump()}
				for sink, text := range sinks {
					for _, secret := range append(echoed, oidctest.EchoMarker, clientSecret) {
						if strings.Contains(text, secret) {
							t.Errorf("the %s holds %q from the %s's answer:\n%s", sink, secret, tc.endpoint, text)
						}
					}
				}
				if !strings.Contains(answer, tc.want) {
					t.Errorf("the answer does not say %q: %s", tc.want, answer)
				}
			})
		}
	}
}
