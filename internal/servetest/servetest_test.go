package servetest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/cli"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/servetest"
)

// The CLI finds a test control plane the way it finds writ serve --dev,
// and reads the runs of the environment dev from it.
func TestRemoteCommandsFindTheDevControlPlane(t *testing.T) {
	srv := servetest.Start(t, servetest.Options{})
	ctx := context.Background()
	var out bytes.Buffer
	app := cli.New(srv.Client(cli.Options{Dir: t.TempDir(), Stdout: &out, Stderr: &out}, nil))
	if err := app.Whoami(ctx, false); err != nil || !strings.HasPrefix(out.String(), srv.URL) || !strings.Contains(out.String(), servetest.User+" (dev)") {
		t.Fatalf("whoami: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := app.RemoteRuns(ctx, cli.RemoteOptions{}, cli.DefaultRunsLimit, false); err != nil || out.String() != "writ serve has started no run in dev.\n" {
		t.Fatalf("runs: %v\n%s", err, out.String())
	}
}

// Each answer of writ serve waits for its audit row, which waits for every
// other write to the control database. On a busy disk a commit takes
// seconds, as another writer's commit holds the database here. The CLI
// still finds the dev control plane, and still refuses at once when none
// listens.
func TestRemoteCommandsWaitForABusyDevControlPlane(t *testing.T) {
	srv := servetest.Start(t, servetest.Options{})
	ctx := context.Background()
	raw := controltest.Raw(t, filepath.Join(srv.Data, "control.db"))
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	held := time.AfterFunc(3*time.Second, func() { tx.Rollback() })
	defer held.Stop()
	var out bytes.Buffer
	app := cli.New(srv.Client(cli.Options{Dir: t.TempDir(), Stdout: &out, Stderr: &out}, nil))
	if err := app.Whoami(ctx, false); err != nil || !strings.Contains(out.String(), servetest.User+" (dev)") {
		t.Fatalf("whoami while another write holds the control database: %v\n%s", err, out.String())
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := "http://" + ln.Addr().String()
	ln.Close()
	if err := cli.WriteDevClient(srv.Data, stopped, srv.Token, servetest.User); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	start := time.Now()
	err = app.Whoami(ctx, false)
	if err == nil || !strings.Contains(err.Error(), "does not answer at "+stopped+": start it with writ serve --dev") {
		t.Fatalf("whoami with no dev control plane: %v\n%s", err, out.String())
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the refusal took %s, want it at once", took)
	}
}

// The dev control plane takes its own token and no other, and no request
// without one reaches a route that needs it.
func TestDevTokenOnly(t *testing.T) {
	srv := servetest.Start(t, servetest.Options{})
	for _, tok := range []string{"", "writd_" + strings.Repeat("0", 64), srv.Token + "x"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: %d", tok, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var who server.Whoami
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil || resp.StatusCode != http.StatusOK || who.Principal.Kind != "dev" || who.Principal.Person != "dev/"+servetest.User {
		t.Fatalf("whoami %d %+v: %v", resp.StatusCode, who, err)
	}
}

// BenchmarkStart measures a test control plane from nothing to its first
// answer: a new SQLite control database, every migration, the server, and
// GET /v1/server.
func BenchmarkStart(b *testing.B) {
	for b.Loop() {
		srv := servetest.Start(b, servetest.Options{})
		resp, err := http.Get(srv.URL + "/v1/server")
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Customer A", "GIT_AUTHOR_EMAIL=user@example.com", "GIT_COMMITTER_NAME=Customer A",
		"GIT_COMMITTER_EMAIL=user@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// One person adds their Git repository to the dev control plane, plans
// the commit they are on, and, with an edit, a speculative plan of the
// same stack.
func TestRepositoryAndCommitPlans(t *testing.T) {
	srv := servetest.Start(t, servetest.Options{})
	ctx := context.Background()
	dir := tofuGitInit(t)
	head := git(t, dir, "rev-parse", "HEAD")
	var out bytes.Buffer
	app := cli.New(srv.Client(cli.Options{Dir: filepath.Join(dir, "hello"), Stdout: &out, Stderr: &out}, nil))
	if err := app.RepoAdd(ctx, cli.RepoAddOptions{Location: dir}); err != nil {
		t.Fatalf("repo add: %v\n%s", err, out.String())
	}
	// The test control plane runs no OpenTofu, so it serves the stack in
	// no environment.
	for _, want := range []string{"Repository demo: " + dir + ", path the root, branch main.", "Read main at " + head[:8] + " from the local repository: 1 file, 1 stack.", "hello  - (not served: dev)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("repo add lacks %q:\n%s", want, out.String())
		}
	}

	// The test control plane runs no OpenTofu, so the plan of the commit
	// fails, and it is of the commit.
	out.Reset()
	if err := app.RemotePlan(ctx, cli.RemotePlanOptions{JSON: true}); !errors.Is(err, cli.ErrFailed) {
		t.Fatalf("plan: %v\n%s", err, out.String())
	}
	var doc server.RemotePlan
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Plan == nil {
		t.Fatalf("plan document %s: %v", out.String(), err)
	}
	if p := doc.Plan; p.Status != "failed" || p.Stacks[0].Stack != "hello" || p.Source != (server.PlanSourceInfo{Kind: "commit", Repo: "demo", Ref: "refs/heads/main", Commit: head, RefCheck: "on-ref"}) {
		t.Fatalf("a commit plan %+v", p)
	}

	// An edit leaves no commit to plan.
	if err := os.WriteFile(filepath.Join(dir, "hello", "main.tf"), []byte("resource \"random_pet\" \"other\" {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := app.RemotePlan(ctx, cli.RemotePlanOptions{}); err == nil || !strings.Contains(err.Error(), "writ serve plans only by commit") || !strings.Contains(err.Error(), "differs from commit "+head[:8]) {
		t.Fatalf("a plan of an edit: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := app.RepoList(ctx, false); err != nil || !strings.Contains(out.String(), "demo  "+dir) || !strings.Contains(out.String(), head[:8]) {
		t.Fatalf("repo list: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := app.UI(ctx, false); err != nil || !strings.Contains(out.String(), srv.URL+"/ui/login/dev?code=") {
		t.Fatalf("ui: %v\n%s", err, out.String())
	}
}
