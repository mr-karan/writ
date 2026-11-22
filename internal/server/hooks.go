package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
)

// A webhook delivery only makes the poll read a repository's default
// branch sooner, through the forge's client and read token. Nothing in
// its body is trusted: the repository is the one the path names, and the
// head is what the forge's API says. A GitHub body, once its signature
// passed, is read only to tell a push from a ping.
const (
	// hookBody bounds a delivery's body, which is read to bound it, to
	// check a signature over it, and on GitHub to tell its event.
	hookBody = 1 << 20
	// hookPerAddress and hookPerRepo bound the deliveries a second from
	// one client address, checked first, and for one repository, checked
	// once a delivery passed its check, each with bursts of as many.
	hookPerAddress = 10
	hookPerRepo    = 5
	// hookDeliveries is how many delivery ids writ serve remembers to
	// drop a forge's retry of a delivery it acted on.
	hookDeliveries = 4096
	// hookSkew is how far a signed delivery's timestamp may be from now.
	hookSkew = 5 * time.Minute
	// signingPrefix starts a GitLab signing token, as GitLab shows it.
	signingPrefix = "whsec_"
)

// hookForge is how one kind of forge proves a webhook delivery, its
// entry of forgeKinds.
type hookForge struct {
	// verify reports whether a delivery with header h and raw body proves
	// that its sender holds secret, and when not, the audit row's check.
	verify func(h http.Header, body, secret []byte, now time.Time) (ok bool, check string)
	// delivery is the delivery's id, the same on each retry, or "", from
	// its header and its raw body, which passed verify.
	delivery func(h http.Header, body []byte) string
	// moves reports whether the delivery's event can move a branch, from
	// its header and its raw body, which passed verify.
	moves func(h http.Header, body []byte) bool
}

// verifyGitLab checks a GitLab delivery against the repository's secret:
// a signing token, which starts with whsec_, against the webhook-signature
// header, as GitLab 19.1 and later sign; any other secret against the
// X-Gitlab-Token header, GitLab's secret token.
func verifyGitLab(h http.Header, body, secret []byte, now time.Time) (bool, string) {
	if key, ok := bytes.CutPrefix(secret, []byte(signingPrefix)); ok {
		return verifySigned(h, body, key, now)
	}
	got := h.Get("X-Gitlab-Token")
	if got == "" {
		return false, "hook-no-token"
	}
	if !secretEqual([]byte(got), secret) {
		return false, "hook-token"
	}
	return true, ""
}

// verifySigned checks a delivery signed as the Standard Webhooks
// specification says, which GitLab follows: webhook-signature holds
// v1,BASE64 of the HMAC-SHA256 of "ID.TIMESTAMP.BODY" under the key the
// signing token encodes in base64 after whsec_. The timestamp must be
// within hookSkew of now.
func verifySigned(h http.Header, body, b64key []byte, now time.Time) (bool, string) {
	key, err := base64.StdEncoding.DecodeString(string(b64key))
	if err != nil || len(key) == 0 {
		return false, "hook-secret-file"
	}
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" {
		return false, "hook-no-signature"
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(sec, 0)).Abs() > hookSkew {
		return false, "hook-timestamp"
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := []byte("v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	ok := false
	for i, sig := range strings.Fields(sigs) {
		if i == 8 {
			break
		}
		ok = secretEqual([]byte(sig), want) || ok
	}
	if !ok {
		return false, "hook-signature"
	}
	return true, ""
}

// verifyGitHub checks a GitHub delivery against the repository's secret:
// X-Hub-Signature-256 holds sha256= and the hex of the HMAC-SHA256 of the
// raw body under the secret, as GitHub signs. GitHub signs no timestamp
// or delivery id, so a replayed delivery passes; it can only make the
// poll read the head sooner, and the repository's rate limit bounds it.
func verifyGitHub(h http.Header, body, secret []byte, _ time.Time) (bool, string) {
	got := h.Get("X-Hub-Signature-256")
	if got == "" {
		return false, "hook-no-signature"
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := []byte("sha256=" + hex.EncodeToString(mac.Sum(nil)))
	if !secretEqual([]byte(got), want) {
		return false, "hook-signature"
	}
	return true, ""
}

// secretEqual compares a proof a request sent with the one writ serve
// holds in time that depends on neither: it compares their SHA-256
// digests, which have one length, with subtle.ConstantTimeCompare.
func secretEqual(got, want []byte) bool {
	a, b := sha256.Sum256(got), sha256.Sum256(want)
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// deliveryRE is what a delivery id may look like. Another id is treated
// as none.
var deliveryRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// gitlabDelivery is webhook-id, or Idempotency-Key before GitLab 19.0:
// GitLab keeps each the same across the retries of a delivery. A signed
// delivery is keyed by its webhook-id alone, which the signature covers,
// so a replay can't change its key with a header no one signed. An id the
// pattern doesn't take is keyed by its SHA-256.
func gitlabDelivery(h http.Header, _ []byte) string {
	if h.Get("webhook-signature") != "" {
		id := h.Get("webhook-id")
		if id == "" || deliveryRE.MatchString(id) {
			return id
		}
		sum := sha256.Sum256([]byte(id))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	for _, k := range []string{"webhook-id", "Idempotency-Key"} {
		if v := h.Get(k); deliveryRE.MatchString(v) {
			return v
		}
	}
	return ""
}

// gitlabMoves reports whether a GitLab delivery is a push event, which
// GitLab sends for each push and merge to a branch.
func gitlabMoves(h http.Header, _ []byte) bool { return h.Get("X-Gitlab-Event") == "Push Hook" }

// githubDelivery is the SHA-256 of the signed body. GitHub sends a
// delivery again with the same body, and X-GitHub-Delivery is not
// signed, so a replay could change it; the body it cannot change. Two
// pushes differ in their body, which names each push's commits.
func githubDelivery(_ http.Header, body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// githubMoves reports whether a GitHub delivery is a push event, which
// GitHub sends for each push and merge to a branch. A ping, sent when the
// webhook is made, passes its check and reads nothing. X-GitHub-Event is
// not signed, so the signed body must be a push's too: a JSON object with
// ref and after, and not a ping's zen. Then a captured ping, sent again as
// a push, reads nothing. What the body names is never believed.
func githubMoves(h http.Header, body []byte) bool {
	if h.Get("X-GitHub-Event") != "push" {
		return false
	}
	var push struct {
		Ref   *string         `json:"ref"`
		After *string         `json:"after"`
		Zen   json.RawMessage `json:"zen"`
	}
	return json.Unmarshal(body, &push) == nil && push.Ref != nil && push.After != nil && push.Zen == nil
}

// hookState is what the webhook route keeps between deliveries: its rate
// limits, the delivery ids it acted on, and the repositories it asked the
// poll to read.
type hookState struct {
	addrs, repos *limiter

	mu sync.Mutex
	// seen maps a repository and a delivery id to its place in ring, the
	// last hookDeliveries ids acted on; next is the place the next one
	// takes.
	seen map[string]uint64
	ring []string
	next uint64
	// pending are the repositories the poll reads at its next wake.
	pending map[string]bool
	// wake tells the poll that pending has a repository.
	wake chan struct{}
}

func newHookState(now func() time.Time) *hookState {
	return &hookState{
		addrs: newLimiter(hookPerAddress, maxBuckets, now), repos: newLimiter(hookPerRepo, maxBuckets, now),
		seen: map[string]uint64{}, ring: make([]string, hookDeliveries), pending: map[string]bool{}, wake: make(chan struct{}, 1),
	}
}

// first records key and reports whether it is new. The oldest key goes
// when hookDeliveries are kept.
func (h *hookState) first(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.seen[key]; ok {
		return false
	}
	slot := h.next % uint64(len(h.ring))
	if old := h.ring[slot]; old != "" && h.seen[old] == h.next-uint64(len(h.ring)) {
		delete(h.seen, old)
	}
	h.ring[slot], h.seen[key] = key, h.next
	h.next++
	return true
}

// forget drops key, for a delivery that first took and writ serve then
// did not act on, so the forge's retry of it is acted on.
func (h *hookState) forget(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.seen, key)
}

// queue asks the poll to read the repository named name.
func (h *hookState) queue(name string) {
	h.mu.Lock()
	h.pending[name] = true
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// take returns the repositories queued since the last take.
func (h *hookState) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for name := range h.pending {
		out = append(out, name)
	}
	clear(h.pending)
	return out
}

// hookSpec is the pinned repository named name when it has a webhook
// secret file, with its forge and the forge's way of proving a delivery.
// forgeName must be the name of the repository's forge.
func (s *Server) hookSpec(forgeName, name string) (RepoSpec, Forge, hookForge, bool) {
	if !s.cfg.PinRepos {
		return RepoSpec{}, Forge{}, hookForge{}, false
	}
	for _, spec := range s.cfg.Repos {
		if spec.Name != name || spec.WebhookSecretFile == "" {
			continue
		}
		f, err := findForge(s.cfg.Forges, spec.Forge)
		if err != nil || f.Name != forgeName {
			break
		}
		fk, ok := forgeKinds[f.Kind]
		return spec, f, fk.hook, ok
	}
	return RepoSpec{}, Forge{}, hookForge{}, false
}

// hasHook reports whether a repository row has a webhook: the config pins
// it with a webhook secret file.
func (s *Server) hasHook(r control.Repo) bool {
	if r.Origin != control.FromConfig {
		return false
	}
	_, _, _, ok := s.hookSpec(r.Forge, r.Name)
	return ok
}

// hookVia starts the via of a hook row whose path names a pinned
// repository with a webhook, before the repository's name. A delivery
// to any other name leaves no via.
const hookVia = "webhook of repository "

// hookRepo is the repository a hook row names, and whether it names one.
func hookRepo(e control.Entry) (string, bool) {
	if e.Op != "hook" {
		return "", false
	}
	name, ok := strings.CutPrefix(e.Via, hookVia)
	return name, ok && name != ""
}

// HookRepo is the repository the audit row of a webhook delivery names,
// "" for a row of anything else or of a delivery to a name with no
// webhook.
func HookRepo(r AuditRow) string {
	name, _ := hookRepo(control.Entry{Op: r.Op, Via: r.Via})
	return name
}

// readableHookRepos are the pinned repositories with a webhook of which
// teams may read at least one stack, in some environment, of those the
// head of the default branch holds.
func (s *Server) readableHookRepos(ctx context.Context, teams []string) (map[string]string, error) {
	out := map[string]string{}
	if !s.cfg.PinRepos {
		return out, nil
	}
	envs := s.envNames()
	for _, spec := range s.cfg.Repos {
		if spec.WebhookSecretFile == "" {
			continue
		}
		rp, err := s.store.Repo(ctx, spec.Name)
		if errors.Is(err, control.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if rp.Head == "" || rp.Origin != control.FromConfig || !s.sameSpec(rp, spec) {
			continue
		}
		head, err := s.store.Commit(ctx, rp.Name, rp.Head)
		if errors.Is(err, control.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(stacksOf(head), func(d discovered) bool {
			return slices.ContainsFunc(envs, func(env string) bool {
				return s.cfg.Policy.Decide(teams, authz.Read, []string{rp.Prefix + d.Dir}, env, nil).Allowed
			})
		}) {
			out[rp.Name] = hookProject(rp)
		}
	}
	return out, nil
}

func hookProject(r control.Repo) string {
	return r.ForgeURL + "\n" + r.ProjectID
}

// hookSecretMax is the most a webhook secret file may hold.
const hookSecretMax = 4096

// CheckHookSecret reports whether file is a webhook secret file writ
// serve can use, for writ serve --check. Its error never holds what the
// file does.
func CheckHookSecret(file string) error {
	_, err := readHookSecret(file)
	return err
}

// readHookSecret reads a webhook secret file. The error names the file,
// never what it holds.
func readHookSecret(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("read webhook_secret_file: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("webhook_secret_file %s is not a regular file", file)
	}
	b, err := io.ReadAll(io.LimitReader(f, hookSecretMax+1))
	if err != nil {
		return nil, fmt.Errorf("read webhook_secret_file: %w", err)
	}
	if len(b) > hookSecretMax {
		return nil, fmt.Errorf("webhook_secret_file %s is over %d bytes", file, hookSecretMax)
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, fmt.Errorf("webhook_secret_file %s is empty", file)
	}
	return b, nil
}

// receiveHook is POST /v1/hooks/{forge}/{repo}: a forge's webhook
// delivery for a pinned repository. A delivery that passes its check asks
// the poll to read the repository's default branch now. It never plans or
// applies, and nothing of its body is kept, logged, or believed.
func (s *Server) receiveHook(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	e := &req.entry
	e.Op = "hook"
	if !s.hooks.addrs.allow(s.clientKey(r)) {
		// Like the server's address limit, this leaves no audit row, so a
		// sender can't fill the control database: shed counts it instead.
		req.audited = true
		s.shed.add(s, s.remoteOf(r))
		w.Header().Set("Retry-After", "1")
		msg := fmt.Sprintf("more than %d webhook deliveries a second from %s", hookPerAddress, s.remoteOf(r))
		s.reply(w, http.StatusTooManyRequests, refusal{Format: FormatHook, Diagnostics: []Diagnostic{{"error", string(diag.Usage), msg}}})
		return
	}
	// The body is read before the repository is looked up, the same way
	// for every name, so neither its status nor whether the connection
	// stays open tells a sender which names have a webhook.
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, hookBody))
	if errors.As(err, new(*http.MaxBytesError)) {
		s.hookRefused(w, r, "hook-body")
		return
	}
	if err != nil {
		s.hookRefused(w, r, "hook-body")
		return
	}
	forgeName, name := r.PathValue("forge"), r.PathValue("repo")
	spec, f, hf, ok := s.hookSpec(forgeName, name)
	if !ok {
		s.hookRefused(w, r, "hook-unknown")
		return
	}
	e.Via = hookVia + name
	rp, err := s.store.Repo(r.Context(), name)
	switch {
	case err == nil && rp.Origin == control.FromConfig && s.sameSpec(rp, spec):
		e.HookProject = hookProject(rp)
	case err != nil && !errors.Is(err, control.ErrNotFound):
		s.log.Warn("could not identify the project for a webhook audit row", "repo", name, "err", err)
	}
	secret, err := readHookSecret(spec.WebhookSecretFile)
	if err != nil {
		s.log.Error("a webhook delivery is refused: its secret is unreadable", "request", req.id, "repo", name, "err", err)
		s.hookRefused(w, r, "hook-secret-file")
		return
	}
	if ok, check := hf.verify(r.Header, body, secret, s.now()); !ok {
		s.hookRefused(w, r, check)
		return
	}
	e.Principal = "forge " + f.Name
	doc := HookDoc{Format: FormatHook, OK: true, Repo: name, Diagnostics: []Diagnostic{}}
	if !hf.moves(r.Header, body) {
		e.Reason = "repository " + name + ": the delivery is not a push event, so nothing is read"
		s.reply(w, http.StatusOK, doc)
		return
	}
	key := ""
	if id := hf.delivery(r.Header, body); id != "" {
		key = name + "\x00" + id
		if !s.hooks.first(key) {
			doc.Duplicate = true
			e.Reason = "repository " + name + ": a delivery with this id was acted on already"
			s.reply(w, http.StatusOK, doc)
			return
		}
	}
	if !s.hooks.repos.allow(name) {
		s.hooks.forget(key)
		w.Header().Set("Retry-After", "1")
		s.refuse(w, r, FormatHook, diag.Usage, http.StatusTooManyRequests, "more than %d webhook deliveries a second for repository %s", hookPerRepo, name)
		return
	}
	if err := s.store.RecordHook(r.Context(), name, s.now()); err != nil && !errors.Is(err, control.ErrNotFound) {
		s.hooks.forget(key)
		s.dbFailed(w, r, FormatHook, err)
		return
	}
	s.hooks.queue(name)
	e.Decision = control.Allowed
	e.Reason = "repository " + name + ": a read of the default branch's head is queued"
	doc.Queued = true
	s.reply(w, http.StatusAccepted, doc)
}

// hookRefused answers 401 for a delivery that did not prove it came from
// the repository's forge, with the same words whatever the reason, so the
// answer tells a sender nothing. The audit row's check says which.
func (s *Server) hookRefused(w http.ResponseWriter, r *http.Request, check string) {
	req := reqOf(r)
	req.entry.Decision, req.entry.Check = control.AuthFailed, check
	s.log.Warn("webhook delivery refused", "request", req.id, "check", check, "remote", req.entry.Remote)
	s.refuse(w, r, FormatHook, diag.Auth, http.StatusUnauthorized, "the delivery failed verification")
}
