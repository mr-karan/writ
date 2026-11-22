package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/oidc"
)

// forgeKind is everything writ serve does differently for one kind of
// forge: what its forge block may leave out, the client that reads it,
// how a webhook delivery proves itself, and which CI tokens name its
// repositories. A new kind is one more entry of forgeKinds.
type forgeKind struct {
	// defaultURL is the url a forge block may leave out, "" when it must
	// name one.
	defaultURL string
	// tokenNeeds is what the read token needs, which the config's error
	// names.
	tokenNeeds string
	client     func(Forge) (forge.Client, error)
	hook       hookForge
	// ci is the issuer kind of its CI jobs' tokens, and job reads what
	// one of them names.
	ci  oidc.Kind
	job func(tok *oidc.Token) ciJob
	// forkNote ends the refusal of a CI job whose repository is none of
	// this control plane's.
	forkNote string
	// checkJob, when set, checks at the forge what a CI job's token names
	// before the job's commit is planned, through the repository's
	// client. It fails closed: an error it cannot settle refuses the plan.
	checkJob func(ctx context.Context, c forge.Client, repo control.Repo, job ciJob) *repoError
}

// ciJob is the repository and commit a CI job's token names.
type ciJob struct {
	// forgeURL is the URL of the forge the job's repository is at, ""
	// when the token's issuer is not a forge's.
	forgeURL string
	// projectID is the repository's numeric id at the forge, and project
	// its path, for messages only.
	projectID, project string
	sha, ref           string
	// event is the event that started the job, such as pull_request, ""
	// for a kind whose tokens name none.
	event string
}

var forgeKinds = map[control.RepoKind]forgeKind{
	control.GitLab: {
		tokenNeeds: "a group or project access token with read_api and the Reporter role",
		client: func(f Forge) (forge.Client, error) {
			return forge.NewGitLab(forge.GitLabOptions{URL: f.URL, TokenFile: f.ReadTokenFile, CAFile: f.CAFile})
		},
		hook: hookForge{verify: verifyGitLab, delivery: gitlabDelivery, moves: gitlabMoves},
		ci:   oidc.GitLabCI,
		job: func(tok *oidc.Token) ciJob {
			c := claimsOf(tok)
			ref := c("ref_path")
			if ref == "" && c("ref") != "" {
				ref = "refs/heads/" + c("ref")
			}
			// A GitLab instance issues its CI jobs' tokens at its own URL.
			return ciJob{forgeURL: issuerURL(tok), projectID: c("project_id"), project: c("project_path"), sha: c("sha"), ref: ref}
		},
		forkNote: "; a merge request from a fork runs in the fork's project, whose id matches no repository",
	},
	control.GitHub: {
		defaultURL: forge.GitHubURL,
		tokenNeeds: "a fine-grained token with Contents: read and Metadata: read on the repositories",
		client: func(f Forge) (forge.Client, error) {
			return forge.NewGitHub(forge.GitHubOptions{URL: f.URL, TokenFile: f.ReadTokenFile, CAFile: f.CAFile})
		},
		hook: hookForge{verify: verifyGitHub, delivery: githubDelivery, moves: githubMoves},
		ci:   oidc.GitHubActions,
		job: func(tok *oidc.Token) ciJob {
			c := claimsOf(tok)
			return ciJob{forgeURL: githubForgeOf(issuerURL(tok)), projectID: c("repository_id"), project: c("repository"), sha: c("sha"), ref: c("ref"), event: c("event_name")}
		},
		forkNote: "; GitHub gives no ID token to a pull request from a fork",
		checkJob: checkGitHubJob,
	},
}

// pullRefRE is a pull request's merge or head ref, refs/pull/N/merge or
// refs/pull/N/head, as a GitHub Actions job of a pull_request event names.
var pullRefRE = regexp.MustCompile(`^refs/pull/([1-9][0-9]{0,18})/(merge|head)$`)

// checkGitHubJob refuses a job no fork may start: one of a
// pull_request_target event, which runs with the base repository's
// permissions on a fork's pull request, and one of a pull request whose
// head is in another repository than the job's, such as a fork. GitHub
// gives no ID token to a fork's pull_request run, so the head check is a
// second line, should a fork's pull request reach a token another way.
func checkGitHubJob(ctx context.Context, c forge.Client, repo control.Repo, job ciJob) *repoError {
	if job.event == "pull_request_target" {
		return &repoError{status: http.StatusForbidden, class: diag.Denied, msg: "a job of a pull_request_target event may not plan: it runs with the repository's permissions for a pull request that may come from a fork; plan on pull_request or push"}
	}
	if !strings.HasPrefix(job.ref, "refs/pull/") {
		return nil
	}
	m := pullRefRE.FindStringSubmatch(job.ref)
	if m == nil {
		return &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("the token's ref %s is not a pull request's merge or head ref, so writ serve cannot check where the pull request comes from", trunc(job.ref))}
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return refusedRepo(http.StatusBadRequest, "the token's ref %s names pull request %s, which is not a pull request's number", trunc(job.ref), trunc(m[1]))
	}
	gh, ok := c.(*forge.GitHub)
	if !ok {
		return &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("repository %s is not read through GitHub's API, so writ serve cannot check where pull request %s comes from", repo.Name, m[1])}
	}
	head, err := gh.PullHeadRepo(ctx, repo.ProjectID, n)
	if err != nil {
		rerr := forgeRefusal(err)
		rerr.msg = fmt.Sprintf("writ serve could not check that pull request %s of repository %s comes from the repository itself, so it refuses the plan: %v", m[1], repo.Name, err)
		return rerr
	}
	if head != job.projectID {
		return &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("pull request %s of repository %s comes from another repository, id %s, such as a fork: writ serve plans only a pull request whose head is in the repository itself", m[1], repo.Name, head)}
	}
	return nil
}

// kindOfToken is the forge kind whose CI jobs a token's issuer kind
// speaks for.
func kindOfToken(k oidc.Kind) (control.RepoKind, forgeKind, bool) {
	for kind, fk := range forgeKinds {
		if fk.ci == k {
			return kind, fk, true
		}
	}
	return "", forgeKind{}, false
}

// githubForgeOf is the GitHub instance whose Actions issue tokens at
// issuer: github.com for https://token.actions.githubusercontent.com,
// with or without an enterprise's path after it, and https://HOST for a
// GitHub Enterprise Server's https://HOST/_services/token. Any other
// issuer is no GitHub's, "". The issuer is the config's, which a token's
// iss matched exactly, so a token cannot choose its forge.
func githubForgeOf(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	switch {
	case u.Host == "token.actions.githubusercontent.com":
		return forge.GitHubURL
	case u.Path == "/_services/token":
		return "https://" + u.Host
	}
	return ""
}

// claimsOf reads a token's string claims, "" for one it lacks.
func claimsOf(tok *oidc.Token) func(string) string {
	return func(name string) string { v, _ := tok.Claims.String(name); return v }
}
