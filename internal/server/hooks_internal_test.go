package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
)

func TestVerifyGitLab(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := []byte("0123456789abcdef0123456789abcdef")
	signing := []byte("whsec_" + base64.StdEncoding.EncodeToString(key))
	body := []byte(`{"object_kind":"push"}`)
	sig := func(id string, ts int64, body []byte) string {
		mac := hmac.New(sha256.New, key)
		fmt.Fprintf(mac, "%s.%d.", id, ts)
		mac.Write(body)
		return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	signed := func(id string, ts int64, s string) http.Header {
		h := http.Header{}
		h.Set("webhook-id", id)
		h.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
		h.Set("webhook-signature", s)
		return h
	}
	tok := func(v string) http.Header {
		h := http.Header{}
		h.Set("X-Gitlab-Token", v)
		return h
	}
	good := sig("msg_1", now.Unix(), body)
	for _, c := range []struct {
		name   string
		secret []byte
		h      http.Header
		body   []byte
		check  string
	}{
		{"token", []byte("s3cret"), tok("s3cret"), body, ""},
		{"wrong token", []byte("s3cret"), tok("s3creT"), body, "hook-token"},
		{"longer token", []byte("s3cret"), tok("s3cret-and-more"), body, "hook-token"},
		{"no token", []byte("s3cret"), http.Header{}, body, "hook-no-token"},
		{"signed", signing, signed("msg_1", now.Unix(), good), body, ""},
		{"one of two signatures", signing, signed("msg_1", now.Unix(), "v1,bm9wZQ== "+good), body, ""},
		{"skew allowed", signing, signed("msg_1", now.Unix()-299, sig("msg_1", now.Unix()-299, body)), body, ""},
		{"another body", signing, signed("msg_1", now.Unix(), good), append(body, ' '), "hook-signature"},
		{"another id", signing, signed("msg_2", now.Unix(), good), body, "hook-signature"},
		{"not v1", signing, signed("msg_1", now.Unix(), "v2"+good[2:]), body, "hook-signature"},
		{"old", signing, signed("msg_1", now.Unix()-301, sig("msg_1", now.Unix()-301, body)), body, "hook-timestamp"},
		{"future", signing, signed("msg_1", now.Unix()+301, sig("msg_1", now.Unix()+301, body)), body, "hook-timestamp"},
		{"timestamp not a number", signing, signed("msg_1", now.Unix(), good), body, "hook-timestamp"},
		{"token header under a signing token", signing, tok(string(signing)), body, "hook-no-signature"},
		{"signing token not base64", []byte("whsec_!!!"), signed("msg_1", now.Unix(), good), body, "hook-secret-file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "timestamp not a number" {
				c.h.Set("webhook-timestamp", "soon")
			}
			ok, check := verifyGitLab(c.h, c.body, c.secret, now)
			if ok != (c.check == "") || check != c.check {
				t.Fatalf("got %v %q, want %q", ok, check, c.check)
			}
		})
	}
}

// TestHookProofsCompareInConstantTime checks that hooks.go compares a
// proof a delivery sent only through secretEqual, and that secretEqual
// compares SHA-256 digests, which have one length, with
// subtle.ConstantTimeCompare, so the time a compare takes says nothing of
// the secret, its length included.
func TestHookProofsCompareInConstantTime(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "hooks.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		seen := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				switch x := c.Fun.(type) {
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok {
						seen[id.Name+"."+x.Sel.Name] = true
					}
				case *ast.Ident:
					seen[x.Name] = true
				}
			}
			return true
		})
		calls[fn.Name.Name] = seen
	}
	if c := calls["secretEqual"]; !c["subtle.ConstantTimeCompare"] || !c["sha256.Sum256"] {
		t.Fatalf("secretEqual calls %v", c)
	}
	for _, fn := range []string{"verifyGitLab", "verifySigned"} {
		if !calls[fn]["secretEqual"] {
			t.Errorf("%s does not compare through secretEqual", fn)
		}
	}
	for fn, c := range calls {
		for _, eq := range []string{"bytes.Equal", "hmac.Equal", "strings.EqualFold", "subtle.ConstantTimeCompare", "slices.Equal"} {
			if c[eq] && !(fn == "secretEqual" && eq == "subtle.ConstantTimeCompare") {
				t.Errorf("%s compares with %s", fn, eq)
			}
		}
	}
	for _, x := range [][2]string{{"a", "a"}, {"", ""}, {"a", "b"}, {"a", "ab"}} {
		if secretEqual([]byte(x[0]), []byte(x[1])) != (x[0] == x[1]) {
			t.Errorf("secretEqual(%q, %q)", x[0], x[1])
		}
	}
}

// A repository with a webhook is due every PollEveryWithHook, within half
// a poll; one without, at every poll.
func TestPollDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Server{now: func() time.Time { return now }, cfg: &Config{
		PollEvery: time.Minute, PollEveryWithHook: 5 * time.Minute, PinRepos: true,
		Forges: []Forge{{Name: "gitlab", Kind: control.GitLab, URL: "https://gitlab.example.com"}},
		Repos:  []RepoSpec{{Name: "infra", Forge: "gitlab", WebhookSecretFile: "/run/secrets/hook"}, {Name: "apps", Forge: "gitlab"}},
	}}
	for _, c := range []struct {
		name, origin string
		ago          time.Duration
		due          bool
	}{
		{"infra", control.FromConfig, 0, true},
		{"infra", control.FromConfig, time.Minute, false},
		{"infra", control.FromConfig, 4 * time.Minute, false},
		{"infra", control.FromConfig, 4*time.Minute + 30*time.Second, true},
		{"infra", control.FromConfig, 5 * time.Minute, true},
		{"apps", control.FromConfig, time.Second, true},
		{"infra", control.FromAPI, time.Second, true},
	} {
		r := control.Repo{Name: c.name, Forge: "gitlab", Origin: c.origin}
		if c.ago > 0 {
			r.PolledAt = now.Add(-c.ago)
		}
		if got := s.pollDue(r); got != c.due {
			t.Errorf("%s from %s polled %s ago: due %v, want %v", c.name, c.origin, c.ago, got, c.due)
		}
	}
}

// The delivery ids are a bounded set: the oldest goes first, and a
// forgotten id does not take another's place.
func TestHookDeliveries(t *testing.T) {
	h := newHookState(time.Now)
	if !h.first("a") || h.first("a") {
		t.Fatal("a delivery id is not dropped the second time")
	}
	h.forget("a")
	if !h.first("a") {
		t.Fatal("a forgotten id is dropped")
	}
	for i := range hookDeliveries - 1 {
		h.first(strconv.Itoa(i))
	}
	if len(h.seen) != hookDeliveries || h.first("a") {
		t.Fatalf("%d ids kept", len(h.seen))
	}
	if !h.first("new") || len(h.seen) != hookDeliveries {
		t.Fatalf("%d ids kept after one more", len(h.seen))
	}
	// "a" was the oldest, so it went, and "0" is still there.
	if h.first("0") || !h.first("a") {
		t.Fatal("not the oldest id went")
	}
	h.queue("infra")
	h.queue("infra")
	if got := h.take(); len(got) != 1 || got[0] != "infra" || len(h.take()) != 0 {
		t.Fatalf("queued %v", got)
	}
}
