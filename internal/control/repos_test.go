package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
)

func TestRepos(t *testing.T) { each(t, testRepos) }

func testRepos(t *testing.T, s *control.Store, loc string) {
	ctx := context.Background()
	cfg, err := s.RecordConfig(ctx, "{}", "https://writ.example.com", "test", policy(), t0)
	if err != nil {
		t.Fatal(err)
	}
	infra := control.Repo{
		Name: "infra", Forge: "gitlab", Kind: control.GitLab, ForgeURL: "https://gitlab.example.com", Project: "acme/infra", ProjectID: "4242",
		Path: "infra", DefaultBranch: "main", WebURL: "https://gitlab.example.com/acme/infra", Origin: control.FromAPI, AddedBy: "gitlab.example.com/user/1", AddedAt: t0,
	}
	head := control.Commit{SHA: "5d1e9a0c7b2f4e1d8a6c3b9e0f7a2d4c6b8e1f3a", ReadAt: t0, Tree: []control.TreeEntry{{Path: "network/main.writ", Blob: "b1"}},
		Files: 1, Bytes: 10, Stacks: json.RawMessage(`[{"stack":"network"}]`), Diagnostics: json.RawMessage(`[]`)}
	e := control.Entry{At: t0, Request: "add", Kind: "request", Decision: control.Allowed, Op: "admin", ConfigID: cfg}
	if err := s.AddRepo(ctx, infra, head, e, nil); err != nil {
		t.Fatal(err)
	}
	// The same name, or the same project and path under another name, is
	// refused, and nothing is stored.
	var conflict *control.RepoConflict
	e.Request = "again"
	if err := s.AddRepo(ctx, infra, head, e, nil); !errors.As(err, &conflict) || !conflict.SameName {
		t.Fatalf("same name: %v", err)
	}
	other := infra
	other.Name = "infra2"
	if err := s.AddRepo(ctx, other, head, e, nil); !errors.As(err, &conflict) || conflict.SameName || conflict.Name != "infra" {
		t.Fatalf("same project and path: %v", err)
	}
	if rows := controltest.Audit(t, loc, controltest.AuditFilter{Request: "again"}); len(rows) != 0 {
		t.Fatal("a refused add left an audit row")
	}
	// A second estate of the project is a second repository.
	other.Path = "platform"
	if err := s.AddRepo(ctx, other, control.Commit{SHA: head.SHA, ReadAt: t0}, control.Entry{At: t0, Request: "add2", Kind: "request", Decision: control.Allowed}, nil); err != nil {
		t.Fatal(err)
	}
	of, err := s.ReposOfProject(ctx, control.GitLab, "https://gitlab.example.com", "4242")
	if err != nil || len(of) != 2 || of[0].Name != "infra" || of[1].Name != "infra2" {
		t.Fatalf("repos of the project %+v, %v", of, err)
	}
	if none, _ := s.ReposOfProject(ctx, control.GitLab, "https://gitlab.com", "4242"); len(none) != 0 {
		t.Fatalf("a project id of another forge matched: %+v", none)
	}

	got, err := s.Repo(ctx, "infra")
	if err != nil || got.Head != head.SHA || got.ProjectID != "4242" || !got.HeadReadAt.Equal(t0) || got.Error != "" {
		t.Fatalf("repo %+v, %v", got, err)
	}
	c, err := s.Commit(ctx, "infra", head.SHA)
	if err != nil || len(c.Tree) != 1 || c.Tree[0].Blob != "b1" || string(c.Stacks) != `[{"stack":"network"}]` {
		t.Fatalf("commit %+v, %v", c, err)
	}
	if _, err := s.Commit(ctx, "infra", "0000000000000000000000000000000000000000"); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("an unread commit: %v", err)
	}

	// A failed poll keeps the head and says why; the next read clears it.
	if err := s.RecordPoll(ctx, "infra", t0.Add(time.Minute), "the forge answered 401"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Repo(ctx, "infra"); got.Error != "the forge answered 401" || got.Head != head.SHA {
		t.Fatalf("after a failed poll %+v", got)
	}
	next := control.Commit{Repo: "infra", SHA: "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432", ReadAt: t0.Add(2 * time.Minute)}
	if err := s.RecordHead(ctx, next, t0.Add(2*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Repo(ctx, "infra"); got.Error != "" || got.Head != next.SHA {
		t.Fatalf("after a new head %+v", got)
	}

	// A webhook delivery's time is kept, and changes nothing else.
	if !got.HookAt.IsZero() {
		t.Fatalf("a repository no hook reached has a hook time %v", got.HookAt)
	}
	if err := s.RecordHook(ctx, "infra", t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Repo(ctx, "infra"); !got.HookAt.Equal(t0.Add(3*time.Minute)) || got.Head != next.SHA || !got.PolledAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("after a hook %+v", got)
	}
	if err := s.RecordHook(ctx, "nope", t0); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a hook of no repository: %v", err)
	}

	// Blobs are stored once by id.
	if err := s.PutBlob(ctx, "b1", []byte("resource"), t0); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlob(ctx, "b1", []byte("other"), t0); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlob(ctx, "empty", nil, t0); err != nil {
		t.Fatal(err)
	}
	blobs, err := s.Blobs(ctx, []string{"b1", "b2", "empty"})
	if _, has := blobs["b2"]; err != nil || string(blobs["b1"]) != "resource" || blobs["empty"] == nil || has {
		t.Fatalf("blobs %q, %v", blobs, err)
	}

	// A plan names the commit it was made from, and its digest is set
	// when the commit is read.
	p := newPlan("REF1", cfg, "prod", "network", t0)
	p.SourceDigest, p.Source = "", control.PlanSource{Repo: "infra", Ref: "refs/heads/main", Commit: next.SHA}
	if err := s.CreatePlan(ctx, p, control.TokenUse{}, control.Entry{At: t0, Request: "plan", Kind: "request", Decision: control.Allowed}); err != nil {
		t.Fatal(err)
	}
	res := control.PlanResult{Status: "refused", MadeAt: t0, Document: json.RawMessage(`{}`), SourceDigest: "abc"}
	if err := s.FinishPlan(ctx, "REF1", res, control.Entry{At: t0, Request: "REF1", Kind: "plan-finished", Decision: control.NoDecision}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Plan(ctx, "REF1", t0); err != nil || got.Source != p.Source || got.SourceDigest != "abc" {
		t.Fatalf("plan %+v, %v", got, err)
	}

	if err := s.DeleteRepo(ctx, "infra", control.Entry{At: t0, Request: "del", Kind: "repo-removed", Decision: control.NoDecision}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, "infra", head.SHA); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a removed repository's commit: %v", err)
	}
	if err := s.DeleteRepo(ctx, "infra", control.Entry{At: t0, Request: "del2", Kind: "repo-removed", Decision: control.NoDecision}); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("removed twice: %v", err)
	}
	if blobs, _ := s.Blobs(ctx, []string{"b1"}); blobs["b1"] == nil {
		t.Fatal("removing a repository removed a blob")
	}
}

func TestRepoCheckIsSerialized(t *testing.T) { each(t, testRepoCheckIsSerialized) }

// Two requests that each pass a check made before the write could both be
// stored. The check runs inside the write, so with eight adds at once
// exactly one passes a rule that allows one repository, and it sees the
// stacks of the head commit of each repository stored before it.
func testRepoCheckIsSerialized(t *testing.T, s *control.Store, _ string) {
	ctx := context.Background()
	cfg, err := s.RecordConfig(ctx, "{}", "https://writ.example.com", "test", policy(), t0)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("the rule allows one repository")
	var seen [][]control.RepoStacks
	var mu sync.Mutex
	check := func(self control.RepoStacks, others []control.RepoStacks) error {
		mu.Lock()
		seen = append(seen, others)
		mu.Unlock()
		// Hold the check, so that every add is inside its check at once
		// unless the store keeps them apart.
		time.Sleep(30 * time.Millisecond)
		if len(others) > 0 {
			return refused
		}
		return nil
	}
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			name := fmt.Sprintf("repo%d", i)
			r := control.Repo{Name: name, Forge: "gitlab", Kind: control.GitLab, ForgeURL: "https://gitlab.example.com", Project: "acme/" + name, ProjectID: fmt.Sprint(100 + i),
				DefaultBranch: "main", Origin: control.FromAPI, AddedBy: "test", AddedAt: t0}
			c := control.Commit{SHA: fmt.Sprintf("%040d", i), ReadAt: t0, Stacks: json.RawMessage(`[{"dir":"network"}]`)}
			e := control.Entry{At: t0, Request: "add-" + name, Kind: "request", Decision: control.Allowed, Op: "admin", ConfigID: cfg}
			errs[i] = s.AddRepo(ctx, r, c, e, check)
		})
	}
	wg.Wait()
	passed := 0
	for _, err := range errs {
		switch {
		case err == nil:
			passed++
		case !errors.Is(err, refused):
			t.Errorf("an add failed for another reason: %v", err)
		}
	}
	if repos, err := s.Repos(ctx); passed != 1 || err != nil || len(repos) != 1 {
		t.Fatalf("%d adds passed and %d repositories are stored, want one of each: %v", passed, len(repos), err)
	}
	var withStacks int
	for _, others := range seen {
		for _, o := range others {
			if string(o.Stacks) == `[{"dir":"network"}]` {
				withStacks++
			}
		}
	}
	if withStacks == 0 {
		t.Error("no check saw the stacks of a stored repository")
	}
}
