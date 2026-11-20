package servetest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mr-karan/writ/internal/cli"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/servetest"
)

// tofuGitInit makes a Git repository demo with one OpenTofu stack, hello,
// and its lock, and returns its directory.
func tofuGitInit(t *testing.T) string {
	t.Helper()
	lock, err := os.ReadFile(filepath.Join("..", "tofu", "testdata", "lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "demo")
	for name, text := range map[string]string{"hello/main.tf": "resource \"random_pet\" \"name\" {}\n", "hello/.terraform.lock.hcl": string(lock)} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "hello")
	return dir
}

// serveMCP runs writ mcp on dir against srv and returns a client session.
func serveMCP(t *testing.T, ctx context.Context, srv *servetest.Server, dir string) *sdk.ClientSession {
	t.Helper()
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	app := cli.New(srv.Client(cli.Options{Dir: dir, Stdin: toServer, Stdout: fromServer}, nil))
	done := make(chan error, 1)
	go func() {
		done <- app.MCP(ctx, cli.MCPOptions{})
		fromServer.Close()
	}()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &sdk.IOTransport{Reader: toClient, Writer: fromClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		fromClient.Close()
		if err := <-done; err != nil && ctx.Err() == nil {
			t.Errorf("writ mcp: %v", err)
		}
	})
	return cs
}

func call(t *testing.T, ctx context.Context, cs *sdk.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*sdk.TextContent).Text, res.IsError
}

// The MCP plan tool plans the commit of a repository the control plane
// has, and refuses a stack whose files differ from it. git runs no program
// the repository's config or attributes name, as it would if it read the
// work tree. This control plane runs no OpenTofu, so the plan it keeps
// failed, and get_plan reads it back.
func TestMCPPlansTheCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv := servetest.Start(t, servetest.Options{})
	dir := tofuGitInit(t)
	head := git(t, dir, "rev-parse", "HEAD")
	var out bytes.Buffer
	if err := cli.New(srv.Client(cli.Options{Dir: filepath.Join(dir, "hello"), Stdout: &out, Stderr: &out}, nil)).RepoAdd(ctx, cli.RepoAddOptions{Location: dir}); err != nil {
		t.Fatalf("repo add: %v\n%s", err, &out)
	}
	marker := filepath.Join(t.TempDir(), "MARKER")
	git(t, dir, "config", "core.fsmonitor", "touch "+marker+"; false")
	git(t, dir, "config", "filter.x.clean", "touch "+marker+"; cat")
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("*.tf filter=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "hello", "main.tf"), future, future); err != nil {
		t.Fatal(err)
	}
	cs := serveMCP(t, ctx, srv, dir)

	text, isError := call(t, ctx, cs, "plan", map[string]any{"dir": "hello", "json": true})
	var doc server.RemotePlan
	if json.Unmarshal([]byte(text), &doc) != nil || doc.Plan == nil {
		t.Fatalf("plan: isError %v:\n%s", isError, text)
	}
	if p := doc.Plan; !isError || p.Status != "failed" || p.Source.Kind != "commit" || p.Source.Commit != head || p.Source.Repo != "demo" || !strings.Contains(doc.Text, "runs no OpenTofu") {
		t.Fatalf("want a failed plan of commit %s: isError %v, %+v\n%s", head, isError, p, doc.Text)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git ran a program the repository names")
	}
	if text, isError := call(t, ctx, cs, "get_plan", map[string]any{"id": doc.Plan.Ref}); !isError || !strings.Contains(text, "is failed.") {
		t.Fatalf("get_plan: isError %v:\n%s", isError, text)
	}
	// Make a second plan so list_plans can filter and page across two plans.
	text2, _ := call(t, ctx, cs, "plan", map[string]any{"dir": "hello", "json": true})
	var doc2 server.RemotePlan
	if json.Unmarshal([]byte(text2), &doc2) != nil || doc2.Plan == nil {
		t.Fatalf("second plan:\n%s", text2)
	}
	page1Text, isError := call(t, ctx, cs, "list_plans", map[string]any{"stack": "hello", "status": "failed", "started_by": "person", "limit": 1})
	var page1 server.RemotePlans
	if isError || json.Unmarshal([]byte(page1Text), &page1) != nil || page1.Format != server.FormatRemotePlans || len(page1.Plans) != 1 || page1.Plans[0].Ref != doc2.Plan.Ref || page1.Next == "" {
		t.Fatalf("list_plans page 1: isError %v\n%s", isError, page1Text)
	}
	page2Text, isError := call(t, ctx, cs, "list_plans", map[string]any{"stack": "hello", "status": "failed", "started_by": "person", "limit": 1, "cursor": page1.Next})
	var page2 server.RemotePlans
	if isError || json.Unmarshal([]byte(page2Text), &page2) != nil || len(page2.Plans) != 1 || page2.Plans[0].Ref != doc.Plan.Ref || page2.Next != "" {
		t.Fatalf("list_plans page 2: isError %v\n%s", isError, page2Text)
	}
	if text, isError := call(t, ctx, cs, "list_plans", map[string]any{"started_by": "drift"}); !isError || !strings.Contains(text, "GET /v1/drift") {
		t.Fatalf("list_plans started_by=drift: isError %v\n%s", isError, text)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello", "main.tf"), []byte("resource \"random_pet\" \"other\" {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if text, isError := call(t, ctx, cs, "plan", map[string]any{"dir": "hello"}); !isError || !strings.Contains(text, "writ serve plans only by commit") {
		t.Fatalf("an edit was planned: isError %v:\n%s", isError, text)
	}
}

// An assistant can, with file writes in the project alone, commit an edit,
// remove the commit's blob, and make the repository a partial clone whose
// promisor remote's fetch runs a program. The MCP plan tool's commit plan
// makes writ serve --dev read that blob, and its git must not fetch it.
func TestMCPPlanDevServerRunsNoFetchProgram(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv := servetest.Start(t, servetest.Options{})
	dir := tofuGitInit(t)
	var out bytes.Buffer
	if err := cli.New(srv.Client(cli.Options{Dir: filepath.Join(dir, "hello"), Stdout: &out, Stderr: &out}, nil)).RepoAdd(ctx, cli.RepoAddOptions{Location: dir}); err != nil {
		t.Fatalf("repo add: %v\n%s", err, &out)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello", "main.tf"), []byte("resource \"random_pet\" \"again\" {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-qam", "again")
	blob := git(t, dir, "rev-parse", "HEAD:hello/main.tf")
	if err := os.Remove(filepath.Join(dir, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	marker, script := filepath.Join(base, "MARKER"), filepath.Join(base, "planted.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "remote.origin.promisor", "true")
	git(t, dir, "config", "remote.origin.url", filepath.Join(base, "none"))
	git(t, dir, "config", "remote.origin.uploadpack", script)
	cs := serveMCP(t, ctx, srv, dir)

	text, _ := call(t, ctx, cs, "plan", map[string]any{"dir": "hello"})
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("git in writ serve --dev ran a program the project's config names:\n%s", text)
	}
	if !strings.Contains(text, "hello/main.tf") {
		t.Fatalf("the plan did not read the commit's blob, so this test shows nothing:\n%s", text)
	}
}

// The read tools reach the dev control plane with its token, and default
// the environment to dev.
func TestMCPReadsTheDevControlPlane(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	srv := servetest.Start(t, servetest.Options{})
	cs := serveMCP(t, ctx, srv, t.TempDir())
	for tool, want := range map[string]string{
		"drift":      `{"format":"writ.drift/v0","ok":true,"stacks":[]`,
		"runs":       `{"format":"writ.remote-runs/v0","ok":true,"runs":[]`,
		"list_plans": `{"format":"writ.remote-plans/v0","ok":true,"plans":[]`,
	} {
		if text, isError := call(t, ctx, cs, tool, map[string]any{}); isError || !strings.HasPrefix(text, want) {
			t.Errorf("%s: isError %v: %s", tool, isError, text)
		}
	}
	text, isError := call(t, ctx, cs, "state_index", map[string]any{"stack": "app"})
	if !isError || !strings.Contains(text, `"format":"writ.state-index/v0"`) || !strings.Contains(text, "the state index holds nothing of app@dev") {
		t.Errorf("state_index: isError %v: %s", isError, text)
	}
}
