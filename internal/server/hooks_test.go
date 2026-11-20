package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
)

// hookSecret is infra's webhook secret token. No answer, log line, or
// audit row may hold it, nor a wrong token a sender tried.
const hookSecret = "hook-SERVERTEST-secret-0123456789abcdef"

const hookPath = "/v1/hooks/gitlab/infra"

// hookFixture pins infra, with a webhook whose secret file holds secret,
// and apps, with none, and polls once an hour, so only a webhook reads a
// new head soon. Both repositories are read when it returns.
func hookFixture(t *testing.T, secret string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	return hookFixtureWith(t, secret, "", setup...)
}

// hookFixtureWith is hookFixture with more config text.
func hookFixtureWith(t *testing.T, secret, more string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	file := filepath.Join(t.TempDir(), "infra_hook")
	if err := os.WriteFile(file, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, withForge(t, fmt.Sprintf(`poll_every = "1h"
repo {
  name = "infra"
  forge = "gitlab"
  project = "acme/infra"
  path = "infra"
  webhook_secret_file = %q
}
repo {
  name = "apps"
  forge = "gitlab"
  project = "acme/apps"
  path = "infra"
  prefix = "apps/"
}
%s`, file, more)), setup...)
	f.gitlab.AddProject(4242, "acme/infra", "main")
	f.gitlab.Commit(4242, "main", infraFiles())
	f.gitlab.AddProject(4343, "acme/apps", "main")
	f.gitlab.Commit(4343, "main", infraFiles())
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		f.srv.Poll()
		a, errA := f.store.Repo(context.Background(), "infra")
		b, errB := f.store.Repo(context.Background(), "apps")
		if errA == nil && errB == nil && a.Head != "" && b.Head != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pinned repositories were not read")
		}
	}
	// The poll that started with the server may still be reading.
	for f.gitlab.Calls(); ; f.gitlab.Calls() {
		time.Sleep(300 * time.Millisecond)
		if len(f.gitlab.Calls()) == 0 {
			break
		}
	}
	return f
}

// hook sends a GitLab push delivery to path: the token, when set, as
// X-Gitlab-Token, and the delivery id, when set, as Idempotency-Key.
func (f *fixture) hook(path, token, id string, body []byte, hdr ...string) response {
	f.t.Helper()
	h := append([]string{"Content-Type", "application/json", "X-Gitlab-Event", "Push Hook"}, hdr...)
	if token != "" {
		h = append(h, "X-Gitlab-Token", token)
	}
	if id != "" {
		h = append(h, "Idempotency-Key", id)
	}
	return f.do("POST", path, "", bytes.NewReader(body), h...)
}

// hostilePush is a push payload that names another project, another
// commit, another branch, and other URLs. writ serve believes none of it.
func hostilePush(commit string) []byte {
	b, _ := json.Marshal(map[string]any{
		"object_kind": "push", "event_name": "push", "ref": "refs/heads/evil", "before": strings.Repeat("0", 40), "after": commit, "checkout_sha": commit,
		"project_id": 4343, "project": map[string]any{"id": 4343, "path_with_namespace": "acme/apps", "default_branch": "evil",
			"git_http_url": "https://evil.example.com/acme/apps.git", "web_url": "https://evil.example.com/acme/apps"},
		"repository": map[string]any{"url": "https://evil.example.com/acme/apps.git", "homepage": "https://evil.example.com"},
		"commits":    []map[string]any{{"id": commit, "url": "https://evil.example.com/commit/" + commit}},
	})
	return b
}

// waitHead waits until the repository's head is want.
func (f *fixture) waitHead(name, want string) control.Repo {
	f.t.Helper()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		rp, err := f.store.Repo(context.Background(), name)
		if err == nil && rp.Head == want {
			return rp
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("repository %s did not read %s: %+v %v", name, want, rp, err)
		}
	}
}

// TestHookReadsTheHead checks that a delivery with the secret makes writ
// serve read the head of the repository the path names, through the
// forge's API, at once, and that nothing its payload names is read: not
// the other project, the commit, the branch, or a URL.
func TestHookReadsTheHead(t *testing.T) {
	f := hookFixture(t, hookSecret)
	files := infraFiles()
	maps.Copy(files, tofuStack("infra/apps"))
	next := f.gitlab.Commit(4242, "main", files)
	appsBefore, _ := f.store.Repo(context.Background(), "apps")
	appsNext := f.gitlab.Commit(4343, "main", files)
	r := f.hook(hookPath, hookSecret, "delivery-1", hostilePush(appsNext))
	if r.status != http.StatusAccepted || r.doc["format"] != server.FormatHook || r.doc["queued"] != true || r.doc["repo"] != "infra" {
		t.Fatalf("delivery: %d %s", r.status, r.body)
	}
	got := f.waitHead("infra", next)
	if got.HookAt.IsZero() {
		t.Fatalf("no hook time recorded: %+v", got)
	}
	for _, c := range f.gitlab.Calls() {
		if !strings.Contains(c, "/projects/4242/") || strings.Contains(c, appsNext) || strings.Contains(c, "evil") {
			t.Errorf("the delivery led to the call %s", c)
		}
	}
	if apps, _ := f.store.Repo(context.Background(), "apps"); apps.Head != appsBefore.Head || !apps.HookAt.IsZero() {
		t.Fatalf("the payload's project was read: %+v", apps)
	}

	// The API shows which repository has a hook and when one last came.
	var doc server.ReposDoc
	_ = json.Unmarshal(f.do("GET", "/v1/repos", f.token(nil), nil).body, &doc)
	hooks := map[string]server.RepoInfo{}
	for _, rp := range doc.Repos {
		hooks[rp.Name] = rp
	}
	if !hooks["infra"].Hook || hooks["infra"].HookAt == "" || hooks["apps"].Hook || hooks["apps"].HookAt != "" {
		t.Fatalf("repositories %+v", doc.Repos)
	}
	var one server.RepoDoc
	_ = json.Unmarshal(f.do("GET", "/v1/repos/infra", f.token(nil), nil).body, &one)
	if !one.Repo.Hook || one.Repo.HookAt != hooks["infra"].HookAt {
		t.Fatalf("repository %+v", one.Repo)
	}

	// The audit row names the delivery's repository and what it did.
	rows := f.audit(controltest.AuditFilter{Request: r.header.Get("X-Request-Id")})
	if len(rows) != 1 || rows[0].Op != "hook" || rows[0].Decision != control.Allowed || rows[0].Route != "/v1/hooks/{forge}/{repo}" ||
		rows[0].Principal != "forge gitlab" || !strings.Contains(rows[0].Reason, "repository infra") || rows[0].Status != http.StatusAccepted {
		t.Fatalf("audit rows %+v", rows)
	}
	page, _ := f.auditPage(admin(f), url.Values{"op": {"hook"}})
	if len(page.Rows) != 1 || page.Rows[0].Reason == "" {
		t.Fatalf("GET /v1/audit?op=hook: %+v", page.Rows)
	}
}

// TestHookRefusals checks that a delivery that does not prove it holds
// the repository's secret is refused with 401 and the same words whatever
// the reason, reads nothing, and leaves an audit row that says why.
func TestHookRefusals(t *testing.T) {
	f := hookFixture(t, hookSecret)
	push := hostilePush(strings.Repeat("a", 40))
	var bodies []string
	for _, c := range []struct {
		name, path, token, check string
	}{
		{"wrong token", hookPath, "not-the-secret", "hook-token"},
		{"near miss", hookPath, hookSecret + "0", "hook-token"},
		{"prefix of the secret", hookPath, hookSecret[:20], "hook-token"},
		{"no token", hookPath, "", "hook-no-token"},
		{"a repository with no hook", "/v1/hooks/gitlab/apps", hookSecret, "hook-unknown"},
		{"no such repository", "/v1/hooks/gitlab/nope", hookSecret, "hook-unknown"},
		{"another forge", "/v1/hooks/github/infra", hookSecret, "hook-unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := f.hook(c.path, c.token, "", push)
			if r.status != http.StatusUnauthorized || r.class() != "auth" {
				t.Fatalf("%d %s", r.status, r.body)
			}
			bodies = append(bodies, string(r.body))
			rows := f.audit(controltest.AuditFilter{Request: r.header.Get("X-Request-Id")})
			if len(rows) != 1 || rows[0].Op != "hook" || rows[0].Decision != control.AuthFailed || rows[0].Check != c.check || rows[0].Principal != "" {
				t.Fatalf("audit rows %+v", rows)
			}
		})
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("two refusals differ:\n%s\n%s", bodies[0], b)
		}
	}
	if r := f.do("GET", hookPath, "", nil); r.status != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d %s", r.status, r.body)
	}
	// A body over the limit is refused, before the secret is checked and
	// before the repository is looked up, with the same 401 for a name
	// with a webhook and for any other, so its status tells a sender
	// nothing of which names exist.
	big := bytes.Repeat([]byte(" "), 1<<20+1)
	known, unknown := f.hook(hookPath, hookSecret, "big", big), f.hook("/v1/hooks/gitlab/nope", hookSecret, "big2", big)
	if known.status != http.StatusUnauthorized || unknown.status != known.status || !bytes.Equal(unknown.body, known.body) {
		t.Fatalf("a large body: %d %s, for an unknown name %d %s", known.status, known.body, unknown.status, unknown.body)
	}
	// Another event than a push passes its check and reads nothing.
	if r := f.hook(hookPath, hookSecret, "mr", push, "X-Gitlab-Event", "Merge Request Hook"); r.status != http.StatusOK || r.doc["queued"] != false {
		t.Fatalf("a merge request event: %d %s", r.status, r.body)
	}
	time.Sleep(200 * time.Millisecond)
	if calls := f.gitlab.Calls(); len(calls) != 0 {
		t.Fatalf("a refused delivery led to the calls %v", calls)
	}
	if rp, _ := f.store.Repo(context.Background(), "infra"); !rp.HookAt.IsZero() {
		t.Fatalf("a refused delivery recorded a hook time: %+v", rp)
	}
}

// TestHookDropsDuplicates checks that a retry of a delivery, with the same
// id, is answered 200 and reads nothing, and that a delivery id belongs to
// its repository.
func TestHookDropsDuplicates(t *testing.T) {
	f := hookFixture(t, hookSecret)
	push := hostilePush(strings.Repeat("a", 40))
	if r := f.hook(hookPath, hookSecret, "delivery-1", push); r.status != http.StatusAccepted {
		t.Fatalf("first: %d %s", r.status, r.body)
	}
	f.waitCalls()
	r := f.hook(hookPath, hookSecret, "delivery-1", push)
	if r.status != http.StatusOK || r.doc["duplicate"] != true || r.doc["queued"] != false {
		t.Fatalf("retry: %d %s", r.status, r.body)
	}
	// webhook-id, which GitLab 19.0 added, is the same id.
	if r := f.hook(hookPath, hookSecret, "", push, "webhook-id", "delivery-1"); r.status != http.StatusOK || r.doc["duplicate"] != true {
		t.Fatalf("retry by webhook-id: %d %s", r.status, r.body)
	}
	time.Sleep(200 * time.Millisecond)
	if calls := f.gitlab.Calls(); len(calls) != 0 {
		t.Fatalf("a retry led to the calls %v", calls)
	}
	if r := f.hook(hookPath, hookSecret, "delivery-2", push); r.status != http.StatusAccepted {
		t.Fatalf("another delivery: %d %s", r.status, r.body)
	}
	f.waitCalls()
}

// waitCalls waits for the forge to be called, and forgets the calls.
func (f *fixture) waitCalls() {
	f.t.Helper()
	for deadline := time.Now().Add(time.Minute); len(f.gitlab.Calls()) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			f.t.Fatal("the forge was not called")
		}
	}
}

// TestHookRateLimits checks the two limits on deliveries, on a clock that
// stands still so no bucket refills: one on a client address, before any
// check, and one on a repository, once a delivery passed its check.
func TestHookRateLimits(t *testing.T) {
	at := time.Now()
	still := func(o *server.Options) { o.Now = func() time.Time { return at } }
	t.Run("address", func(t *testing.T) {
		f := hookFixture(t, hookSecret, still)
		for i := range 10 {
			if r := f.hook(hookPath, "wrong", "", nil); r.status != http.StatusUnauthorized {
				t.Fatalf("delivery %d: %d %s", i, r.status, r.body)
			}
		}
		if r := f.hook(hookPath, hookSecret, "x", nil); r.status != http.StatusTooManyRequests || !strings.Contains(string(r.body), "webhook deliveries a second from") {
			t.Fatalf("over the address limit: %d %s", r.status, r.body)
		}
	})
	t.Run("repository", func(t *testing.T) {
		f := hookFixture(t, hookSecret, still)
		for i := range 5 {
			if r := f.hook(hookPath, hookSecret, "d"+strconv.Itoa(i), nil); r.status != http.StatusAccepted {
				t.Fatalf("delivery %d: %d %s", i, r.status, r.body)
			}
		}
		r := f.hook(hookPath, hookSecret, "d5", nil)
		if r.status != http.StatusTooManyRequests || !strings.Contains(string(r.body), "for repository infra") {
			t.Fatalf("over the repository limit: %d %s", r.status, r.body)
		}
		// The refused delivery's id is not taken, so the forge's retry
		// of it counts.
		if r := f.hook("/v1/hooks/gitlab/infra", hookSecret, "d5", nil); r.status != http.StatusTooManyRequests {
			t.Fatalf("its retry: %d %s", r.status, r.body)
		}
	})
}

// TestHookSigned checks a GitLab signing token: a delivery signed over its
// body with the token's key passes, and one whose body, signature, or
// timestamp is not the one signed is refused, as is the secret token's
// header holding the whole signing token.
func TestHookSigned(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	token := "whsec_" + base64.StdEncoding.EncodeToString(key)
	f := hookFixture(t, token)
	body := hostilePush(strings.Repeat("a", 40))
	sign := func(id string, ts int64, body []byte) []string {
		mac := hmac.New(sha256.New, key)
		fmt.Fprintf(mac, "%s.%d.", id, ts)
		mac.Write(body)
		return []string{"webhook-id", id, "webhook-timestamp", strconv.FormatInt(ts, 10), "webhook-signature", "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))}
	}
	now := time.Now().Unix()
	if r := f.hook(hookPath, "", "", body, sign("msg_1", now, body)...); r.status != http.StatusAccepted {
		t.Fatalf("signed: %d %s", r.status, r.body)
	}
	f.waitCalls()
	for _, c := range []struct {
		name, check string
		hdr         []string
		body        []byte
	}{
		{"another body", "hook-signature", sign("msg_2", now, body), append([]byte(" "), body...)},
		{"another id", "hook-signature", append(sign("msg_3", now, body), "webhook-id", "msg_4"), body},
		{"an old timestamp", "hook-timestamp", sign("msg_5", now-600, body), body},
		{"no signature", "hook-no-signature", nil, body},
		{"the token as a secret token", "hook-no-signature", []string{"X-Gitlab-Token", token}, body},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := f.hook(hookPath, "", "", c.body, c.hdr...)
			rows := f.audit(controltest.AuditFilter{Request: r.header.Get("X-Request-Id")})
			if r.status != http.StatusUnauthorized || len(rows) != 1 || rows[0].Check != c.check {
				t.Fatalf("%d %s %+v", r.status, r.body, rows)
			}
		})
	}
}

// TestHookSecretNeverLeaks checks that neither the webhook secret nor a
// wrong token a sender tried reaches a log line, an audit row, or an
// answer of the API.
func TestHookSecretNeverLeaks(t *testing.T) {
	var logs lockedBuffer
	f := hookFixture(t, hookSecret, logTo(&logs))
	wrong := []string{"hook-WRONG-token-a1b2c3d4e5", hookSecret + "Z", hookSecret[:len(hookSecret)-1]}
	push := hostilePush(strings.Repeat("a", 40))
	var answers []string
	answers = append(answers, string(f.hook(hookPath, hookSecret, "leak-1", push).body))
	for _, w := range wrong {
		answers = append(answers, string(f.hook(hookPath, w, "", push).body))
		answers = append(answers, string(f.hook("/v1/hooks/gitlab/apps", w, "", push).body))
	}
	f.waitCalls()
	tok := admin(f)
	for _, p := range []string{"/v1/repos", "/v1/repos/infra", "/v1/config", "/v1/audit", "/v1/audit?op=hook"} {
		answers = append(answers, string(f.do("GET", p, tok, nil).body))
	}
	for _, row := range f.audit(controltest.AuditFilter{Limit: 1000}) {
		answers = append(answers, fmt.Sprintf("%+v", row))
	}
	answers = append(answers, logs.String())
	for _, a := range answers {
		for _, s := range append(wrong, hookSecret, hookSecret[:16]) {
			if strings.Contains(a, s) {
				t.Fatalf("%q leaked: %s", s, a)
			}
		}
	}
}

// TestHookBackstopPoll checks that the poll reads a repository with a
// webhook only every poll_every_with_hook, and one without every
// poll_every.
func TestHookBackstopPoll(t *testing.T) {
	f := hookFixture(t, hookSecret)
	files := infraFiles()
	maps.Copy(files, tofuStack("infra/apps"))
	next := f.gitlab.Commit(4242, "main", files)
	appsNext := f.gitlab.Commit(4343, "main", files)
	f.srv.Poll()
	f.waitHead("apps", appsNext)
	if rp, _ := f.store.Repo(context.Background(), "infra"); rp.Head == next {
		t.Fatal("the poll read a repository with a webhook before poll_every_with_hook")
	}
	for _, c := range f.gitlab.Calls() {
		if strings.Contains(c, "/projects/4242/") {
			t.Fatalf("the poll called %s for a repository with a webhook", c)
		}
	}
}

// A body under the cap is read whole for every name, so whether the
// connection stays open tells a sender nothing of which names have a
// webhook either.
func TestHookReadsTheBodyForEveryName(t *testing.T) {
	f := hookFixture(t, hookSecret)
	mid := bytes.Repeat([]byte(" "), 512<<10)
	var closes []bool
	for _, p := range []string{hookPath, "/v1/hooks/gitlab/nope"} {
		req, _ := http.NewRequest("POST", f.http.URL+p, bytes.NewReader(mid))
		req.Header.Set("X-Gitlab-Token", "wrong")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		closes = append(closes, resp.Close)
	}
	if closes[0] != closes[1] {
		t.Fatalf("the connection closed for one name and not the other: %v", closes)
	}
}

// A signed delivery is deduplicated by its signed webhook-id alone, so a
// replay inside the window can't make itself new with an unsigned
// Idempotency-Key, whatever the id looks like.
func TestHookSignedReplayIsDropped(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	f := hookFixture(t, "whsec_"+base64.StdEncoding.EncodeToString(key))
	body := []byte(`{"object_kind":"push"}`)
	for _, id := range []string{"msg_ok", "msg/slash+" + strings.Repeat("x", 140)} {
		ts := time.Now().Unix()
		mac := hmac.New(sha256.New, key)
		fmt.Fprintf(mac, "%s.%d.", id, ts)
		mac.Write(body)
		h := []string{"webhook-id", id, "webhook-timestamp", strconv.FormatInt(ts, 10), "webhook-signature", "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))}
		var got []int
		for i := range 3 {
			got = append(got, f.hook(hookPath, "", "replay-"+strconv.Itoa(i), body, h...).status)
		}
		if got[0] != http.StatusAccepted || got[1] != http.StatusOK || got[2] != http.StatusOK {
			t.Fatalf("webhook-id %.20q sent three times with fresh Idempotency-Keys: %v, want 202 200 200", id, got)
		}
		time.Sleep(1100 * time.Millisecond)
	}
}

// The webhook route's address limit leaves no audit row for what it
// refuses, as the server's own limit does, so a flood can't fill the
// control database.
func TestHookLimitLeavesNoAuditRow(t *testing.T) {
	at := time.Now()
	f := hookFixture(t, hookSecret, func(o *server.Options) { o.Now = func() time.Time { return at } })
	limited := 0
	for range 30 {
		r := f.hook(hookPath, "wrong", "", nil)
		rows := len(f.audit(controltest.AuditFilter{Request: r.header.Get("X-Request-Id")}))
		switch r.status {
		case http.StatusTooManyRequests:
			limited++
			if rows != 0 {
				t.Fatalf("a delivery the hook limit refused wrote %d audit rows", rows)
			}
		case http.StatusUnauthorized:
			if rows != 1 {
				t.Fatalf("a refused delivery wrote %d audit rows, want 1", rows)
			}
		}
	}
	if limited == 0 {
		t.Fatal("no delivery hit the hook limit")
	}
}

// A webhook secret file must be a regular file of at most 4 KiB, read
// with a bound, so a misconfigured path can't make each unverified
// delivery read without end.
func TestHookSecretFileIsBounded(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, bytes.Repeat([]byte("a"), 4097), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte(hookSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.CheckHookSecret(ok); err != nil {
		t.Fatalf("a good secret file: %v", err)
	}
	for _, p := range []string{big, dir} {
		err := server.CheckHookSecret(p)
		if err == nil {
			t.Fatalf("%s: no error", p)
		}
		if strings.Contains(err.Error(), "aaaa") {
			t.Fatalf("the error holds the file's contents: %v", err)
		}
	}
}

// A delivery's row is a row of no environment, which only admin on every
// stack in every environment used to see, so the team that runs a
// repository could not see its own repository's deliveries. A row whose
// path names a pinned repository with a webhook, passed or refused, now
// shows to a principal that may read a stack of that repository at the
// head of its default branch, and to no one else but admin. A delivery to
// any other name names no repository, and stays admin's.
func TestHookRowsShowToTheRepositorysReaders(t *testing.T) {
	f := hookFixtureWith(t, hookSecret, `
team "network_readers" {
  issuer = "gitlab"
  match = { user_login = ["nora"] }
}

team "apps_readers" {
  issuer = "gitlab"
  match = { user_login = ["ava"] }
}

grant {
  teams = ["network_readers"]
  stacks = ["network"]
  envs = ["prod"]
  can = ["read"]
}

grant {
  teams = ["apps_readers"]
  stacks = ["apps/**"]
  envs = ["*"]
  can = ["read", "plan", "approve"]
}
`)
	as := func(login string) string { return f.token(map[string]any{"user_login": login, "project_id": "5000"}) }
	push := hostilePush(strings.Repeat("a", 40))
	passed := f.hook(hookPath, hookSecret, "d-1", push)
	refused := f.hook(hookPath, "not-the-secret", "", push)
	unknown := f.hook("/v1/hooks/gitlab/nope", hookSecret, "", push)
	if passed.status != http.StatusAccepted || refused.status != http.StatusUnauthorized || unknown.status != http.StatusUnauthorized {
		t.Fatalf("deliveries: %d, %d, %d", passed.status, refused.status, unknown.status)
	}
	id := func(r response) string { return r.header.Get("X-Request-Id") }
	for _, c := range []struct {
		who  string
		tok  string
		want []string
	}{
		{"a reader of network, a stack of infra", as("nora"), []string{id(refused), id(passed)}},
		{"a team that reads, plans, and approves apps, another repository", as("ava"), nil},
		{"a principal with no grant", as("zed"), nil},
		{"admin on every stack in every environment", admin(f), []string{id(unknown), id(refused), id(passed)}},
	} {
		t.Run(c.who, func(t *testing.T) {
			rows, bodies := f.auditAll(c.tok, url.Values{"op": {"hook"}})
			var got []string
			for _, row := range rows {
				got = append(got, row.Request)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("hook rows %v, want %v", got, c.want)
			}
			for _, row := range rows {
				if row.Request == id(passed) && (row.Reason == "" || row.Via != "webhook of repository infra") ||
					row.Request == id(refused) && (row.Check != "hook-token" || row.Reason != "" || row.Via != "webhook of repository infra") ||
					row.Request == id(unknown) && row.Via != "" {
					t.Errorf("row %+v", row)
				}
			}
			if c.want == nil && strings.Contains(string(bodies), "infra") {
				t.Errorf("a page names infra: %s", bodies)
			}
			// With no op filter the same hook rows show among the rest.
			all, _ := f.auditAll(c.tok, url.Values{})
			got = nil
			for _, row := range all {
				if row.Op == "hook" {
					got = append(got, row.Request)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("hook rows of the whole log %v, want %v", got, c.want)
			}
		})
	}
	t.Run("replacement project cannot read old deliveries", func(t *testing.T) {
		if err := f.store.Audit(context.Background(), control.Entry{
			At: time.Now(), Request: "legacy-hook", Op: "hook", Kind: "request", Via: "webhook of repository infra", Decision: control.Allowed,
		}); err != nil {
			t.Fatal(err)
		}
		f.gitlab.AddProject(4444, "acme/replacement", "main")
		head := f.gitlab.Commit(4444, "main", changed("# replacement\n"))
		f.restart(func(config string) string {
			return strings.Replace(config, "project = \"acme/infra\"", "project = \"acme/replacement\"", 1)
		})
		f.srv.Poll()
		if rp := f.waitHead("infra", head); rp.ProjectID != "4444" {
			t.Fatalf("replacement repository: %+v", rp)
		}
		current := f.hook(hookPath, hookSecret, "d-new", push)
		if current.status != http.StatusAccepted {
			t.Fatalf("replacement delivery: %d %s", current.status, current.body)
		}
		rows, _ := f.auditAll(as("nora"), url.Values{"op": {"hook"}})
		if len(rows) != 1 || rows[0].Request != id(current) {
			t.Errorf("reader of the replacement project sees %+v", rows)
		}
		rows, _ = f.auditAll(admin(f), url.Values{"op": {"hook"}})
		if len(rows) != 5 {
			t.Errorf("admin sees %d hook rows, want 5", len(rows))
		}
	})
	// A filter of an environment leaves out every row of none.
	if rows, _ := f.auditAll(as("nora"), url.Values{"env": {"prod"}, "op": {"hook"}}); len(rows) != 0 {
		t.Fatalf("hook rows in prod: %+v", rows)
	}
}
