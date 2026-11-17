package repo_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/repo"
)

const token = "glpat-repotest-0123456789"

var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// lockOf is a .terraform.lock.hcl that pins hashicorp/random at version.
func lockOf(version string) string {
	return "provider \"registry.opentofu.org/hashicorp/random\" {\n  version = \"" + version + "\"\n  hashes  = [\"h1:" + strings.Repeat("ab", 22) + "=\"]\n}\n"
}

// estate is a repository whose path infra holds three OpenTofu stacks,
// each with a lock file of its own, and a module.
func estate() map[string]forgetest.File {
	return map[string]forgetest.File{
		"README.md":                          {Content: "not read\n"},
		"infra/modules/cert/main.tf":         {Content: "resource \"random_id\" \"i\" {}\n"},
		"infra/network/main.tf":              {Content: "module \"cert\" {\n  source = \"../modules/cert\"\n}\n"},
		"infra/network/.terraform.lock.hcl":  {Content: lockOf("3.9.1")},
		"infra/database/main.tf":             {Content: "resource \"random_pet\" \"p\" {}\n"},
		"infra/database/.terraform.lock.hcl": {Content: lockOf("3.9.0")},
		"infra/app/main.tf":                  {Content: "resource \"random_pet\" \"a\" {}\n"},
		"infra/app/.terraform.lock.hcl":      {Content: lockOf("3.8.1")},
	}
}

func setup(t *testing.T) (*forgetest.GitLab, *forge.GitLab, *control.Store) {
	t.Helper()
	g := forgetest.New(t, token)
	g.AddProject(4242, "acme/infra", "main")
	tf := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tf, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := forge.NewGitLab(forge.GitLabOptions{URL: g.URL, TokenFile: tf})
	if err != nil {
		t.Fatal(err)
	}
	st, err := control.Open(context.Background(), control.Options{Location: filepath.Join(t.TempDir(), "control.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return g, c, st
}

func blobCalls(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.HasSuffix(c, "/raw") {
			n++
		}
	}
	return n
}

func TestReadFetchesOnlyNewFiles(t *testing.T) {
	ctx := context.Background()
	g, c, st := setup(t)
	files := estate()
	first := g.Commit(4242, "main", files)
	g.Calls()
	snap, err := repo.Read(ctx, c, "4242", first, "infra", nil, st, repo.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range snap.Files {
		names = append(names, n)
	}
	slices.Sort(names)
	want := []string{"app/.terraform.lock.hcl", "database/.terraform.lock.hcl", "network/.terraform.lock.hcl"}
	if !slices.Equal(names, want) {
		t.Fatalf("files read:\n got %v\nwant %v", names, want)
	}
	if n := blobCalls(g.Calls()); n != 3 || snap.Fetched != 3 {
		t.Fatalf("fetched %d blobs, counted %d", n, snap.Fetched)
	}
	if err := snap.Keep(ctx, st, now); err != nil {
		t.Fatal(err)
	}

	// A second commit that changes one file fetches that file alone.
	files["infra/network/.terraform.lock.hcl"] = forgetest.File{Content: lockOf("3.9.2")}
	second := g.Commit(4242, "main", files)
	snap, err = repo.Read(ctx, c, "4242", second, "infra", nil, st, repo.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	calls := g.Calls()
	if n := blobCalls(calls); n != 1 || snap.Fetched != 1 || !strings.Contains(string(snap.Files["network/.terraform.lock.hcl"]), "3.9.2") {
		t.Fatalf("the second commit fetched %d blobs: %v", n, calls)
	}
	if err := snap.Keep(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	// With the tree it kept, a commit read again asks the forge nothing.
	if _, err := repo.Read(ctx, c, "4242", second, "infra", snap.Tree, st, repo.DefaultLimits); err != nil {
		t.Fatal(err)
	}
	if calls := g.Calls(); len(calls) != 0 {
		t.Fatalf("a stored commit called the forge: %v", calls)
	}
}

// A commit that holds .writ files, the code of Writ's retired language, is
// refused with the files named, and nothing of it is fetched.
func TestReadRefusesWritFiles(t *testing.T) {
	ctx := context.Background()
	g, c, st := setup(t)
	files := estate()
	for i := range 7 {
		files[fmt.Sprintf("infra/old%d/main.writ", i)] = forgetest.File{Content: "let x = 1\n"}
	}
	sha := g.Commit(4242, "main", files)
	g.Calls()
	_, err := repo.Read(ctx, c, "4242", sha, "infra", nil, st, repo.DefaultLimits)
	want := "holds .writ files, the code of Writ's retired language, which writ serve no longer plans: old0/main.writ, old1/main.writ, old2/main.writ, old3/main.writ, old4/main.writ and 2 more"
	if we, ok := errors.AsType[*repo.WritFilesError](err); !ok || len(we.Names) != 7 || !strings.Contains(err.Error(), want) {
		t.Fatalf("a commit with .writ files: %v", err)
	}
	if n := blobCalls(g.Calls()); n != 0 {
		t.Fatalf("a refused commit fetched %d blobs", n)
	}
}

func TestReadRefusesATamperedBlob(t *testing.T) {
	ctx := context.Background()
	g, c, st := setup(t)
	files := estate()
	sha := g.Commit(4242, "main", files)
	evil := lockOf("9.9.9")
	g.Tamper(forge.BlobID([]byte(files["infra/network/.terraform.lock.hcl"].Content)), evil)
	_, err := repo.Read(ctx, c, "4242", sha, "infra", nil, st, repo.DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "network/.terraform.lock.hcl of commit") || !strings.Contains(err.Error(), "does not match its blob id") {
		t.Fatalf("a tampered blob: %v", err)
	}
	if b, _ := st.Blobs(ctx, []string{forge.BlobID([]byte(evil)), forge.BlobID([]byte(files["infra/network/.terraform.lock.hcl"].Content))}); len(b) != 0 {
		t.Fatalf("a tampered blob was stored: %v", b)
	}
}

func TestReadBounds(t *testing.T) {
	ctx := context.Background()
	g, c, st := setup(t)
	sha := g.Commit(4242, "main", estate())
	for _, tc := range []struct {
		lim  repo.Limits
		want string
	}{
		{repo.Limits{Files: 2, Bytes: 1 << 20, File: 1 << 20}, "over 2 lock files"},
		{repo.Limits{Files: 100, Bytes: 100, File: 1 << 20}, "over 100 bytes"},
	} {
		if _, err := repo.Read(ctx, c, "4242", sha, "infra", nil, st, tc.lim); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("limits %+v: %v", tc.lim, err)
		}
	}
	// A lock file over the limit on one is left out, and refuses only its
	// stack.
	snap, err := repo.Read(ctx, c, "4242", sha, "infra", nil, st, repo.Limits{Files: 100, Bytes: 1 << 20, File: 20})
	if err != nil || len(snap.Files) != 0 {
		t.Fatalf("locks over the limit on one file: %v, read %d", err, len(snap.Files))
	}
	stacks, diags, err := repo.Discover(snap)
	if err != nil || len(stacks) != 3 || len(diags) != 3 || !strings.Contains(diags[0].Message, "the lock file is over 20 bytes") {
		t.Fatalf("discover %+v %v %v", stacks, diags, err)
	}
}

// loadCounter is a store that counts the blob contents it loads.
type loadCounter struct {
	*control.Store
	loaded int
}

func (l *loadCounter) Blobs(ctx context.Context, ids []string) (map[string][]byte, error) {
	l.loaded += len(ids)
	return l.Store.Blobs(ctx, ids)
}

// TestReadChecksSizesBeforeLoading checks that a commit whose stored blobs
// are over the bound is refused before any content is loaded.
func TestReadChecksSizesBeforeLoading(t *testing.T) {
	ctx := context.Background()
	g, c, st := setup(t)
	sha := g.Commit(4242, "main", estate())
	snap, err := repo.Read(ctx, c, "4242", sha, "infra", nil, st, repo.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.Keep(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		lim  repo.Limits
		want string
	}{
		{repo.Limits{Files: 100, Bytes: 100, File: 1 << 20}, "the lock files of commit"},
	} {
		lc := &loadCounter{Store: st}
		_, err := repo.Read(ctx, c, "4242", sha, "infra", snap.Tree, lc, tc.lim)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("limits %+v: %v", tc.lim, err)
		}
		if lc.loaded != 0 {
			t.Fatalf("limits %+v: loaded %d blobs before the sizes refused the commit", tc.lim, lc.loaded)
		}
	}
	if calls := g.Calls(); blobCalls(calls) != 3 {
		t.Fatalf("the stored blobs were fetched again: %v", calls)
	}
}

// TestReadStoresNothing checks that Read leaves the store as it was, so a
// commit that fails a check leaves no blob behind, and that Keep stores
// what was fetched.
func TestReadStoresNothing(t *testing.T) {
	ctx := context.Background()
	stored := func(st *control.Store, files map[string]forgetest.File) int {
		var ids []string
		for _, f := range files {
			ids = append(ids, forge.BlobID([]byte(f.Content)))
		}
		sizes, err := st.BlobSizes(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		return len(sizes)
	}
	t.Run("a tampered blob after good ones", func(t *testing.T) {
		g, c, st := setup(t)
		files := estate()
		sha := g.Commit(4242, "main", files)
		// network sorts after app and database, so their locks were
		// fetched and checked when its lock fails.
		g.Tamper(forge.BlobID([]byte(files["infra/network/.terraform.lock.hcl"].Content)), lockOf("9.9.9"))
		if _, err := repo.Read(ctx, c, "4242", sha, "infra", nil, st, repo.DefaultLimits); err == nil || !strings.Contains(err.Error(), "does not match its blob id") {
			t.Fatalf("a tampered blob: %v", err)
		}
		if n := stored(st, files); n != 0 {
			t.Fatalf("a refused commit stored %d blobs", n)
		}
	})
	t.Run("a commit over the bound", func(t *testing.T) {
		g, c, st := setup(t)
		files := estate()
		sha := g.Commit(4242, "main", files)
		if _, err := repo.Read(ctx, c, "4242", sha, "infra", nil, st, repo.Limits{Files: 100, Bytes: 200, File: 1 << 20}); err == nil || !strings.Contains(err.Error(), "over 200 bytes") {
			t.Fatalf("over the bound: %v", err)
		}
		if n := stored(st, files); n != 0 {
			t.Fatalf("a refused commit stored %d blobs", n)
		}
	})
	t.Run("a good commit, kept", func(t *testing.T) {
		g, c, st := setup(t)
		files := estate()
		sha := g.Commit(4242, "main", files)
		snap, err := repo.Read(ctx, c, "4242", sha, "infra", nil, st, repo.DefaultLimits)
		if err != nil {
			t.Fatal(err)
		}
		if n := stored(st, files); n != 0 {
			t.Fatalf("Read stored %d blobs", n)
		}
		if err := snap.Keep(ctx, st, now); err != nil {
			t.Fatal(err)
		}
		if n := stored(st, files); n != 3 {
			t.Fatalf("Keep stored %d blobs, want 3", n)
		}
	})
}

// slowStore holds the first blob it stores until its caller is
// cancelled, as a write in progress at shutdown does, then finishes it.
type slowStore struct {
	*control.Store
	puts     atomic.Int32
	writing  chan struct{}
	finished chan struct{}
}

func (s *slowStore) PutBlob(ctx context.Context, id string, content []byte, at time.Time) error {
	if s.puts.Add(1) == 1 {
		close(s.writing)
		<-ctx.Done()
		defer close(s.finished)
	}
	return s.Store.PutBlob(context.WithoutCancel(ctx), id, content, at)
}

// A cancelled Keep stops after the blob it is writing, and the next read
// of the commit fetches only the blobs the cache lacks.
func TestKeepStopsBetweenBlobs(t *testing.T) {
	g, c, st := setup(t)
	sha := g.Commit(4242, "main", estate())
	slow := &slowStore{Store: st, writing: make(chan struct{}), finished: make(chan struct{})}
	snap, err := repo.Read(context.Background(), c, "4242", sha, "infra", nil, slow, repo.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Fetched != 3 {
		t.Fatalf("fetched %d blobs", snap.Fetched)
	}
	ctx, cancel := context.WithCancel(context.Background())
	kept := make(chan error, 1)
	go func() { kept <- snap.Keep(ctx, slow, now) }()
	<-slow.writing
	cancel()
	<-slow.finished
	if err := <-kept; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled Keep: %v", err)
	}
	if n := slow.puts.Load(); n != 1 {
		t.Fatalf("a cancelled Keep stored %d blobs, want the one it was writing", n)
	}
	g.Calls()
	again, err := repo.Read(context.Background(), c, "4242", sha, "infra", nil, st, repo.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if n := blobCalls(g.Calls()); n != 2 || again.Fetched != 2 {
		t.Fatalf("the read after a partial cache fetched %d blobs, counted %d; want 2", n, again.Fetched)
	}
	if err := again.Keep(context.Background(), st, now); err != nil {
		t.Fatal(err)
	}
	if third, err := repo.Read(context.Background(), c, "4242", sha, "infra", nil, st, repo.DefaultLimits); err != nil || third.Fetched != 0 {
		t.Fatalf("a read after the cache was completed fetched %d blobs, %v", third.Fetched, err)
	}
}
