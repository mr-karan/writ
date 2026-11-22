package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/repo"
)

// addTimeout bounds POST /v1/repos, which reads the files of the default
// branch's stacks: the first read of a repository fetches every one of them.
const addTimeout = 2 * time.Minute

// commitReadTimeout bounds one read of a commit for a poll or a plan.
const commitReadTimeout = 2 * time.Minute

// discovered is one stack as the control database keeps a commit's
// discovery. Tofu is set on every stack: a commit read before writ serve
// refused Writ's retired language may hold stacks without it, which
// stacksOf leaves out.
type discovered struct {
	Dir       string          `json:"dir"`
	Providers []repo.Provider `json:"providers"`
	Tofu      bool            `json:"tofu,omitempty"`
}

// forgeClient is a forge of the config with its client.
type forgeClient struct {
	cfg    Forge
	client forge.Client
}

// initForges makes a client for each forge of the config.
func (s *Server) initForges() error {
	s.forges = map[string]forgeClient{}
	for _, f := range s.cfg.Forges {
		c, err := forgeKinds[f.Kind].client(f)
		if err != nil {
			return fmt.Errorf("forge %s: %w", f.Name, err)
		}
		s.forges[f.Name] = forgeClient{cfg: f, client: c}
	}
	return nil
}

// clientOf returns the client that reads a repository: its forge's, while
// the forge keeps the URL the repository was added at, or the local one
// under --dev.
func (s *Server) clientOf(r control.Repo) (forge.Client, error) {
	if r.Kind == control.Dir {
		if !s.cfg.Dev {
			return nil, fmt.Errorf("repository %s is a local directory, which only writ serve --dev reads", r.Name)
		}
		return forge.Local{}, nil
	}
	f, ok := s.forges[r.Forge]
	if !ok {
		return nil, fmt.Errorf("repository %s is at forge %s, which the config no longer names", r.Name, r.Forge)
	}
	if f.cfg.URL != r.ForgeURL {
		return nil, fmt.Errorf("forge %s is now %s, and repository %s was added at %s; add the repository again", r.Forge, f.cfg.URL, r.Name, r.ForgeURL)
	}
	return f.client, nil
}

// readCommit returns the estate's files of a commit of r: from the control
// database when the commit was read before, with only the blobs it lacks
// from the forge, else from the forge, after which discovery runs and the
// commit is stored. It returns the stored commit too.
func (s *Server) readCommit(ctx context.Context, r control.Repo, c forge.Client, sha string) (*repo.Snapshot, control.Commit, error) {
	stored, err := s.store.Commit(ctx, r.Name, sha)
	switch {
	case err == nil:
		tree := make([]forge.File, len(stored.Tree))
		for i, e := range stored.Tree {
			tree[i] = forge.File{Path: e.Path, Blob: e.Blob}
		}
		snap, err := repo.Read(ctx, c, r.ProjectID, sha, r.Path, tree, s.store, repo.DefaultLimits)
		if err != nil {
			return nil, control.Commit{}, err
		}
		s.keepBlobs(r.Name, snap)
		return snap, stored, nil
	case !errors.Is(err, control.ErrNotFound):
		return nil, control.Commit{}, err
	}
	snap, err := repo.Read(ctx, c, r.ProjectID, sha, r.Path, nil, s.store, repo.DefaultLimits)
	if err != nil {
		return nil, control.Commit{}, err
	}
	commit, err := s.discover(snap, r.Name)
	if err != nil {
		return nil, control.Commit{}, err
	}
	if err := s.store.PutCommit(ctx, commit); err != nil {
		return nil, control.Commit{}, err
	}
	s.keepBlobs(r.Name, snap)
	return snap, commit, nil
}

// keepTimeout bounds the write of a commit's blobs to the cache.
const keepTimeout = 30 * time.Second

// keepBlobs stores the blobs a snapshot read from the forge, once its
// commit passed every check. It runs under the server's context, not the
// request's, so a client that leaves does not stop it, and a stop of the
// server does, after the blob being written. The blobs are a cache, so a
// failure is logged and costs only a later fetch: the next read of the
// commit fetches the blobs the cache lacks.
func (s *Server) keepBlobs(repoName string, snap *repo.Snapshot) {
	ctx, cancel := context.WithTimeout(s.ctx, keepTimeout)
	defer cancel()
	if err := snap.Keep(ctx, s.store, s.now()); err != nil {
		s.log.Error("store the blobs of a commit", "repo", repoName, "commit", snap.Commit, "err", err)
	}
}

// discover runs discovery over a snapshot and makes the commit row.
func (s *Server) discover(snap *repo.Snapshot, name string) (control.Commit, error) {
	found, diags, err := repo.Discover(snap)
	if err != nil {
		return control.Commit{}, err
	}
	stacks := make([]discovered, len(found))
	for i, f := range found {
		stacks[i] = discovered{Dir: f.Dir, Providers: f.Providers, Tofu: true}
	}
	ds := []Diagnostic{}
	for _, d := range diags {
		msg := d.Message
		if d.Span.File != "" {
			msg = d.Span.String() + ": " + msg
		}
		ds = append(ds, Diagnostic{string(d.Severity), string(d.Class), msg})
	}
	c := control.Commit{Repo: name, SHA: snap.Commit, ReadAt: s.now(), Files: len(snap.Files), Bytes: snap.Bytes}
	for _, f := range snap.Tree {
		c.Tree = append(c.Tree, control.TreeEntry{Path: f.Path, Blob: f.Blob})
	}
	c.Stacks, _ = json.Marshal(stacks)
	c.Diagnostics, _ = json.Marshal(ds)
	return c, nil
}

// repoError is a refusal of a repository request, with its status.
type repoError struct {
	status int
	class  diag.Class
	msg    string
	// retryAfter, when set, is how long a client should wait before it
	// asks again, which the answer's Retry-After says.
	retryAfter time.Duration
}

func (e *repoError) Error() string { return e.msg }

// retry sets Retry-After on an answer of the refusal, when it has one.
func (e *repoError) retry(w http.ResponseWriter) {
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.retryAfter.Seconds()))))
	}
}

func refusedRepo(status int, format string, args ...any) *repoError {
	return &repoError{status: status, class: diag.Usage, msg: fmt.Sprintf(format, args...)}
}

// forgeRefusal turns a forge's error into a refusal: a project, branch, or
// commit the token cannot see is 404, a forge's rate limit 503 with how
// long it lasts, so a client waits and asks again, and anything else 502,
// because the forge, not the request, failed.
func forgeRefusal(err error) *repoError {
	if rl, ok := errors.AsType[*forge.RateLimitError](err); ok {
		return &repoError{status: http.StatusServiceUnavailable, class: diag.Provider, msg: err.Error(), retryAfter: max(time.Until(rl.Until), time.Second)}
	}
	if _, ok := errors.AsType[*repo.WritFilesError](err); ok {
		return &repoError{status: http.StatusBadRequest, class: diag.Usage, msg: err.Error()}
	}
	if errors.Is(err, forge.ErrNotFound) {
		return &repoError{status: http.StatusNotFound, class: diag.Usage, msg: err.Error()}
	}
	return &repoError{status: http.StatusBadGateway, class: diag.Provider, msg: err.Error()}
}

// addRepo reads a repository's default branch, discovers its stacks, and
// stores it with the audit row e. It is POST /v1/repos, and the config's
// pinned list.
func (s *Server) addRepo(ctx context.Context, spec RepoSpec, origin, addedBy string, e control.Entry) (control.Repo, control.Commit, *repoError) {
	if err := checkRepoSpec(spec, s.cfg.Forges, s.cfg.Dev); err != nil {
		return control.Repo{}, control.Commit{}, refusedRepo(http.StatusBadRequest, "%v", err)
	}
	r := control.Repo{Name: spec.Name, Path: spec.Path, Prefix: spec.Prefix, Origin: origin, AddedBy: addedBy, AddedAt: s.now()}
	var c forge.Client
	if spec.Forge == string(control.Dir) {
		r.Forge, r.Kind, c = string(control.Dir), control.Dir, forge.Local{}
	} else {
		f, _ := findForge(s.cfg.Forges, spec.Forge)
		r.Forge, r.Kind, r.ForgeURL, c = f.Name, f.Kind, f.URL, s.forges[f.Name].client
	}
	p, err := c.Project(ctx, spec.Project)
	if err != nil {
		return control.Repo{}, control.Commit{}, forgeRefusal(err)
	}
	r.Project, r.ProjectID, r.DefaultBranch, r.WebURL = p.Path, p.ID, p.DefaultBranch, p.WebURL
	if rerr := s.repoTaken(ctx, r); rerr != nil {
		return control.Repo{}, control.Commit{}, rerr
	}
	head, err := c.Head(ctx, p.ID, p.DefaultBranch)
	if err != nil {
		return control.Repo{}, control.Commit{}, forgeRefusal(err)
	}
	snap, err := repo.Read(ctx, c, p.ID, head, r.Path, nil, s.store, repo.DefaultLimits)
	if err != nil {
		return control.Repo{}, control.Commit{}, forgeRefusal(err)
	}
	commit, err := s.discover(snap, r.Name)
	if err != nil {
		return control.Repo{}, control.Commit{}, refusedRepo(http.StatusBadRequest, "%s at %s: %v", r.Project, forge.Short(head), err)
	}
	e.Reason = fmt.Sprintf("repository %s: %s path %q at %s, %d files fetched", r.Name, r.Project, r.Path, forge.Short(head), snap.Fetched)
	if err := s.store.AddRepo(ctx, r, commit, e, addRules); err != nil {
		if rerr, ok := errors.AsType[*repoError](err); ok {
			return control.Repo{}, control.Commit{}, rerr
		}
		var conflict *control.RepoConflict
		if errors.As(err, &conflict) {
			if conflict.SameName {
				return control.Repo{}, control.Commit{}, refusedRepo(http.StatusConflict, "a repository named %s exists; pick another name", r.Name)
			}
			return control.Repo{}, control.Commit{}, refusedRepo(http.StatusConflict, "repository %s already reads %s at path %q", conflict.Name, r.Project, r.Path)
		}
		s.log.Error("add a repository", "repo", r.Name, "err", err)
		return control.Repo{}, control.Commit{}, &repoError{status: http.StatusServiceUnavailable, class: diag.StateIO, msg: "the control database failed"}
	}
	s.keepBlobs(r.Name, snap)
	r.Head, r.HeadReadAt, r.PolledAt = commit.SHA, commit.ReadAt, commit.ReadAt
	s.log.Info("repository added", "repo", r.Name, "kind", r.Kind, "project", r.Project, "project_id", r.ProjectID, "path", r.Path, "commit", head, "fetched", snap.Fetched, "by", addedBy)
	return r, commit, nil
}

// repoTaken refuses a repository whose name, or whose project and path,
// another repository has, before its files are read. AddRepo checks again
// in its transaction.
func (s *Server) repoTaken(ctx context.Context, r control.Repo) *repoError {
	same, err := s.store.ReposOfProject(ctx, r.Kind, r.ForgeURL, r.ProjectID)
	if err != nil {
		return &repoError{status: http.StatusServiceUnavailable, class: diag.StateIO, msg: "the control database failed"}
	}
	for _, o := range same {
		if o.Path == r.Path && o.Name != r.Name {
			return refusedRepo(http.StatusConflict, "repository %s already reads %s at path %q", o.Name, r.Project, r.Path)
		}
	}
	if _, err := s.store.Repo(ctx, r.Name); err == nil {
		return refusedRepo(http.StatusConflict, "a repository named %s exists; pick another name", r.Name)
	} else if !errors.Is(err, control.ErrNotFound) {
		return &repoError{status: http.StatusServiceUnavailable, class: diag.StateIO, msg: "the control database failed"}
	}
	all, err := s.store.Repos(ctx)
	if err != nil {
		return &repoError{status: http.StatusServiceUnavailable, class: diag.StateIO, msg: "the control database failed"}
	}
	if o, ok := nestedPrefix(all, r); ok {
		return refusedRepo(http.StatusConflict, "prefix %q of repository %s and prefix %q of repository %s start one another; a prefix may not start with another repository's prefix, or be the start of one, so that each stack name has one prefix to remove", r.Prefix, r.Name, o.Prefix, o.Name)
	}
	return nil
}

// nestedPrefix finds a repository among others whose prefix starts with r's
// prefix or is the start of it, but is not the same. Both prefixes must be
// set. A name then has one longest prefix, and estateName removes it.
func nestedPrefix(others []control.Repo, r control.Repo) (control.Repo, bool) {
	if r.Prefix == "" {
		return control.Repo{}, false
	}
	for _, o := range others {
		if o.Name != r.Name && o.Prefix != "" && o.Prefix != r.Prefix &&
			(strings.HasPrefix(o.Prefix, r.Prefix) || strings.HasPrefix(r.Prefix, o.Prefix)) {
			return o, true
		}
	}
	return control.Repo{}, false
}

// addRules judges a repository about to be added, inside the transaction
// that stores it. Its prefix may not nest with another's, and see
// headRules for its stacks.
func addRules(self control.RepoStacks, others []control.RepoStacks) error {
	if o, ok := nestedPrefix(reposOf(others), self.Repo); ok {
		return refusedRepo(http.StatusConflict, "prefix %q of repository %s and prefix %q of repository %s start one another; a prefix may not start with another repository's prefix, or be the start of one, so that each stack name has one prefix to remove", self.Repo.Prefix, self.Repo.Name, o.Prefix, o.Name)
	}
	if msg := prefixShadow(self, others, true); msg != "" {
		return refusedRepo(http.StatusConflict, "%s", msg)
	}
	return headRules(self, others)
}

// headRules judges the stacks a repository is about to have, for a
// repository being added and for a new head of one: no stack may have the
// name of another repository's stack, because state is kept by stack name,
// and none may have a name that starts with another repository's prefix,
// which writ.stack.name would remove.
func headRules(self control.RepoStacks, others []control.RepoStacks) error {
	mine := map[string]bool{}
	for _, st := range stacksOf(control.Commit{Stacks: self.Stacks}) {
		mine[self.Repo.Prefix+st.Dir] = true
	}
	for _, o := range others {
		for _, st := range stacksOf(control.Commit{Stacks: o.Stacks}) {
			if name := o.Repo.Prefix + st.Dir; mine[name] {
				return refusedRepo(http.StatusConflict, "stack %s of %s is also a stack of repository %s; set a prefix, such as %s/, so each stack has its own name", name, self.Repo.Project, o.Repo.Name, self.Repo.Name)
			}
		}
	}
	if msg := prefixShadow(self, others, false); msg != "" {
		return refusedRepo(http.StatusConflict, "%s", msg)
	}
	return nil
}

func reposOf(rs []control.RepoStacks) []control.Repo {
	out := make([]control.Repo, len(rs))
	for i, r := range rs {
		out[i] = r.Repo
	}
	return out
}

// prefixShadow says which stack of self has a name that starts with the
// prefix of another repository, "" when none does. With both, it also
// looks at the stacks of the others and the prefix of self. A name that
// starts with another repository's prefix is read, by estateName, as the
// rest of it.
func prefixShadow(self control.RepoStacks, others []control.RepoStacks, both bool) string {
	repos := append(reposOf(others), self.Repo)
	check := func(rs control.RepoStacks) string {
		for _, st := range stacksOf(control.Commit{Stacks: rs.Stacks}) {
			name := rs.Repo.Prefix + st.Dir
			o, ok := longestPrefix(repos, name)
			if ok && len(o.Prefix) != len(rs.Repo.Prefix) {
				return fmt.Sprintf("stack %s of repository %s starts with prefix %q of repository %s, which would read its name as %s; give one of the repositories another prefix, so that no stack name starts with another repository's prefix", name, rs.Repo.Name, o.Prefix, o.Name, strings.TrimPrefix(name, o.Prefix))
			}
		}
		return ""
	}
	if msg := check(self); msg != "" || !both {
		return msg
	}
	for _, o := range others {
		if msg := check(o); msg != "" {
			return msg
		}
	}
	return ""
}

// longestPrefix finds the repository whose prefix is the longest one that
// starts name and leaves something of it.
func longestPrefix(repos []control.Repo, name string) (control.Repo, bool) {
	var best control.Repo
	for _, r := range repos {
		if rest, ok := strings.CutPrefix(name, r.Prefix); ok && rest != "" && strings.HasSuffix(r.Prefix, "/") && len(r.Prefix) > len(best.Prefix) {
			best = r
		}
	}
	return best, best.Name != ""
}

func stacksOf(c control.Commit) []discovered {
	var all []discovered
	_ = json.Unmarshal(c.Stacks, &all)
	return slices.DeleteFunc(all, func(d discovered) bool { return !d.Tofu })
}

// repoInfo is a repository as the API shows it. It holds no credential,
// nor the name of its webhook secret file.
func (s *Server) repoInfo(r control.Repo) RepoInfo {
	return RepoInfo{
		Name: r.Name, Forge: r.Forge, Kind: string(r.Kind), ForgeURL: r.ForgeURL, Project: r.Project, ProjectID: r.ProjectID,
		Path: r.Path, Prefix: r.Prefix, DefaultBranch: r.DefaultBranch, WebURL: r.WebURL, Origin: r.Origin, AddedBy: r.AddedBy,
		AddedAt: stamp(r.AddedAt), Head: r.Head, HeadReadAt: stamp(r.HeadReadAt), PolledAt: stamp(r.PolledAt), Error: r.Error, ErrorAt: stamp(r.ErrorAt),
		Hook: s.hasHook(r), HookAt: stamp(r.HookAt),
	}
}

// repoDoc is writ.repo/v0: the repository and what discovery found at its
// head, with what this config serves.
func (s *Server) repoDoc(r control.Repo, c control.Commit) RepoDoc {
	doc := RepoDoc{Format: FormatRepo, OK: true, Repo: s.repoInfo(r), Stacks: []FoundStack{}, Diagnostics: []Diagnostic{}}
	if c.SHA == "" {
		return doc
	}
	doc.Commit = &CommitInfo{SHA: c.SHA, ReadAt: stamp(c.ReadAt), Files: c.Files, Bytes: c.Bytes}
	_ = json.Unmarshal(c.Diagnostics, &doc.Diagnostics)
	envs := s.envNames()
	if s.cfg.Dev {
		envs = []string{DevEnv}
	}
	for _, st := range stacksOf(c) {
		f := FoundStack{Stack: r.Prefix + st.Dir, Dir: path.Join(r.Path, st.Dir), Served: []string{}, NotServed: []string{}, Providers: []FoundProvider{}}
		if s.exec != nil {
			f.Served = append(f.Served, envs...)
		} else {
			f.NotServed = append(f.NotServed, envs...)
		}
		for _, p := range st.Providers {
			f.Providers = append(f.Providers, FoundProvider{Source: p.Source, Version: p.Version, Runs: s.exec != nil})
		}
		doc.Stacks = append(doc.Stacks, f)
	}
	return doc
}

// mayAny reports whether the principal holds op on any stack@env, which
// is what reading the repositories takes: they hold code, not state.
func (s *Server) mayAny(p *principal, op authz.Op) bool {
	for _, row := range s.cfg.Policy.Table(p.teams) {
		if slices.Contains(row.Can, op) {
			return true
		}
	}
	return false
}

// maxStackParts is the most parts a stack name can have: each part holds
// at least one character, and a slash separates two.
const maxStackParts = (MaxStackName + 1) / 2

// createRepo is POST /v1/repos. It takes admin on every stack in every
// environment, because a repository's code reaches every stack whose name
// it holds.
func (s *Server) createRepo(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	var body RepoRequest
	if err := s.readBody(r, &body); err != nil {
		s.refuse(w, r, FormatRepo, diag.Usage, http.StatusBadRequest, "%v", err)
		return
	}
	if body.Format != FormatRepoRequest {
		s.refuse(w, r, FormatRepo, diag.Usage, http.StatusBadRequest, "the body's format is %q, not %s", trunc(body.Format), FormatRepoRequest)
		return
	}
	req.entry.Op, req.entry.Env = string(authz.Admin), "*"
	if d := s.cfg.Policy.DecideEvery(req.principal.teams, authz.Admin, maxStackParts); !d.Allowed {
		s.refuse(w, r, FormatRepo, diag.Denied, http.StatusForbidden, "%s may not add a repository: %s; pin the repository in the config's repos instead", req.principal.info.Display, d.Reason)
		return
	} else {
		req.entry.Decision, req.entry.Grants = control.Allowed, oneBased(d.Grants)
	}
	if s.cfg.PinRepos {
		s.refuse(w, r, FormatRepo, diag.Usage, http.StatusConflict, "the config pins the repositories with repos, so they are added there and reviewed with the config")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), addTimeout)
	defer cancel()
	e := req.entry
	e.At, e.Status, e.Elapsed = req.start, http.StatusCreated, 0
	spec := RepoSpec{Name: body.Name, Forge: body.Forge, Project: body.Project, Path: body.Path, Prefix: body.Prefix}
	rp, c, rerr := s.addRepo(ctx, spec, control.FromAPI, req.principal.key(), e)
	if rerr != nil {
		rerr.retry(w)
		s.refuse(w, r, FormatRepo, rerr.class, rerr.status, "%s", rerr.msg)
		return
	}
	req.audited = true
	w.Header().Set("Location", "/v1/repos/"+rp.Name)
	s.reply(w, http.StatusCreated, s.repoDoc(rp, c))
}

// listRepos is GET /v1/repos.
func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	req.entry.Op = string(authz.Read)
	if !s.mayAny(req.principal, authz.Read) {
		s.refuse(w, r, FormatRepos, diag.Denied, http.StatusForbidden, "%s may not read anything, so may not list the repositories", req.principal.info.Display)
		return
	}
	repos, err := s.store.Repos(r.Context())
	if err != nil {
		s.refuse(w, r, FormatRepos, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", req.id)
		return
	}
	req.entry.Decision = control.Allowed
	doc := ReposDoc{Format: FormatRepos, OK: true, Pinned: s.cfg.PinRepos, Repos: []RepoInfo{}, Diagnostics: []Diagnostic{}}
	for _, rp := range repos {
		info := s.repoInfo(rp)
		if c, err := s.store.Commit(r.Context(), rp.Name, rp.Head); err == nil {
			n := len(stacksOf(c))
			info.Stacks = &n
		}
		doc.Repos = append(doc.Repos, info)
	}
	s.reply(w, http.StatusOK, doc)
}

// getRepo is GET /v1/repos/{name}: the repository and the stacks found at
// the head of its default branch.
func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	req.entry.Op = string(authz.Read)
	name := r.PathValue("name")
	if !nameRE.MatchString(name) {
		s.refuse(w, r, FormatRepo, diag.Usage, http.StatusNotFound, "%q is not a repository name", trunc(name))
		return
	}
	if !s.mayAny(req.principal, authz.Read) {
		s.refuse(w, r, FormatRepo, diag.Denied, http.StatusForbidden, "%s may not read anything, so may not read a repository", req.principal.info.Display)
		return
	}
	rp, err := s.store.Repo(r.Context(), name)
	if errors.Is(err, control.ErrNotFound) {
		s.refuse(w, r, FormatRepo, diag.Usage, http.StatusNotFound, "no repository %s", name)
		return
	}
	if err != nil {
		s.refuse(w, r, FormatRepo, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", req.id)
		return
	}
	req.entry.Decision = control.Allowed
	c, err := s.store.Commit(r.Context(), rp.Name, rp.Head)
	if err != nil && !errors.Is(err, control.ErrNotFound) {
		s.refuse(w, r, FormatRepo, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", req.id)
		return
	}
	s.reply(w, http.StatusOK, s.repoDoc(rp, c))
}

// poll polls each repository's default branch every PollEvery, when it is
// set, and reads each repository a webhook queued as it is queued, until
// ctx ends. Close waits for it.
func (s *Server) poll(ctx context.Context) {
	var tick <-chan time.Time
	if s.cfg.PollEvery > 0 {
		t := time.NewTicker(s.cfg.PollEvery)
		defer t.Stop()
		tick = t.C
		s.pollOnce(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			s.pollOnce(ctx)
		case <-s.hooks.wake:
			s.pollHooked(ctx)
		}
	}
}

// pollOnce brings the pinned repositories in line with the config, then
// asks the forge for the head of each repository's default branch that is
// due, one call each, and reads the commit when the head moved.
func (s *Server) pollOnce(ctx context.Context) {
	if s.cfg.PinRepos {
		s.syncRepos(ctx)
	}
	repos, err := s.store.Repos(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("list repositories to poll", "err", err)
		}
		return
	}
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		if s.pollDue(r) {
			s.pollRepo(ctx, r)
		}
	}
}

// pollDue reports whether the poll reads r now. A repository without a
// webhook is read at each poll; one with a webhook only once
// PollEveryWithHook passed since its last read, within half a poll, so a
// read the tick runs a moment early is not put off a whole poll.
func (s *Server) pollDue(r control.Repo) bool {
	if !s.hasHook(r) || r.PolledAt.IsZero() {
		return true
	}
	return s.now().Sub(r.PolledAt) >= s.cfg.PollEveryWithHook-s.cfg.PollEvery/2
}

// pollHooked reads the head of each repository a webhook queued.
func (s *Server) pollHooked(ctx context.Context) {
	for _, name := range s.hooks.take() {
		if ctx.Err() != nil {
			return
		}
		r, err := s.store.Repo(ctx, name)
		if err != nil {
			if !errors.Is(err, control.ErrNotFound) && ctx.Err() == nil {
				s.log.Error("read a repository a webhook queued", "repo", name, "err", err)
			}
			continue
		}
		s.pollRepo(ctx, r)
	}
}

func (s *Server) pollRepo(ctx context.Context, r control.Repo) {
	fail := func(err error) {
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("poll a repository", "repo", r.Name, "err", err)
		if err := s.store.RecordPoll(context.WithoutCancel(ctx), r.Name, s.now(), err.Error()); err != nil {
			s.log.Error("record a poll", "repo", r.Name, "err", err)
		}
	}
	c, err := s.clientOf(r)
	if err != nil {
		fail(err)
		return
	}
	head, err := c.Head(ctx, r.ProjectID, r.DefaultBranch)
	if err != nil {
		fail(err)
		return
	}
	if head == r.Head {
		if err := s.store.RecordPoll(ctx, r.Name, s.now(), ""); err != nil && ctx.Err() == nil {
			s.log.Error("record a poll", "repo", r.Name, "err", err)
		}
		return
	}
	rctx, cancel := context.WithTimeout(ctx, commitReadTimeout)
	defer cancel()
	snap, commit, err := s.readCommit(rctx, r, c, head)
	if err != nil {
		fail(err)
		return
	}
	if err := s.store.RecordHead(ctx, commit, s.now(), headRules); err != nil {
		fail(err)
		return
	}
	s.log.Info("repository read", "repo", r.Name, "branch", r.DefaultBranch, "commit", head, "files", commit.Files, "fetched", snap.Fetched, "stacks", len(stacksOf(commit)))
}

// syncRepos makes the repository rows follow the config's repos: a row the
// config does not list, or lists with other fields, is removed, and a
// repository it lists with no row is added.
func (s *Server) syncRepos(ctx context.Context) {
	rows, err := s.store.Repos(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("list repositories to sync", "err", err)
		}
		return
	}
	have := map[string]control.Repo{}
	for _, r := range rows {
		have[r.Name] = r
	}
	entry := func(kind, reason string) control.Entry {
		return control.Entry{At: s.now(), Request: newID(), Kind: kind, Op: string(authz.Admin), Principal: "config", Decision: control.NoDecision, Reason: reason, ConfigID: s.configID}
	}
	for _, spec := range s.cfg.Repos {
		r, ok := have[spec.Name]
		delete(have, spec.Name)
		if ok && r.Origin == control.FromConfig && s.sameSpec(r, spec) {
			continue
		}
		if ok {
			if err := s.store.DeleteRepo(ctx, r.Name, entry("repo-removed", "the config lists repository "+r.Name+" with other fields")); err != nil {
				s.log.Error("remove a repository", "repo", r.Name, "err", err)
				continue
			}
		}
		actx, cancel := context.WithTimeout(ctx, addTimeout)
		_, _, rerr := s.addRepo(actx, spec, control.FromConfig, "config", entry("repo-added", ""))
		cancel()
		if rerr != nil && ctx.Err() == nil {
			s.log.Error("add a repository the config lists", "repo", spec.Name, "err", rerr.msg)
		}
	}
	for name := range have {
		if err := s.store.DeleteRepo(ctx, name, entry("repo-removed", "the config does not list repository "+name)); err != nil {
			s.log.Error("remove a repository", "repo", name, "err", err)
			continue
		}
		s.log.Info("repository removed: the config does not list it", "repo", name)
	}
}

func (s *Server) sameSpec(r control.Repo, spec RepoSpec) bool {
	if r.Path != spec.Path || r.Prefix != spec.Prefix || r.Project != spec.Project && r.ProjectID != spec.Project {
		return false
	}
	if spec.Forge == string(control.Dir) {
		return r.Kind == control.Dir
	}
	f, err := findForge(s.cfg.Forges, spec.Forge)
	return err == nil && f.Name == r.Forge && f.URL == r.ForgeURL
}

// stackDir is the directory of a stack in its repository, from its name.
func stackDir(r control.Repo, stack string) (string, bool) {
	dir, ok := strings.CutPrefix(stack, r.Prefix)
	return dir, ok && dir != ""
}
