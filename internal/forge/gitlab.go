package forge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// GitLabOptions configure a GitLab client.
type GitLabOptions struct {
	// URL is the instance, such as https://gitlab.com.
	URL string
	// TokenFile holds the read token: a group or project access token, or
	// a bot user's personal access token, with read_api. The client reads
	// it for each call, so a rotated token needs no restart.
	TokenFile string
	// CAFile names extra roots for the instance's certificate; "" trusts
	// the system roots.
	CAFile string
}

// GitLab reads projects through GitLab's REST API, version 4.
type GitLab struct {
	api       string
	tokenFile string
	http      *http.Client
}

// pageSize is the tree listing's page size, GitLab's largest.
const pageSize = 100

// maxDoc bounds one JSON answer, and maxPage one page of a tree listing:
// a page of 100 entries with long paths is well under it.
const (
	maxDoc  = 8 << 20
	maxPage = 1 << 20
)

// maxPages bounds the pages of one tree listing, so a forge that answers
// with empty pages and a next link cannot keep the client reading.
const maxPages = MaxEntries/pageSize + 1

func NewGitLab(o GitLabOptions) (*GitLab, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("forge URL %q is not a URL", trunc(o.URL))
	}
	client := &http.Client{Timeout: CallTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		// A redirect could carry the token to another host.
		return http.ErrUseLastResponse
	}}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read the forge's ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the forge's ca_file %s holds no certificate", o.CAFile)
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	return &GitLab{api: strings.TrimSuffix(o.URL, "/") + "/api/v4", tokenFile: o.TokenFile, http: client}, nil
}

func (g *GitLab) token() (string, error) {
	b, err := os.ReadFile(g.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read the forge's read_token_file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("the forge's read_token_file %s is empty", g.tokenFile)
	}
	return t, nil
}

// call runs fn with the current token and removes the token from any
// error fn returns. Errors carry no text the forge sent, so this is the
// second line of defence.
func (g *GitLab) call(fn func(tok string) error) error {
	tok, err := g.token()
	if err != nil {
		return err
	}
	return redact(fn(tok), tok)
}

// redacted is an error whose text had the token removed. It unwraps to
// ErrNotFound only, so no caller can reach the text it replaced.
type redacted struct {
	msg      string
	notFound bool
}

func (e *redacted) Error() string { return e.msg }

func (e *redacted) Unwrap() error {
	if e.notFound {
		return ErrNotFound
	}
	return nil
}

func redact(err error, tok string) error {
	if err == nil || tok == "" || !strings.Contains(err.Error(), tok) {
		return err
	}
	return &redacted{msg: strings.ReplaceAll(err.Error(), tok, "[REDACTED]"), notFound: errors.Is(err, ErrNotFound)}
}

// echoed reports that an answer holds the token. GitLab never sends the
// token back, so such an answer is refused whole.
func echoed(what string) error {
	return fmt.Errorf("%s: the forge's answer holds the read token, so writ serve refuses it; rotate the token if the forge is not yours", what)
}

// get sends one GET to the API and returns the answer's body and headers,
// reading at most max bytes. A status that is not 200 is an *Error.
func (g *GitLab) get(ctx context.Context, tok, what, p string, q url.Values, max int64) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	target := g.api + p
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", tok)
	req.Header.Set("Accept", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		// The error names the URL, which holds no token.
		return nil, nil, fmt.Errorf("%s: %w", what, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body is the forge's own text, which is never read.
		return nil, nil, &Error{What: what, Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: read the answer: %w", what, err)
	}
	if int64(len(body)) > max {
		return nil, nil, &TooLargeError{What: what + ": the answer", Max: max}
	}
	if bytes.Contains(body, []byte(tok)) {
		return nil, nil, echoed(what)
	}
	return body, resp.Header, nil
}

// getJSON reads one JSON answer of at most max bytes into v.
func (g *GitLab) getJSON(ctx context.Context, tok, what, p string, q url.Values, max int64, v any) (http.Header, error) {
	b, h, err := g.get(ctx, tok, what, p, q, max)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, fmt.Errorf("%s: the answer is not the JSON GitLab sends", what)
	}
	// JSON can spell the token with escapes the raw check misses; the
	// decoded values, encoded again, spell it plainly.
	if again, err := json.Marshal(v); err != nil || bytes.Contains(again, []byte(tok)) {
		return nil, echoed(what)
	}
	return h, nil
}

func projectPath(id string) string { return "/projects/" + url.PathEscape(id) }

// Project reads a project by its path, such as acme/infra, or its id.
func (g *GitLab) Project(ctx context.Context, ref string) (Project, error) {
	var p struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
		WebURL            string `json:"web_url"`
		EmptyRepo         bool   `json:"empty_repo"`
	}
	err := g.call(func(tok string) error {
		_, err := g.getJSON(ctx, tok, "read project "+ref, projectPath(ref), nil, maxDoc, &p)
		return err
	})
	if err != nil {
		return Project{}, err
	}
	if p.ID <= 0 {
		return Project{}, fmt.Errorf("read project %s: the answer has no id", ref)
	}
	if p.EmptyRepo || p.DefaultBranch == "" {
		return Project{}, fmt.Errorf("project %s has no commits, so it has no default branch", p.PathWithNamespace)
	}
	return Project{ID: strconv.FormatInt(p.ID, 10), Path: p.PathWithNamespace, DefaultBranch: p.DefaultBranch, WebURL: p.WebURL}, nil
}

// Head reads the commit a branch points to: one call.
func (g *GitLab) Head(ctx context.Context, projectID, branch string) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	err := g.call(func(tok string) error {
		_, err := g.getJSON(ctx, tok, "read branch "+branch, projectPath(projectID)+"/repository/branches/"+url.PathEscape(branch), nil, maxDoc, &b)
		return err
	})
	if err != nil {
		return "", err
	}
	if !IsSHA(b.Commit.ID) {
		return "", fmt.Errorf("read branch %s: the answer names no commit", branch)
	}
	return b.Commit.ID, nil
}

// Commit checks that a commit exists and returns its full id. It asks
// only for full ids and takes only an answer naming that same commit, so a
// wrong answer cannot swap in another commit's sources.
func (g *GitLab) Commit(ctx context.Context, projectID, sha string) (string, error) {
	if !IsSHA(sha) {
		return "", fmt.Errorf("commit %q is not a full SHA-1", trunc(sha))
	}
	var c struct {
		ID string `json:"id"`
	}
	err := g.call(func(tok string) error {
		_, err := g.getJSON(ctx, tok, "read commit "+sha, projectPath(projectID)+"/repository/commits/"+url.PathEscape(sha), nil, maxDoc, &c)
		return err
	})
	if err != nil {
		return "", err
	}
	if !IsSHA(c.ID) {
		return "", fmt.Errorf("read commit %s: the answer names no commit", sha)
	}
	if c.ID != sha {
		return "", fmt.Errorf("read commit %s: the answer names another commit, %s", sha, Short(c.ID))
	}
	return c.ID, nil
}

// OnRef asks GitLab for the merge base of the commit and the ref: one
// call. The commit is on the ref when it is its own merge base with it.
func (g *GitLab) OnRef(ctx context.Context, projectID, sha, ref string) (bool, error) {
	var c struct {
		ID string `json:"id"`
	}
	q := url.Values{"refs[]": {sha, ref}}
	err := g.call(func(tok string) error {
		_, err := g.getJSON(ctx, tok, "read the merge base of "+Short(sha)+" and "+ref, projectPath(projectID)+"/repository/merge_base", q, maxDoc, &c)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		// The project was found before: 404 means no merge base.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !IsSHA(c.ID) {
		return false, fmt.Errorf("read the merge base of %s and %s: the answer names no commit", Short(sha), ref)
	}
	return c.ID == sha, nil
}

// Tree lists a commit's files under dir, page by page with keyset
// pagination. Only the next page's token is taken from the Link header:
// the client never follows a URL the forge sends. It counts every entry
// the forge lists, whatever the pages hold, and refuses a listing of over
// MaxEntries.
func (g *GitLab) Tree(ctx context.Context, projectID, sha, dir string) ([]File, error) {
	q := url.Values{"ref": {sha}, "recursive": {"true"}, "per_page": {strconv.Itoa(pageSize)}, "pagination": {"keyset"}}
	if dir != "" {
		q.Set("path", dir)
	}
	what := "read the tree of " + Short(sha)
	var out []File
	err := g.call(func(tok string) error {
		listed := 0
		for page := 0; ; page++ {
			if page == maxPages {
				return fmt.Errorf("%s: it takes over %d pages", what, maxPages)
			}
			var entries []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Path string `json:"path"`
				Mode string `json:"mode"`
			}
			h, err := g.getJSON(ctx, tok, what, projectPath(projectID)+"/repository/tree", q, maxPage, &entries)
			if err != nil {
				return err
			}
			if listed += len(entries); listed > MaxEntries {
				return fmt.Errorf("%s: it lists over %d entries", what, MaxEntries)
			}
			for _, e := range entries {
				if e.Type != "blob" {
					continue
				}
				rel, err := under(dir, e.Path)
				if err != nil {
					return fmt.Errorf("%s: %w", what, err)
				}
				if e.Mode == "120000" {
					return fmt.Errorf("%s: %s is a symbolic link, which writ serve does not follow", what, e.Path)
				}
				if !IsSHA(e.ID) {
					return fmt.Errorf("%s: %s has no blob id", what, e.Path)
				}
				out = append(out, File{Path: rel, Blob: e.ID})
			}
			next := nextPageToken(h.Get("Link"))
			if next == "" {
				return nil
			}
			q.Set("page_token", next)
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nextPageToken takes page_token from the rel="next" link of a Link
// header, or "".
func nextPageToken(link string) string {
	for part := range strings.SplitSeq(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
		if err != nil {
			return ""
		}
		return u.Query().Get("page_token")
	}
	return ""
}

// Blob reads a file's content by its blob id.
func (g *GitLab) Blob(ctx context.Context, projectID, blob string, max int64) ([]byte, error) {
	if !IsSHA(blob) {
		return nil, fmt.Errorf("blob id %q is not a SHA-1", trunc(blob))
	}
	var b []byte
	err := g.call(func(tok string) error {
		var err error
		b, _, err = g.get(ctx, tok, "read blob "+Short(blob), projectPath(projectID)+"/repository/blobs/"+blob+"/raw", nil, max)
		return err
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Short is a commit's SHA cut to eight characters, as a message names it.
func Short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
