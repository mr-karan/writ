package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/oidc"
)

// principal is a verified token, or a person's session, and what the
// config makes of it.
type principal struct {
	tok   *oidc.Token // nil for a session
	info  Principal
	teams []string
	// matched holds the claims the principal's teams matched on, for the
	// audit trail of a plan's author.
	matched map[string][]string
	// session is the session's id and how it travels, for a person with a
	// session; "" otherwise.
	session, sessionVia string
}

// newPrincipal reads a verified token of an issuer of identity, the
// config's name for whose user ids the token carries. A token whose claims
// name no user has no person: it approves nothing, and its author is no
// one an approver could be.
func newPrincipal(tok *oidc.Token, identity string, p *authz.Policy) *principal {
	pr := &principal{tok: tok, teams: p.TeamsOf(tok.Issuer, tok.Claims), matched: map[string][]string{}}
	c := tok.Claims
	s := func(name string) string { v, _ := c.String(name); return v }
	pr.info = Principal{Kind: "ci-job", Issuer: tok.Issuer, Subject: tok.Subject, Expires: stamp(tok.Expires)}
	switch tok.Kind {
	case oidc.GitLabCI:
		pr.info.Person = personID(identity, s("user_id"))
		pr.info.Display = "gitlab " + s("project_path") + " #" + s("pipeline_id") + " (" + s("user_login") + ")"
	case oidc.GitHubActions:
		pr.info.Person = personID(identity, s("actor_id"))
		pr.info.Display = "github " + s("repository") + " run " + s("run_id") + " (" + s("actor") + ")"
	}
	for _, t := range p.Teams {
		if !slices.Contains(pr.teams, t.Name) {
			continue
		}
		for _, r := range t.Match {
			if v, ok := c.Values(r.Claim); ok {
				pr.matched[r.Claim] = v
			}
		}
	}
	if pr.teams == nil {
		pr.teams = []string{}
	}
	return pr
}

// sessionPrincipal is a person signed in with a session, which via says
// how they hold. The teams are the ones the session started with.
func sessionPrincipal(sess *Session, via string) *principal {
	return &principal{
		info:  Principal{Kind: "person", Issuer: sess.Issuer, Subject: sess.Subject, Display: sess.Display, Person: sess.Person, Expires: stamp(sess.EndsAt), Session: sess.ID},
		teams: sess.Teams, matched: map[string][]string{}, session: sess.ID, sessionVia: via,
	}
}

// personID is the stable id of user id at identity: IDENTITY/user/ID.
// The identity is a name the config gives, never a URL's host, so no
// spelling of an issuer's URL gives one person two ids. It is "" when
// either part is missing.
func personID(identity, id string) string {
	if identity == "" || id == "" {
		return ""
	}
	return identity + "/user/" + id
}

// identityOfPerson is the identity a person id names, the part before
// /user/; "" for an id of another shape, such as an earlier build's,
// which held a host, or a dev person's.
func identityOfPerson(person string) string {
	identity, _, ok := strings.Cut(person, "/user/")
	if !ok || !nameRE.MatchString(identity) {
		return ""
	}
	return identity
}

// knownIdentity reports whether person is IDENTITY/user/ID of an identity
// an issuer of the config names.
func (s *Server) knownIdentity(person string) bool {
	id := identityOfPerson(person)
	return id != "" && slices.ContainsFunc(s.cfg.Issuers, func(is Issuer) bool { return is.Identity == id })
}

// rules are the config's approval rules. An approval counts only when its
// person is of an identity the config defines, so a person whose
// identity was renamed, and who approved again under the new name, counts
// once. --dev serves one person, checks no identity, and approves and
// applies a speculative plan.
func (s *Server) rules() control.Rules {
	return control.Rules{
		Counts:      func(person string) bool { return s.cfg.Dev || s.knownIdentity(person) },
		Speculative: s.cfg.Dev,
		Need: func(p control.Plan) int {
			n, _ := s.cfg.Policy.Need(p.Stack, p.Env, p.Risks, p.Changes)
			return n
		},
	}
}

// key is what limits count by: the person, or the token's subject when it
// names no person.
func (p *principal) key() string {
	if p.info.Person != "" {
		return p.info.Person
	}
	return p.info.Issuer + ":" + p.info.Subject
}

// via says how the principal proved itself, for the audit log: the CI job
// whose token it sent, or a browser session.
func (p *principal) via() string {
	if p.info.Kind == "dev" {
		return "dev token"
	}
	if p.tok == nil {
		return p.sessionVia
	}
	c := p.tok.Claims
	switch p.tok.Kind {
	case oidc.GitLabCI:
		job, _ := c.String("job_id")
		return "gitlab job " + job
	case oidc.GitHubActions:
		run, _ := c.String("run_id")
		return "github run " + run
	}
	return string(p.tok.Kind)
}

// sessionName is NAME in the role session name writ-NAME-RUN: ci for a CI
// job, the person's name for a session.
func (p *principal) sessionName() string {
	if p.tok == nil {
		return p.info.Display
	}
	return "ci"
}

// sourceIdentity names the principal on every AWS session made for it:
// gitlab-PROJECTID-PIPELINEID or github-REPOID-RUNID for a CI job, and
// the person, such as gitlab/user/2002, for a session.
func (p *principal) sourceIdentity() string {
	if p.tok == nil {
		return p.info.Person
	}
	c := p.tok.Claims
	s := func(name string) string { v, _ := c.String(name); return v }
	switch p.tok.Kind {
	case oidc.GitLabCI:
		return "gitlab-" + s("project_id") + "-" + s("pipeline_id")
	case oidc.GitHubActions:
		return "github-" + s("repository_id") + "-" + s("run_id")
	}
	return p.tok.Issuer + "-" + p.tok.Subject
}

// detail is the author column of a plan: the principal and the claims its
// teams matched on.
func (p *principal) detail() json.RawMessage {
	b, _ := json.Marshal(struct {
		Principal
		Teams   []string            `json:"teams"`
		Matched map[string][]string `json:"matched"`
	}{p.info, p.teams, p.matched})
	return b
}

// origin is where a plan came from, from the token's claims. The merge
// request is the request's hint, kept only for a merge request pipeline.
func (p *principal) origin(h Hints) json.RawMessage {
	if p.info.Kind == "dev" {
		return json.RawMessage(`{"kind":"dev"}`)
	}
	if p.tok == nil {
		return json.RawMessage(`{"kind":"session"}`)
	}
	c := p.tok.Claims
	s := func(name string) string { v, _ := c.String(name); return v }
	o := map[string]any{"issuer": issuerURL(p.tok)}
	switch p.tok.Kind {
	case oidc.GitLabCI:
		o["kind"] = "gitlab-pipeline"
		if s("pipeline_source") == "merge_request_event" {
			o["kind"] = "gitlab-merge-request"
			if h.MergeRequest > 0 {
				o["merge_request"] = h.MergeRequest
			}
		}
		o["project_id"], o["project"], o["pipeline"], o["job"] = s("project_id"), s("project_path"), s("pipeline_id"), s("job_id")
		o["sha"], o["ref"] = s("sha"), s("ref_path")
	case oidc.GitHubActions:
		o["kind"] = "github-run"
		o["repository_id"], o["repository"], o["run"] = s("repository_id"), s("repository"), s("run_id")
		o["sha"], o["ref"], o["event"] = s("sha"), s("ref"), s("event_name")
	}
	b, _ := json.Marshal(o)
	return b
}

// issuerURL is a token's issuer as a plan's origin and a token's use
// record it: its URL, which names one GitLab instance or one GitHub. A
// project, pipeline, or token id is unique only within one, and an issuer's
// config name may change.
func issuerURL(tok *oidc.Token) string { return strings.TrimSuffix(tok.URL, "/") }

// ciRun names the CI run a token proves, or the run a plan came from: the
// issuer's URL, and the project and pipeline on GitLab, or the repository
// and run on GitHub. Ids are compared only with the issuer's.
type ciRun struct{ issuer, project, pipeline string }

// ok reports whether the run is named in full. A run named in part, such
// as a session's, owns no plan.
func (r ciRun) ok() bool { return r.issuer != "" && r.project != "" && r.pipeline != "" }

// ciRun is the run the principal's token proves; the zero ciRun for a
// session, the dev token, or another kind of token.
func (p *principal) ciRun() ciRun {
	if p.tok == nil {
		return ciRun{}
	}
	s := func(name string) string { v, _ := p.tok.Claims.String(name); return v }
	switch p.tok.Kind {
	case oidc.GitLabCI:
		return ciRun{issuerURL(p.tok), s("project_id"), s("pipeline_id")}
	case oidc.GitHubActions:
		return ciRun{issuerURL(p.tok), s("repository_id"), s("run_id")}
	}
	return ciRun{}
}

// planOrigin is the part of a plan's origin that names its CI run.
type planOrigin struct {
	Kind         string `json:"kind"`
	Issuer       string `json:"issuer"`
	ProjectID    string `json:"project_id"`
	Pipeline     string `json:"pipeline"`
	RepositoryID string `json:"repository_id"`
	Run          string `json:"run"`
}

// originRun is the CI run a plan's origin names; the zero ciRun for a plan
// of a session or the dev token, or one whose origin names no issuer.
func originRun(doc json.RawMessage) ciRun {
	var o planOrigin
	if json.Unmarshal(doc, &o) != nil {
		return ciRun{}
	}
	switch o.Kind {
	case "gitlab-pipeline", "gitlab-merge-request":
		return ciRun{o.Issuer, o.ProjectID, o.Pipeline}
	case "github-run":
		return ciRun{o.Issuer, o.RepositoryID, o.Run}
	}
	return ciRun{}
}

// ownsPlan reports whether the principal's token is of a CI run that
// requested plan pl: the run that made it, or one whose plan was the same.
func (s *Server) ownsPlan(ctx context.Context, p *principal, pl control.Plan) (bool, error) {
	mine := p.ciRun()
	if !mine.ok() {
		return false, nil
	}
	if mine == originRun(pl.Origin) {
		return true, nil
	}
	return s.store.PipelineHas(ctx, control.Pipeline{Issuer: mine.issuer, ProjectID: mine.project, Pipeline: mine.pipeline}, pl.ID)
}
