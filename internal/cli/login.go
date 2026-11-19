package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/server"
)

// writ login and writ logout. writ serve brokers the sign-in with the
// team's identity provider, through the device flow or, in a browser, the
// authorization code flow, so the CLI never holds the client secret or a
// token of the identity provider. It keeps only the session writ serve
// issues: in SessionsFile, readable by its user alone, under the control
// plane's URL.

// SessionsFile holds the sessions of writ login, in the user's writ config
// directory.
const SessionsFile = "sessions.json"

// sessionsFormat is the format of SessionsFile.
const sessionsFormat = "writ.sessions-file/v0"

// maxSessionsFile bounds the session file writ reads.
const maxSessionsFile = 1 << 20

// savedSession is one session in SessionsFile.
type savedSession struct {
	Token   string    `json:"token"`
	ID      string    `json:"id"`
	Person  string    `json:"person"`
	Display string    `json:"display"`
	Issuer  string    `json:"issuer"`
	Teams   []string  `json:"teams"`
	EndsAt  time.Time `json:"ends_at"`
}

type sessionsDoc struct {
	Format string `json:"format"`
	// Sessions are by the control plane's URL.
	Sessions map[string]savedSession `json:"sessions"`
}

// configDir is writ's config directory: $XDG_CONFIG_HOME/writ, else
// ~/.config/writ.
func (a *App) configDir() (string, error) {
	base, _ := a.opts.LookupEnv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the home directory for %s: %w", SessionsFile, err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "writ"), nil
}

func (a *App) sessionsPath() (string, error) {
	dir, err := a.configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, SessionsFile), nil
}

// readSessions reads SessionsFile. A missing file holds no session. A
// session acts as its person, so the file must be a regular file, not a
// symbolic link, that this user owns and only this user may read; any
// other is refused, since another user may have a copy or may have put it
// there.
func (a *App) readSessions() (string, sessionsDoc, error) {
	doc := sessionsDoc{Format: sessionsFormat, Sessions: map[string]savedSession{}}
	path, err := a.sessionsPath()
	if err != nil {
		return "", doc, err
	}
	const again = "delete it and run writ login again, since another user may hold a copy of its sessions or may have put it there"
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return path, doc, usagef("%s is a symbolic link, and writ reads sessions only from a regular file it wrote: %s", path, again)
	}
	f, err := os.OpenFile(path, openSessionsFlags, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return path, doc, nil
	}
	if err != nil {
		return path, doc, fmt.Errorf("read the sessions of writ login: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return path, doc, err
	}
	switch {
	case !info.Mode().IsRegular():
		return path, doc, usagef("%s is not a regular file (%s): %s", path, info.Mode().Type(), again)
	case !ownedByUser(info):
		return path, doc, usagef("%s belongs to another user: %s", path, again)
	case info.Mode().Perm()&0o077 != 0:
		return path, doc, usagef("%s holds sessions and others may read it (mode %04o): run chmod 600 %s, then writ login again, since another user may hold a copy", path, info.Mode().Perm(), path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSessionsFile+1))
	if err != nil {
		return path, doc, err
	}
	if len(b) > maxSessionsFile || json.Unmarshal(b, &doc) != nil || doc.Format != sessionsFormat {
		return path, doc, usagef("%s is not a %s document; delete it and run writ login again", path, sessionsFormat)
	}
	if doc.Sessions == nil {
		doc.Sessions = map[string]savedSession{}
	}
	return path, doc, nil
}

// writeSessions replaces SessionsFile, mode 0600, through a new file
// renamed over the old, and drops sessions past their end. The directory
// must belong to this user, and its mode is set to 0700 when it is looser:
// writ made it, or the user did for writ, and nothing else belongs in it.
func (a *App) writeSessions(path string, doc sessionsDoc) error {
	now := a.opts.Now()
	maps.DeleteFunc(doc.Sessions, func(_ string, s savedSession) bool { return !now.Before(s.EndsAt) })
	doc.Format = sessionsFormat
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	switch {
	case err != nil:
		return err
	case !info.IsDir():
		return usagef("%s is not a directory", dir)
	case !ownedByUser(info):
		return usagef("%s belongs to another user, and writ keeps sessions only in a directory of yours", dir)
	case info.Mode().Perm() != 0o700:
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("make %s mode 0700: %w", dir, err)
		}
	}
	f, err := os.CreateTemp(dir, ".sessions-*.json")
	if err != nil {
		return fmt.Errorf("write the sessions of writ login: %w", err)
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// liveSessions returns the control planes the user holds a live session
// for, sorted.
func (a *App) liveSessions(doc sessionsDoc) []string {
	now := a.opts.Now()
	var out []string
	for u, s := range doc.Sessions {
		if now.Before(s.EndsAt) {
			out = append(out, u)
		}
	}
	slices.Sort(out)
	return out
}

// serverURL finds the control plane a command talks to: --server,
// WRIT_SERVER, then the one control plane the user holds a session for. It returns "" when none
// names one, and "dev" for --server dev. from says which.
func (a *App) serverURL() (base, from string, err error) {
	if a.opts.Server != "" {
		return a.opts.Server, "--server", nil
	}
	if v, _ := a.opts.LookupEnv("WRIT_SERVER"); v != "" {
		return v, "WRIT_SERVER", nil
	}
	_, doc, err := a.readSessions()
	if err != nil {
		return "", "", err
	}
	switch live := a.liveSessions(doc); len(live) {
	case 0:
		return "", "", nil
	case 1:
		return live[0], "your session", nil
	default:
		return "", "", usagef("you hold sessions for %s: pick one with --server or WRIT_SERVER", strings.Join(live, " and "))
	}
}

// checkServer parses a control plane's URL and returns it with no trailing
// slash.
func checkServer(base, from string) (string, error) {
	if err := checkServerURL(base); err != nil {
		return "", usagef("%s %q: %v", from, base, err)
	}
	return strings.TrimSuffix(base, "/"), nil
}

// checkServerURL refuses a control plane's URL that is not https://, or
// http:// on a loopback address, with no path, user, query, or fragment:
// writ sends a token to it.
func checkServerURL(text string) error {
	u, err := url.Parse(text)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return fmt.Errorf("want a URL such as https://writ.example.com, with no path, user, query, or fragment")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme != "http" || !(host == "localhost" || ip != nil && ip.IsLoopback()) {
		return fmt.Errorf("want https://, or http:// on a loopback address; writ sends a token to it")
	}
	return nil
}

// LoginOptions are the flags of `writ login [URL]`.
type LoginOptions struct {
	JSON bool
	// URL is the control plane; empty means the server precedence.
	URL string
	// Issuer names the people issuer when writ serve has several.
	Issuer string
	// Device and Browser force a flow. With neither, writ login uses the
	// device flow when the issuer offers it, and the browser otherwise.
	Device, Browser bool
}

// formatLogin is the document writ login --json prints.
const formatLogin = "writ.login/v0"

type loginReport struct {
	Format      string              `json:"format"`
	OK          bool                `json:"ok"`
	Server      string              `json:"server"`
	Session     *server.SessionInfo `json:"session"`
	Diagnostics []jsonDiag          `json:"diagnostics"`
}

// Login is `writ login`: it signs the person in through writ serve, with
// the device flow or in a browser, and keeps the session. With --json, the
// prompts go to stderr and stdout holds writ.login/v0, which never holds
// the token.
func (a *App) Login(ctx context.Context, o LoginOptions) (err error) {
	defer escapeError(&err, o.JSON)
	base, from := o.URL, "the URL"
	if base == "" {
		if base, from, err = a.serverURL(); err != nil {
			return err
		}
	}
	switch base {
	case "":
		return usagef("writ login needs the control plane's URL: writ login https://writ.example.com")
	case "dev":
		return usagef("the dev control plane needs no sign-in: the CLI reads its token from %s in its data directory", DevClientFile)
	}
	if base, err = checkServer(base, from); err != nil {
		return err
	}
	r := &remote{base: base, http: apiClient()}
	prompt := a.humanOutput()
	if o.JSON {
		prompt = humanOut(a.opts.Stderr)
	}

	doc, err := r.get(ctx, "/v1/server", remoteTimeout)
	if re, ok := errors.AsType[*remoteError](err); ok {
		return a.remoteFailed(o.JSON, re.doc)
	}
	if err != nil {
		return err
	}
	var info server.ServerInfo
	if json.Unmarshal(doc, &info) != nil || info.Format != server.FormatServer {
		return fmt.Errorf("%s answered GET /v1/server with a body that is not %s", base, server.FormatServer)
	}
	if info.Dev {
		return usagef("%s is a dev control plane, which needs no sign-in", base)
	}

	// A writ serve from before writ login in a browser sends no
	// device_issuers, and offers the device flow alone.
	var flows struct {
		DeviceIssuers *[]string `json:"device_issuers"`
	}
	_ = json.Unmarshal(doc, &flows)

	var sess *server.SessionDoc
	if useDevice(info, flows.DeviceIssuers == nil, o) {
		sess, err = a.deviceLogin(ctx, r, o, prompt)
	} else {
		sess, err = a.browserLogin(ctx, r, o, prompt)
	}
	if err != nil {
		return err
	}
	if err := a.keepSession(ctx, base, sess); err != nil {
		return err
	}
	info2 := sess.Session
	if o.JSON {
		b, err := json.Marshal(loginReport{Format: formatLogin, OK: true, Server: base, Session: info2, Diagnostics: []jsonDiag{}})
		if err != nil {
			return err
		}
		_, err = a.opts.Stdout.Write(append(b, '\n'))
		return err
	}
	out := a.humanOutput()
	fmt.Fprintf(out, "Signed in to %s as %s (%s).\n", base, escapeHuman(info2.Display, false), escapeHuman(info2.Person, false))
	fmt.Fprintf(out, "Teams: %s\n", dash(strings.Join(info2.Teams, ", ")))
	fmt.Fprintf(out, "Session ends %s.\n", info2.EndsAt)
	return nil
}

// useDevice reports whether writ login uses the device flow: when a flag
// forces it, when writ serve offers no other (deviceOnly), or else when the
// issuer offers it. With no issuer named and several people issuers, writ
// serve picks or refuses, and the device flow asks it when one of them
// offers that flow.
func useDevice(info server.ServerInfo, deviceOnly bool, o LoginOptions) bool {
	switch {
	case o.Device || o.Browser:
		return o.Device
	case deviceOnly:
		return true
	case o.Issuer != "":
		return slices.Contains(info.DeviceIssuers, o.Issuer)
	}
	var people int
	for _, is := range info.Issuers {
		if is.Kind == string(oidc.People) {
			people++
		}
	}
	return len(info.DeviceIssuers) > 0 || people == 0
}

// deviceLogin asks writ serve to start a device sign-in, prints the code
// to type at the identity provider, and polls until the person signs in.
func (a *App) deviceLogin(ctx context.Context, r *remote, o LoginOptions, prompt out) (*server.SessionDoc, error) {
	body, _ := json.Marshal(server.DeviceRequest{Issuer: o.Issuer})
	status, resp, err := r.call(ctx, http.MethodPost, "/v1/auth/device", body, remoteTimeout)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, a.remoteFailed(o.JSON, resp)
	}
	var dev server.Device
	if err := json.Unmarshal(resp, &dev); err != nil || dev.Format != server.FormatDevice || dev.Handle == "" {
		return nil, fmt.Errorf("%s answered with a body that is not %s", r.base, server.FormatDevice)
	}
	expires, _ := time.Parse(time.RFC3339, dev.ExpiresAt)
	fmt.Fprintf(prompt, "Signing in to %s through issuer %s.\n", r.base, dev.Issuer)
	fmt.Fprintf(prompt, "To sign in, open %s and enter the code %s.\n", dev.VerificationURI, dev.UserCode)
	if !expires.IsZero() {
		fmt.Fprintf(prompt, "The code expires in %s.\n", minutes(expires.Sub(a.opts.Now())))
	}
	return a.pollLogin(ctx, r, dev, expires, o.JSON)
}

// minutes says a duration in whole minutes, at least one.
func minutes(d time.Duration) string {
	m := max(int((d+30*time.Second)/time.Minute), 1)
	if m == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// pollLogin polls writ serve at the interval it gives until the person
// signs in, the code runs out, or writ serve refuses. It returns nil and
// prints the refusal when writ serve refuses.
func (a *App) pollLogin(ctx context.Context, r *remote, dev server.Device, expires time.Time, asJSON bool) (*server.SessionDoc, error) {
	interval := time.Duration(max(dev.IntervalSeconds, 1)) * time.Second
	deadline := expires.Add(time.Minute)
	if expires.IsZero() {
		deadline = a.opts.Now().Add(30 * time.Minute)
	}
	body, _ := json.Marshal(server.DeviceTokenRequest{Handle: dev.Handle})
	for {
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if a.opts.Now().After(deadline) {
			return nil, fmt.Errorf("the sign-in code ran out at %s before the sign-in finished: run writ login again", dev.ExpiresAt)
		}
		status, resp, err := r.once(ctx, http.MethodPost, "/v1/auth/device/token", body, remoteTimeout, nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if _, ok := errors.AsType[*redirectError](err); ok {
				return nil, err
			}
			a.opts.Log.Warn("poll the sign-in", "err", err)
			continue
		}
		if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			interval += 5 * time.Second
			continue
		}
		if status != http.StatusOK {
			return nil, a.remoteFailed(asJSON, resp)
		}
		var doc server.SessionDoc
		if err := json.Unmarshal(resp, &doc); err != nil || doc.Format != server.FormatSession {
			return nil, fmt.Errorf("%s answered with a body that is not %s", r.base, server.FormatSession)
		}
		switch doc.Status {
		case server.SessionPending:
			interval = time.Duration(max(doc.IntervalSeconds, 1)) * time.Second
		case server.SessionSignedIn:
			return signedIn(r, resp)
		default:
			return nil, fmt.Errorf("%s answered the sign-in with status %q", r.base, doc.Status)
		}
	}
}

// signedIn reads the writ.session/v0 answer that signs the person in.
func signedIn(r *remote, resp []byte) (*server.SessionDoc, error) {
	var doc server.SessionDoc
	if err := json.Unmarshal(resp, &doc); err != nil || doc.Format != server.FormatSession {
		return nil, fmt.Errorf("%s answered with a body that is not %s", r.base, server.FormatSession)
	}
	if doc.Status != server.SessionSignedIn || !strings.HasPrefix(doc.Token, server.SessionPrefix) || doc.Session == nil {
		return nil, fmt.Errorf("%s signed the person in and sent no session", r.base)
	}
	return &doc, nil
}

// keepSession writes the new session into SessionsFile under base. A
// session it replaces is ended on the server first, as best it can.
func (a *App) keepSession(ctx context.Context, base string, doc *server.SessionDoc) error {
	path, file, err := a.readSessions()
	if err != nil {
		return err
	}
	if old, ok := file.Sessions[base]; ok && a.opts.Now().Before(old.EndsAt) {
		r := &remote{base: base, token: old.Token, http: apiClient()}
		if status, _, err := r.once(ctx, http.MethodDelete, "/v1/auth/session", nil, remoteTimeout, nil); err != nil || status != http.StatusOK && status != http.StatusUnauthorized {
			a.opts.Log.Warn("the session this sign-in replaces did not end on the server; it ends on its own", "session", old.ID, "ends", old.EndsAt, "status", status, "err", err)
		}
	}
	ends, err := time.Parse(time.RFC3339, doc.Session.EndsAt)
	if err != nil {
		return fmt.Errorf("%s sent a session whose end %q is not a time", base, doc.Session.EndsAt)
	}
	s := doc.Session
	file.Sessions[base] = savedSession{Token: doc.Token, ID: s.ID, Person: s.Person, Display: s.Display, Issuer: s.Issuer, Teams: s.Teams, EndsAt: ends}
	return a.writeSessions(path, file)
}

// LogoutOptions are the flags of `writ logout [URL]`.
type LogoutOptions struct {
	JSON bool
	URL  string
}

// formatLogout is the document writ logout --json prints.
const formatLogout = "writ.logout/v0"

type logoutReport struct {
	Format string `json:"format"`
	OK     bool   `json:"ok"`
	Server string `json:"server"`
	// Session is the id of the session, not its token.
	Session string `json:"session"`
	// Ended says whether writ serve ended the session now; false when it
	// had already ended there.
	Ended       bool       `json:"ended"`
	Diagnostics []jsonDiag `json:"diagnostics"`
}

// Logout is `writ logout`: it ends the session on writ serve and deletes
// it from SessionsFile. A session writ serve no longer knows is deleted
// too. When writ serve cannot be reached the session is still deleted
// here, and the command fails, since the session stays live on the server
// until it ends.
func (a *App) Logout(ctx context.Context, o LogoutOptions) (err error) {
	defer escapeError(&err, o.JSON)
	base, from := o.URL, "the URL"
	if base == "" {
		if base, from, err = a.serverURL(); err != nil {
			return err
		}
	}
	switch base {
	case "":
		return usagef("you hold no session of writ login")
	case "dev":
		return usagef("the dev control plane has no sessions to end")
	}
	if base, err = checkServer(base, from); err != nil {
		return err
	}
	path, file, err := a.readSessions()
	if err != nil {
		return err
	}
	sess, ok := file.Sessions[base]
	if !ok {
		return usagef("you hold no session for %s", base)
	}
	delete(file.Sessions, base)
	r := &remote{base: base, token: sess.Token, http: apiClient()}
	status, resp, callErr := r.call(ctx, http.MethodDelete, "/v1/auth/session", nil, remoteTimeout)
	if err := a.writeSessions(path, file); err != nil {
		return err
	}
	rep := logoutReport{Format: formatLogout, OK: true, Server: base, Session: sess.ID, Diagnostics: []jsonDiag{}}
	switch {
	case callErr != nil:
		rep.OK = false
		rep.Diagnostics = append(rep.Diagnostics, jsonDiag{Severity: "error", Class: "state-io",
			Message: fmt.Sprintf("deleted the session here, and could not end it on %s: %v; it stays live there until %s, or until an admin ends session %s", base, callErr, sess.EndsAt.UTC().Format(time.RFC3339), sess.ID)})
	case status == http.StatusOK:
		rep.Ended = true
	case status == http.StatusUnauthorized:
	default:
		return a.remoteFailed(o.JSON, resp)
	}
	if o.JSON {
		b, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		if _, err := a.opts.Stdout.Write(append(b, '\n')); err != nil {
			return err
		}
	} else {
		out := a.humanOutput()
		switch {
		case !rep.OK:
			fmt.Fprintf(out, "error: state-io: %s\n", escapeHuman(rep.Diagnostics[0].Message, false))
		case rep.Ended:
			fmt.Fprintf(out, "Signed out of %s: session %s of %s ended.\n", base, sess.ID, escapeHuman(sess.Display, false))
		default:
			fmt.Fprintf(out, "Signed out of %s: session %s had already ended there.\n", base, sess.ID)
		}
	}
	if !rep.OK {
		return ErrFailed
	}
	return nil
}
