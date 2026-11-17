package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/repo"
)

// planInput is the commit of a repository a plan is made from, which the
// plan reads before it runs: the repository, its client, the commit, and
// the stack's directory in the repository.
type planInput struct {
	repo   control.Repo
	client forge.Client
	source control.PlanSource
	dir    string
}

var refRE = regexp.MustCompile(`^refs/[A-Za-z0-9._/-]{1,250}$`)

// fullRef reports whether ref is a full Git ref whose characters are safe
// in a forge's query and in git's arguments: refs/, no .. part, and no
// leading dash.
func fullRef(ref string) bool {
	return refRE.MatchString(ref) && !strings.Contains(ref, "..") && !strings.HasSuffix(ref, "/")
}

// planSource finds the repository and commit a commit plan names, checks
// the commit exists at the forge, and checks that it is on the ref the
// plan names, refusing the plan when the forge cannot show it. A CI job's token names its own
// project and commit: the job may not plan another's, and a pipeline of a
// fork, whose project id matches no repository, is refused.
func (s *Server) planSource(ctx context.Context, p *principal, req *SourceRequest, stack string) (planInput, *repoError) {
	var in planInput
	var commit string
	if p.tok != nil {
		if p.tok.Kind != oidc.GitLabCI {
			return in, refusedRepo(http.StatusBadRequest, "a plan of a commit from a %s token is not built yet", p.tok.Kind)
		}
		claim := func(name string) string { v, _ := p.tok.Claims.String(name); return v }
		projectID, sha := claim("project_id"), claim("sha")
		ref := claim("ref_path")
		if ref == "" && claim("ref") != "" {
			ref = "refs/heads/" + claim("ref")
		}
		repos, err := s.store.ReposOfProject(ctx, control.GitLab, issuerURL(p.tok), projectID)
		if err != nil {
			return in, &repoError{status: http.StatusServiceUnavailable, class: diag.StateIO, msg: "the control database failed"}
		}
		if len(repos) == 0 {
			return in, &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("project %s (id %s) is not a repository of this control plane; a merge request from a fork runs in the fork's project, whose id matches no repository", trunc(claim("project_path")), trunc(projectID))}
		}
		switch {
		case req.Repo != "":
			for _, r := range repos {
				if r.Name == req.Repo {
					in.repo = r
				}
			}
			if in.repo.Name == "" {
				return in, &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("repository %s is not of the job's project %s", trunc(req.Repo), trunc(projectID))}
			}
		case len(repos) == 1:
			in.repo = repos[0]
		default:
			names := make([]string, len(repos))
			for i, r := range repos {
				names[i] = r.Name
			}
			return in, refusedRepo(http.StatusBadRequest, "project %s holds the repositories %s; name one with --repo", trunc(projectID), strings.Join(names, ", "))
		}
		if req.Commit != "" && req.Commit != sha {
			return in, &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("a CI job plans its own commit: the token names %s, not %s", forge.Short(sha), forge.Short(req.Commit))}
		}
		if ref != "" && !fullRef(ref) {
			return in, refusedRepo(http.StatusBadRequest, "the token's ref_path %q is not a full ref, such as refs/heads/main", trunc(ref))
		}
		commit, in.source.Ref = sha, ref
	} else {
		if req.Repo == "" || !forge.IsSHA(req.Commit) {
			return in, refusedRepo(http.StatusBadRequest, "a plan of a commit names the repository and the commit's full id: source.repo and source.commit")
		}
		if req.Ref != "" && !fullRef(req.Ref) {
			return in, refusedRepo(http.StatusBadRequest, "source.ref %q is not a full ref, such as refs/heads/main", trunc(req.Ref))
		}
		r, err := s.store.Repo(ctx, req.Repo)
		if errors.Is(err, control.ErrNotFound) {
			return in, refusedRepo(http.StatusNotFound, "no repository %s: add it with writ repo add", trunc(req.Repo))
		}
		if err != nil {
			return in, &repoError{status: http.StatusServiceUnavailable, class: diag.StateIO, msg: "the control database failed"}
		}
		in.repo, commit, in.source.Ref = r, req.Commit, req.Ref
	}
	dir, ok := stackDir(in.repo, stack)
	if !ok {
		return in, refusedRepo(http.StatusBadRequest, "stack %s is not a stack name of repository %s, whose stacks are named %s and a directory", stack, in.repo.Name, cmpOr(in.repo.Prefix, "by"))
	}
	c, err := s.clientOf(in.repo)
	if err != nil {
		return in, refusedRepo(http.StatusConflict, "%v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, forge.CallTimeout)
	defer cancel()
	full, err := c.Commit(cctx, in.repo.ProjectID, commit)
	if err != nil {
		rerr := forgeRefusal(err)
		if rerr.status == http.StatusNotFound {
			rerr.msg = fmt.Sprintf("commit %s is not in repository %s: push it (%s)", forge.Short(commit), in.repo.Name, err)
		}
		return in, rerr
	}
	in.source.RefCheck = control.NoRef
	if ref := in.source.Ref; ref != "" {
		on, err := c.OnRef(cctx, in.repo.ProjectID, full, ref)
		if err != nil {
			rerr := forgeRefusal(err)
			rerr.msg = fmt.Sprintf("writ serve could not check that commit %s is on %s, so it refuses the plan: %v", forge.Short(full), ref, err)
			return in, rerr
		}
		if !on {
			return in, &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("commit %s is not on %s of repository %s: the forge shows it is neither the ref's commit nor an ancestor of it", forge.Short(full), ref, in.repo.Name)}
		}
		in.source.RefCheck = control.OnRef
	}
	in.client, in.dir = c, dir
	in.source.Repo, in.source.Commit = in.repo.Name, full
	return in, nil
}

// commitSnapshot reads a commit's files, bounded by commitReadTimeout.
func (s *Server) commitSnapshot(ctx context.Context, r control.Repo, c forge.Client, commit string) (*repo.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, commitReadTimeout)
	defer cancel()
	snap, _, err := s.readCommit(ctx, r, c, commit)
	if err != nil {
		return nil, &classedError{diag.Usage, err}
	}
	return snap, nil
}

// commitDigest is the source digest of stack at a commit, which a plan of
// that commit records: the source match compares the two. It is the
// digest of the files TofuSources gathers.
func (s *Server) commitDigest(ctx context.Context, r control.Repo, c forge.Client, commit, stack string) (string, error) {
	dir, ok := stackDir(r, stack)
	if !ok {
		return "", fmt.Errorf("stack %s is not a stack name of repository %s", stack, r.Name)
	}
	snap, err := s.commitSnapshot(ctx, r, c, commit)
	if err != nil {
		return "", err
	}
	ts, err := repo.TofuSources(ctx, c, r.ProjectID, snap, dir, s.store, repo.DefaultLimits)
	if err != nil {
		return "", err
	}
	s.keepBlobs(r.Name, snap)
	return ts.Digest, nil
}
