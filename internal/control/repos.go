package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// RepoKind is the kind of forge a repository is at.
type RepoKind string

const (
	// GitLab is a project at a GitLab instance, read through its API.
	GitLab RepoKind = "gitlab"
	// Dir is a local Git repository, which only writ serve --dev reads.
	Dir RepoKind = "dir"
)

// Repo origins: added through POST /v1/repos, or pinned by the config.
const (
	FromAPI    = "api"
	FromConfig = "config"
)

// Repo is a repository the control plane reads code from. It holds no
// credential: Forge names the forge, whose token is a file the config
// names.
type Repo struct {
	Name     string
	Forge    string
	Kind     RepoKind
	ForgeURL string
	// Project is the project's path, as the forge reported it when it was
	// added; ProjectID is its numeric id, which every later call uses.
	// For a dir repository both are the absolute path of the Git work tree.
	Project   string
	ProjectID string
	// Path is the directory in the project that holds the stacks, "" for
	// the project's root.
	Path          string
	Prefix        string
	DefaultBranch string
	WebURL        string
	Origin        string
	AddedBy       string
	AddedAt       time.Time
	// Head is the newest commit of the default branch the control plane
	// read, "" before the first read.
	Head       string
	HeadReadAt time.Time
	PolledAt   time.Time
	// Error says why the last poll or read failed, "" after one that worked.
	Error   string
	ErrorAt time.Time
}

// TreeEntry is one regular file of a commit, named from the repository's
// path, with its Git blob id.
type TreeEntry struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
}

// Commit is one commit the control plane read.
type Commit struct {
	Repo   string
	SHA    string
	ReadAt time.Time
	// Tree lists every regular file under the repository's path.
	Tree []TreeEntry
	// Files and Bytes count the files of the repository's stacks, which
	// were fetched.
	Files int
	Bytes int64
	// Stacks and Diagnostics are the discovery result, as the server
	// encodes it.
	Stacks      json.RawMessage
	Diagnostics json.RawMessage
}

// RepoConflict is a repository that already holds the name, or the same
// project and path, of one being added.
type RepoConflict struct {
	Name     string
	SameName bool
}

func (e *RepoConflict) Error() string {
	if e.SameName {
		return fmt.Sprintf("a repository named %s exists", e.Name)
	}
	return fmt.Sprintf("repository %s has the same project and path", e.Name)
}

const repoColumns = `name, forge, kind, forge_url, project, project_id, path, prefix, default_branch, web_url, origin, added_by, added_at,
	head, head_read_at, polled_at, error, error_at`

func scanRepo(r scanner) (Repo, error) {
	var rp Repo
	var kind string
	var added int64
	var headAt, polled, errAt sqldb.NullInt64
	err := r.Scan(&rp.Name, &rp.Forge, &kind, &rp.ForgeURL, &rp.Project, &rp.ProjectID, &rp.Path, &rp.Prefix, &rp.DefaultBranch, &rp.WebURL,
		&rp.Origin, &rp.AddedBy, &added, &rp.Head, &headAt, &polled, &rp.Error, &errAt)
	if err != nil {
		return Repo{}, err
	}
	rp.Kind, rp.AddedAt = RepoKind(kind), fromMS(added)
	for _, t := range []struct {
		v  sqldb.NullInt64
		to *time.Time
	}{{headAt, &rp.HeadReadAt}, {polled, &rp.PolledAt}, {errAt, &rp.ErrorAt}} {
		if t.v.Valid {
			*t.to = fromMS(t.v.Int64)
		}
	}
	return rp, nil
}

// repoLock is the Postgres advisory lock that serializes the writes that
// judge a repository against the others. SQLite's write transaction holds
// the whole database.
const repoLock = 735301

// RepoStacks is a repository and the stacks of its head commit, as the
// server encoded them.
type RepoStacks struct {
	Repo   Repo
	Stacks json.RawMessage
}

// RepoCheck judges a repository, or the new head of one, against every
// other repository, inside the transaction that stores it, so that two
// requests cannot both pass. self holds the stacks about to be stored.
type RepoCheck func(self RepoStacks, others []RepoStacks) error

// checkRepo runs check, if any, over every repository but self's, with the
// stacks of each one's head commit. Nothing else writes repositories while
// it runs.
func checkRepo(ctx context.Context, tx *sqldb.Tx, self RepoStacks, check RepoCheck) error {
	if check == nil {
		return nil
	}
	if tx.Postgres() {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+fmt.Sprint(repoLock)+")"); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+repoColumns+" FROM repos WHERE name <> ? ORDER BY name", self.Repo.Name)
	if err != nil {
		return err
	}
	var others []RepoStacks
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			rows.Close()
			return err
		}
		others = append(others, RepoStacks{Repo: r})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i, o := range others {
		var stacks string
		err := tx.QueryRowContext(ctx, "SELECT stacks FROM repo_commits WHERE repo = ? AND sha = ?", o.Repo.Name, o.Repo.Head).Scan(&stacks)
		if err != nil && !errors.Is(err, sqldb.ErrNoRows) {
			return err
		}
		others[i].Stacks = json.RawMessage(jsonOr([]byte(stacks), "[]"))
	}
	return check(self, others)
}

// AddRepo stores a repository, with the commit of its default branch that
// was read when it was added, and writes the audit row, in one
// transaction. A repository with the same name, or the same project and
// path, is a *RepoConflict. check, when set, may refuse it with an error,
// which AddRepo returns as it is.
func (s *Store) AddRepo(ctx context.Context, r Repo, c Commit, e Entry, check RepoCheck) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if err := checkRepo(ctx, tx, RepoStacks{Repo: r, Stacks: c.Stacks}, check); err != nil {
			return err
		}
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM repos WHERE name = ? OR (kind = ? AND forge_url = ? AND project_id = ? AND path = ?)`,
			r.Name, string(r.Kind), r.ForgeURL, r.ProjectID, r.Path).Scan(&name)
		if err == nil {
			return &RepoConflict{Name: name, SameName: name == r.Name}
		}
		if !errors.Is(err, sqldb.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO repos (name, forge, kind, forge_url, project, project_id, path, prefix, default_branch, web_url,
				origin, added_by, added_at, head, head_read_at, polled_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Name, r.Forge, string(r.Kind), r.ForgeURL, r.Project, r.ProjectID, r.Path, r.Prefix, r.DefaultBranch, r.WebURL,
			r.Origin, r.AddedBy, r.AddedAt.UnixMilli(), c.SHA, c.ReadAt.UnixMilli(), c.ReadAt.UnixMilli())
		if err != nil {
			return err
		}
		c.Repo = r.Name
		if err := putCommit(ctx, tx, c); err != nil {
			return err
		}
		return insertAudit(ctx, tx, e)
	})
}

// DeleteRepo removes a repository and the commits read from it, and writes
// the audit row. The blobs stay: other commits may share them. Plans that
// name it keep its name.
func (s *Store) DeleteRepo(ctx context.Context, name string, e Entry) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM repo_commits WHERE repo = ?", name); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM repos WHERE name = ?", name)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmpErr(err, ErrNotFound)
		}
		return insertAudit(ctx, tx, e)
	})
}

// Repo returns the repository named name, or ErrNotFound.
func (s *Store) Repo(ctx context.Context, name string) (Repo, error) {
	r, err := scanRepo(s.db.QueryRowContext(ctx, "SELECT "+repoColumns+" FROM repos WHERE name = ?", name))
	if errors.Is(err, sqldb.ErrNoRows) {
		return Repo{}, ErrNotFound
	}
	return r, err
}

// Repos lists every repository by name.
func (s *Store) Repos(ctx context.Context) ([]Repo, error) {
	return s.repos(ctx, "SELECT "+repoColumns+" FROM repos ORDER BY name")
}

// ReposOfProject lists the repositories of one project at one forge, by
// name: a project with two estates is two repositories.
func (s *Store) ReposOfProject(ctx context.Context, kind RepoKind, forgeURL, projectID string) ([]Repo, error) {
	return s.repos(ctx, "SELECT "+repoColumns+" FROM repos WHERE kind = ? AND forge_url = ? AND project_id = ? ORDER BY name", string(kind), forgeURL, projectID)
}

func (s *Store) repos(ctx context.Context, q string, args ...any) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, s.db.Error(err)
		}
		out = append(out, r)
	}
	return out, s.db.Error(rows.Err())
}

// RecordHead stores a commit read from a repository's default branch,
// unless it is stored already, and makes it the repository's head. check,
// when set, may refuse the commit with an error, which RecordHead returns
// as it is, and the head stays where it was.
func (s *Store) RecordHead(ctx context.Context, c Commit, now time.Time, check RepoCheck) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if check != nil {
			r, err := scanRepo(tx.QueryRowContext(ctx, "SELECT "+repoColumns+" FROM repos WHERE name = ?", c.Repo))
			if errors.Is(err, sqldb.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if err := checkRepo(ctx, tx, RepoStacks{Repo: r, Stacks: c.Stacks}, check); err != nil {
				return err
			}
		}
		if err := putCommit(ctx, tx, c); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE repos SET head = ?, head_read_at = ?, polled_at = ?, error = '', error_at = NULL WHERE name = ?`,
			c.SHA, c.ReadAt.UnixMilli(), now.UnixMilli(), c.Repo)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmpErr(err, ErrNotFound)
		}
		return nil
	})
}

// RecordPoll records that a repository's default branch was polled: with
// msg "", the head had not moved; otherwise msg says why the poll failed.
func (s *Store) RecordPoll(ctx context.Context, name string, now time.Time, msg string) error {
	q := "UPDATE repos SET polled_at = ?, error = '', error_at = NULL WHERE name = ?"
	args := []any{now.UnixMilli(), name}
	if msg != "" {
		q = "UPDATE repos SET polled_at = ?, error = ?, error_at = ? WHERE name = ?"
		args = []any{now.UnixMilli(), msg, now.UnixMilli(), name}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// PutCommit stores a commit read for a plan, unless it is stored already.
func (s *Store) PutCommit(ctx context.Context, c Commit) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error { return putCommit(ctx, tx, c) })
}

func putCommit(ctx context.Context, tx *sqldb.Tx, c Commit) error {
	tree, err := json.Marshal(c.Tree)
	if err != nil {
		return err
	}
	if c.Tree == nil {
		tree = []byte("[]")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO repo_commits (repo, sha, read_at, tree, files, bytes, stacks, diagnostics)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (repo, sha) DO NOTHING`,
		c.Repo, c.SHA, c.ReadAt.UnixMilli(), string(tree), c.Files, c.Bytes, jsonOr(c.Stacks, "[]"), jsonOr(c.Diagnostics, "[]"))
	return err
}

// Commit returns a commit read from repo, or ErrNotFound.
func (s *Store) Commit(ctx context.Context, repo, sha string) (Commit, error) {
	c := Commit{Repo: repo, SHA: sha}
	var at int64
	var tree, stacks, diags string
	err := s.db.QueryRowContext(ctx, "SELECT read_at, tree, files, bytes, stacks, diagnostics FROM repo_commits WHERE repo = ? AND sha = ?", repo, sha).
		Scan(&at, &tree, &c.Files, &c.Bytes, &stacks, &diags)
	if errors.Is(err, sqldb.ErrNoRows) {
		return Commit{}, ErrNotFound
	}
	if err != nil {
		return Commit{}, err
	}
	c.ReadAt, c.Stacks, c.Diagnostics = fromMS(at), json.RawMessage(stacks), json.RawMessage(diags)
	if err := json.Unmarshal([]byte(tree), &c.Tree); err != nil {
		return Commit{}, fmt.Errorf("read a commit's tree from the control database: %w", err)
	}
	return c, nil
}

// blobBatch bounds the ids one query names.
const blobBatch = 500

// BlobSizes returns the size of each of ids the store holds, and reads no
// content.
func (s *Store) BlobSizes(ctx context.Context, ids []string) (map[string]int64, error) {
	out := map[string]int64{}
	err := s.eachBatch(ctx, ids, "SELECT id, size FROM blobs WHERE id IN ", func(rows *sqldb.Rows) error {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		out[id] = n
		return nil
	})
	return out, err
}

// Blobs returns the content of each of ids the store holds.
func (s *Store) Blobs(ctx context.Context, ids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := s.eachBatch(ctx, ids, "SELECT id, content FROM blobs WHERE id IN ", func(rows *sqldb.Rows) error {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return err
		}
		if b == nil {
			b = []byte{}
		}
		out[id] = b
		return nil
	})
	return out, err
}

func (s *Store) eachBatch(ctx context.Context, ids []string, q string, row func(*sqldb.Rows) error) error {
	for len(ids) > 0 {
		n := min(len(ids), blobBatch)
		batch := ids[:n]
		ids = ids[n:]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, q+"("+strings.TrimSuffix(strings.Repeat("?, ", n), ", ")+")", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := row(rows); err != nil {
				rows.Close()
				return s.db.Error(err)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return s.db.Error(err)
		}
	}
	return nil
}

// PutBlob stores a file's content under its Git blob id, which the caller
// checked against the content. A blob stored already is left as it is.
func (s *Store) PutBlob(ctx context.Context, id string, content []byte, now time.Time) error {
	if content == nil {
		content = []byte{}
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO blobs (id, size, content, stored_at) VALUES (?, ?, ?, ?) ON CONFLICT (id) DO NOTHING",
		id, len(content), content, now.UnixMilli())
	return err
}
