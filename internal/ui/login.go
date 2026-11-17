package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// loginTTL is how long a started sign-in may take at the identity provider.
const loginTTL = 10 * time.Minute

// loginState is what the sign-in cookie holds between the start and the
// callback. The cookie is HttpOnly and lives ten minutes.
type loginState struct {
	Issuer   string `json:"issuer"`
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
}

func random32() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type loginData struct {
	Issuers []server.IssuerInfo
	// Dev is set on the dev control plane, whose user signs in with a link
	// writ ui prints.
	Dev     bool
	Next    string
	Message string
}

// login lists the people issuers, each a link that starts a sign-in.
func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	data := loginData{Issuers: u.srv.PeopleIssuers(), Dev: u.srv.Dev(), Next: safeNext(r.URL.Query().Get("next"))}
	if r.URL.Query().Get("signed_out") == "1" {
		data.Message = "You are signed out of writ serve. Your session at the identity provider is unchanged."
	}
	u.render(w, r, http.StatusOK, "login", &page{Title: "Sign in", Data: data})
}

// loginStart makes the state and the nonce, gets the PKCE verifier from
// the server, keeps them in the sign-in cookie, and sends the browser to
// the identity provider.
func (u *UI) loginStart(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	st := loginState{Issuer: q.Get("issuer"), State: random32(), Nonce: random32(), Next: safeNext(q.Get("next"))}
	to, verifier, err := u.srv.AuthURL(st.Issuer, u.redirectURI, st.State, st.Nonce)
	st.Verifier = verifier
	if err != nil {
		u.errorPage(w, r, nil, http.StatusBadRequest, "bad request", []server.Diagnostic{{Severity: "error", Class: "usage", Message: err.Error()}}, nil)
		return
	}
	b, _ := json.Marshal(st)
	u.setCookie(w, loginCookie, base64.RawURLEncoding.EncodeToString(b), int(loginTTL/time.Second))
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// callback finishes a sign-in: it checks state against the cookie in
// constant time, deletes the cookie so a callback works once, and asks the
// server to trade the code for a session.
func (u *UI) callback(w http.ResponseWriter, r *http.Request) {
	u.setCookie(w, loginCookie, "", -1)
	fail := func(msg string) {
		u.render(w, r, http.StatusUnauthorized, "login", &page{Title: "Sign in", Data: loginData{Issuers: u.srv.PeopleIssuers(), Next: "/ui/", Message: msg}})
	}
	st, err := u.readLogin(r)
	if err != nil {
		fail("The sign-in did not start in this browser, or it took longer than 10 minutes. Sign in again.")
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		u.log.Warn("sign-in callback with a wrong state", "remote", r.RemoteAddr)
		fail("The sign-in's state does not match the one this browser started. Sign in again.")
		return
	}
	if e := q.Get("error"); e != "" {
		fail("The identity provider refused the sign-in: " + server.CallbackRefusal(e) + ".")
		return
	}
	token, sess, err := u.srv.SignIn(r.Context(), r, st.Issuer, q.Get("code"), st.Verifier, u.redirectURI, st.Nonce)
	if err != nil {
		fail("The sign-in failed: " + err.Error())
		return
	}
	u.setCookie(w, sessionCookie, token, int(time.Until(sess.EndsAt)/time.Second))
	// The browser holds the sign-in cookie, so its next is checked again.
	http.Redirect(w, r, safeNext(st.Next), http.StatusSeeOther)
}

func (u *UI) readLogin(r *http.Request) (loginState, error) {
	var st loginState
	c, err := r.Cookie(u.cookieName(loginCookie))
	if err != nil {
		return st, err
	}
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.State == "" || st.Nonce == "" || st.Verifier == "" {
		return st, errors.New("the sign-in cookie is incomplete")
	}
	return st, nil
}

// logout ends the session on the server and clears the cookie. It does not
// sign the person out of the identity provider.
func (u *UI) logout(w http.ResponseWriter, r *http.Request, s *server.Session) {
	if c, err := r.Cookie(u.cookieName(sessionCookie)); err == nil {
		if err := u.srv.SignOut(r.Context(), r, c.Value, s); err != nil && !errors.Is(err, server.ErrNoSession) {
			// The session lives on, so the cookie stays and the person is
			// not told they signed out.
			u.log.Error("end a session", "err", err)
			u.errorPage(w, r, s, http.StatusServiceUnavailable, "unavailable", []server.Diagnostic{{Severity: "error", Class: "state-io", Message: "writ serve could not end the session: " + err.Error()}}, nil)
			return
		}
	}
	u.setCookie(w, sessionCookie, "", -1)
	http.Redirect(w, r, "/ui/login?signed_out=1&next="+url.QueryEscape("/ui/"), http.StatusSeeOther)
}
