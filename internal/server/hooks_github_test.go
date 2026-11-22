package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
)

// githubToken is the GitHub forge's read token. No answer, log line, or
// audit row may hold it.
const githubToken = "github_pat_SERVERTEST_read_0123456789"

const githubHookPath = "/v1/hooks/github/web"

// githubFixture is a control plane with the forge github, a stand-in on
// loopback, besides the forge gitlab, and the pinned repository web, the
// GitHub repository acme/web, with a webhook whose secret file holds
// secret. It returns once web is read.
func githubFixture(t *testing.T, secret string) (*fixture, *forgetest.GitHub) {
	t.Helper()
	gh := forgetest.NewGitHub(t, githubToken)
	gh.AddProject(777001, "acme/web", "main")
	gh.Commit(777001, "main", infraFiles())
	dir := t.TempDir()
	tokenFile, secretFile := filepath.Join(dir, "github_read"), filepath.Join(dir, "web_hook")
	for p, v := range map[string]string{tokenFile: githubToken, secretFile: secret} {
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixture(t, withForge(t, fmt.Sprintf(`poll_every = "1h"
forge "github" {
  kind = "github"
  url = %q
  read_token_file = %q
}
repo {
  name = "web"
  forge = "github"
  project = "acme/web"
  path = "infra"
  prefix = "web/"
  webhook_secret_file = %q
}
`, gh.URL, tokenFile, secretFile)))
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		f.srv.Poll()
		if rp, err := f.store.Repo(context.Background(), "web"); err == nil && rp.Head != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pinned GitHub repository was not read")
		}
	}
	for gh.Calls(); ; gh.Calls() {
		time.Sleep(300 * time.Millisecond)
		if len(gh.Calls()) == 0 {
			break
		}
	}
	return f, gh
}

// githubHook sends a GitHub delivery of event to path, signed with
// secret when it is set.
func (f *fixture) githubHook(path, secret, event, id string, body []byte, hdr ...string) response {
	f.t.Helper()
	h := append([]string{"Content-Type", "application/json", "X-GitHub-Event", event, "User-Agent", "GitHub-Hookshot/0000000"}, hdr...)
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		h = append(h, "X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	if id != "" {
		h = append(h, "X-GitHub-Delivery", id)
	}
	return f.do("POST", path, "", bytes.NewReader(body), h...)
}

// TestGitHubRepository checks that a GitHub repository is read, by its
// numeric id after it is found by its path, and stored with kind github.
func TestGitHubRepository(t *testing.T) {
	f, gh := githubFixture(t, hookSecret)
	rp, err := f.store.Repo(context.Background(), "web")
	if err != nil || rp.Kind != control.GitHub || rp.ProjectID != "777001" || rp.Project != "acme/web" || rp.ForgeURL != gh.URL || rp.WebURL != gh.URL+"/acme/web" {
		t.Fatalf("repository %+v, %v", rp, err)
	}
	c, err := f.store.Commit(context.Background(), "web", rp.Head)
	if err != nil || len(c.Tree) != 6 {
		t.Fatalf("commit %+v, %v", c, err)
	}
	// The API and the UI's data show it as any other repository.
	if r := f.do("GET", "/v1/repos/web", f.token(nil), nil); r.status != http.StatusOK || !strings.Contains(string(r.body), `"kind":"github"`) || strings.Contains(string(r.body), githubToken) {
		t.Fatalf("GET /v1/repos/web: %d %s", r.status, r.body)
	}
}

// TestGitHubHook checks GitHub deliveries: a signed push reads the head
// at once, a ping passes and reads nothing, a retry of a delivery is
// dropped, and a delivery with no signature, a wrong one, or one over
// another body is refused with the audit row's check.
func TestGitHubHook(t *testing.T) {
	f, gh := githubFixture(t, hookSecret)
	push := hostilePush(strings.Repeat("a", 40))

	if r := f.githubHook(githubHookPath, hookSecret, "ping", "11111111-0000-4000-8000-000000000000", []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)); r.status != http.StatusOK || r.doc["ok"] != true || r.doc["queued"] != false {
		t.Fatalf("ping: %d %s", r.status, r.body)
	}
	files := infraFiles()
	files["infra/network/main.tf"] = forgetest.File{Content: files["infra/network/main.tf"].Content + "# moved\n"}
	next := gh.Commit(777001, "main", files)
	r := f.githubHook(githubHookPath, hookSecret, "push", "22222222-0000-4000-8000-000000000000", push)
	if r.status != http.StatusAccepted || r.doc["queued"] != true || r.doc["repo"] != "web" {
		t.Fatalf("push: %d %s", r.status, r.body)
	}
	if got := f.waitHead("web", next); got.HookAt.IsZero() {
		t.Fatalf("no hook time recorded: %+v", got)
	}
	for _, c := range gh.Calls() {
		if !strings.Contains(c, "/repositories/777001/") || strings.Contains(c, "evil") {
			t.Errorf("the delivery led to the call %s", c)
		}
	}
	rows := f.audit(controltest.AuditFilter{Request: r.header.Get("X-Request-Id")})
	if len(rows) != 1 || rows[0].Op != "hook" || rows[0].Decision != control.Allowed || rows[0].Principal != "forge github" || !strings.Contains(rows[0].Reason, "repository web") {
		t.Fatalf("audit rows %+v", rows)
	}

	// A captured ping, signed, sent again as a push, reads nothing: the
	// signed body is a ping's.
	if r := f.githubHook(githubHookPath, hookSecret, "push", "33333333-0000-4000-8000-000000000000", []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)); r.status != http.StatusOK || r.doc["queued"] != false {
		t.Fatalf("a ping's body as a push: %d %s", r.status, r.body)
	}

	// GitHub sends a delivery again with the same X-GitHub-Delivery.
	if r := f.githubHook(githubHookPath, hookSecret, "push", "22222222-0000-4000-8000-000000000000", push); r.status != http.StatusOK || r.doc["duplicate"] != true || r.doc["queued"] != false {
		t.Fatalf("a replayed delivery: %d %s", r.status, r.body)
	}

	for _, c := range []struct {
		name, secret, check string
		hdr                 []string
	}{
		{"no signature", "", "hook-no-signature", nil},
		{"a wrong secret", "not-the-secret", "hook-signature", nil},
		{"the SHA-1 signature only", "", "hook-no-signature", []string{"X-Hub-Signature", "sha1=0000000000000000000000000000000000000000"}},
		{"GitLab's token header", "", "hook-no-signature", []string{"X-Gitlab-Token", hookSecret}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := f.githubHook(githubHookPath, c.secret, "push", "", push, c.hdr...)
			if r.status != http.StatusUnauthorized || r.class() != "auth" || strings.Contains(string(r.body), hookSecret) {
				t.Fatalf("%d %s", r.status, r.body)
			}
			rows := f.audit(controltest.AuditFilter{Request: r.header.Get("X-Request-Id")})
			if len(rows) != 1 || rows[0].Decision != control.AuthFailed || rows[0].Check != c.check || rows[0].Principal != "" {
				t.Fatalf("audit rows %+v", rows)
			}
		})
	}
	// A signature over another body is refused: the signature covers the
	// raw body, byte for byte.
	mac := hmac.New(sha256.New, []byte(hookSecret))
	mac.Write(push)
	tampered := append(bytes.Clone(push), ' ')
	if r := f.githubHook(githubHookPath, "", "push", "", tampered, "X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil))); r.status != http.StatusUnauthorized {
		t.Fatalf("a signature over another body: %d %s", r.status, r.body)
	}
	time.Sleep(200 * time.Millisecond)
	if calls := gh.Calls(); len(calls) != 0 {
		t.Fatalf("a refused or repeated delivery led to the calls %v", calls)
	}
}
