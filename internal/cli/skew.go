package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// serverSeen is what the remote commands learned of writ serve's version
// from its answers' X-Writ-Version header.
type serverSeen struct {
	mu      sync.Mutex
	version string
	warned  bool
}

// noteServerVersion keeps v, writ serve's version, and the first time it
// is newer than this writ by a feature release or more, warns on stderr:
// a newer server can send answers this writ does not know.
func (a *App) noteServerVersion(v string) {
	s := &a.seen
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = v
	if s.warned || !newerRelease(v, a.opts.Version) {
		return
	}
	s.warned = true
	fmt.Fprintf(a.opts.Stderr, "writ: warning: writ serve is %s and this writ is %s: a newer writ serve can answer in ways an older writ cannot read; install writ %s, the version of writ serve\n",
		escapeHuman(v, false), a.clientVersion(), escapeHuman(v, false))
}

// clientVersion is this writ's version, dev for a build that names none.
func (a *App) clientVersion() string {
	if a.opts.Version == "" {
		return "dev"
	}
	return a.opts.Version
}

// serverVersion is writ serve's version from its answers, "" before one
// named it.
func (a *App) serverVersion() string {
	a.seen.mu.Lock()
	defer a.seen.mu.Unlock()
	return a.seen.version
}

// newerRelease reports whether server is a newer feature release than
// client: a newer major or minor version, or, before 1.0, any newer
// version, since a 0.y release adds features in its patch number. A
// version that is not semver, such as a development build's "dev", is
// never newer and is never warned about.
func newerRelease(server, client string) bool {
	s, ok1 := semver(server)
	c, ok2 := semver(client)
	if !ok1 || !ok2 {
		return false
	}
	if s[0] != c[0] {
		return s[0] > c[0]
	}
	if s[1] != c[1] {
		return s[1] > c[1]
	}
	return s[0] == 0 && s[2] > c[2]
}

// semver parses v1.2.3 or 1.2.3, with any pre-release or build suffix
// after the patch number left out.
func semver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// refusalDiags returns the error diagnostics of a refusal writ serve
// sent: a JSON object with ok false and at least one diagnostic whose
// severity is error. ok is false for any other body, which a command
// cannot report as writ serve's own refusal.
func refusalDiags(doc []byte) (diags []jsonDiag, ok bool) {
	var head struct {
		OK          *bool      `json:"ok"`
		Diagnostics []jsonDiag `json:"diagnostics"`
	}
	if json.Unmarshal(doc, &head) != nil || head.OK == nil || *head.OK {
		return nil, false
	}
	for _, d := range head.Diagnostics {
		if d.Severity == "error" {
			return head.Diagnostics, true
		}
	}
	return nil, false
}

// unusable is the error of an answer a command cannot use: what it got,
// and both versions when writ serve named its own.
func (a *App) unusable(what string) error {
	msg := "writ serve sent " + what + ", which this writ cannot use"
	if v := a.serverVersion(); v != "" {
		msg += fmt.Sprintf("; writ serve is %s and this writ is %s", escapeHuman(v, false), a.clientVersion())
		if v != a.clientVersion() {
			msg += fmt.Sprintf(": install writ %s, the version of writ serve", escapeHuman(v, false))
		}
	} else {
		msg += "; writ serve may be newer than this writ, " + a.clientVersion() + ": install the version of writ serve"
	}
	return fmt.Errorf("%s", msg)
}
