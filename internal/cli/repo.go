package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/server"
)

// repoAddTimeout bounds writ repo add, whose request reads every file of
// the default branch's stacks the first time.
const repoAddTimeout = 2*time.Minute + 10*time.Second

var repoNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// RepoAddOptions are the arguments of `writ repo add`.
type RepoAddOptions struct {
	JSON bool
	// Name names the repository; empty takes it from the directory.
	Name string
	// Location is a directory, for a local Git repository under writ serve
	// --dev, or FORGE/PROJECT, where FORGE is a forge's name or host.
	Location string
	// Path is the directory in the project that holds the stacks. For a
	// directory, empty means that directory.
	Path   string
	Prefix string
}

// isDir reports whether a repo add location names a directory.
func isDir(loc string) bool {
	return loc == "." || loc == ".." || strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "./") || strings.HasPrefix(loc, "../")
}

// RepoAdd is `writ repo add`: the control plane reads the default branch,
// finds the stacks, and keeps the repository.
func (a *App) RepoAdd(ctx context.Context, o RepoAddOptions) (err error) {
	defer escapeError(&err, o.JSON)
	req := server.RepoRequest{Format: server.FormatRepoRequest, Name: o.Name, Path: o.Path, Prefix: o.Prefix}
	if isDir(o.Location) {
		dir, err := filepath.Abs(o.Location)
		if err != nil {
			return err
		}
		top, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
		if errors.Is(err, forge.ErrNoGit) {
			return err
		}
		if err != nil {
			return usagef("%s is not in a Git work tree: writ repo add reads commits, so commit the stacks first", dir)
		}
		top = canonical(top)
		if req.Path == "" {
			root := canonical(dir)
			rel, err := filepath.Rel(top, root)
			if err != nil || strings.HasPrefix(rel, "..") {
				return usagef("%s is outside the Git work tree %s", root, top)
			}
			if req.Path = filepath.ToSlash(rel); req.Path == "." {
				req.Path = ""
			}
		}
		if req.Name == "" {
			req.Name = strings.ToLower(filepath.Base(path.Join(top, req.Path)))
			if !repoNameRE.MatchString(req.Name) {
				return usagef("the directory's name %q is not a repository name; name it: writ repo add NAME %s", req.Name, o.Location)
			}
		}
		req.Forge, req.Project = "dir", top
	} else {
		forge, project, ok := strings.Cut(o.Location, "/")
		if !ok || forge == "" || project == "" {
			return usagef("repo add takes a directory, such as ., or FORGE/PROJECT, such as gitlab.com/acme/infra, got %q", o.Location)
		}
		if req.Name == "" {
			return usagef("name the repository: writ repo add NAME %s", o.Location)
		}
		req.Forge, req.Project = forge, strings.TrimSuffix(project, ".git")
	}
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	status, resp, err := r.call(ctx, http.MethodPost, "/v1/repos", body, repoAddTimeout)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return a.remoteFailed(o.JSON, resp)
	}
	return a.printRepo(o.JSON, resp, true)
}

// RepoShow is `writ repo show NAME`: the repository and the stacks found at
// the head of its default branch.
func (a *App) RepoShow(ctx context.Context, name string, asJSON bool) (err error) {
	defer escapeError(&err, asJSON)
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	doc, err := r.get(ctx, "/v1/repos/"+url.PathEscape(name), remoteTimeout)
	if re, ok := errors.AsType[*remoteError](err); ok {
		return a.remoteFailed(asJSON, re.doc)
	}
	if err != nil {
		return err
	}
	return a.printRepo(asJSON, doc, false)
}

func (a *App) printRepo(asJSON bool, doc []byte, added bool) error {
	if asJSON {
		_, err := a.opts.Stdout.Write(doc)
		return err
	}
	var d server.RepoDoc
	if err := json.Unmarshal(doc, &d); err != nil || d.Format != server.FormatRepo {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatRepo)
	}
	out := a.humanOutput()
	rp := d.Repo
	where := rp.Project
	if rp.Kind != "dir" {
		where = hostOf(rp.ForgeURL) + "/" + rp.Project + " (project " + rp.ProjectID + ")"
	}
	fmt.Fprintf(out, "Repository %s: %s, path %s, branch %s.\n", rp.Name, escapeHuman(where, false), dirWords(rp.Path), escapeHuman(rp.DefaultBranch, false))
	if c := d.Commit; c != nil {
		how := "with the read-only token of forge " + rp.Forge
		if rp.Kind == "dir" {
			how = "from the local repository"
		}
		fmt.Fprintf(out, "Read %s at %s %s: %s, %s.\n", escapeHuman(rp.DefaultBranch, false), forge.Short(c.SHA), how, plural(c.Files, "file"), plural(len(d.Stacks), "stack"))
	}
	if rp.Error != "" {
		fmt.Fprintf(out, "The last poll, %s, failed: %s\n", rp.ErrorAt, escapeHuman(rp.Error, false))
	}
	for _, dg := range d.Diagnostics {
		fmt.Fprintf(out, "%s: %s\n", dg.Severity, escapeHuman(dg.Message, false))
	}
	if len(d.Stacks) > 0 {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "STACK\tSERVED")
		for _, s := range d.Stacks {
			served := dash(strings.Join(s.Served, ", "))
			if len(s.NotServed) > 0 {
				served += " (not served: " + strings.Join(s.NotServed, ", ") + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\n", escapeHuman(s.Stack, true), served)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if added {
		if rp.Kind == "dir" {
			fmt.Fprintln(out, "Next: in a clean work tree of it, writ plan --remote plans the commit you are on.")
		} else {
			fmt.Fprintf(out, "Next: add the CI job that plans each merge request: writ ci gitlab --repo %s\n", rp.Name)
		}
	}
	return nil
}

func dirWords(p string) string {
	if p == "" {
		return "the root"
	}
	return p + "/"
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

// RepoList is `writ repo list`.
func (a *App) RepoList(ctx context.Context, asJSON bool) (err error) {
	defer escapeError(&err, asJSON)
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	doc, err := r.get(ctx, "/v1/repos", remoteTimeout)
	if re, ok := errors.AsType[*remoteError](err); ok {
		return a.remoteFailed(asJSON, re.doc)
	}
	if err != nil {
		return err
	}
	if asJSON {
		_, err := a.opts.Stdout.Write(doc)
		return err
	}
	var d server.ReposDoc
	if err := json.Unmarshal(doc, &d); err != nil || d.Format != server.FormatRepos {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatRepos)
	}
	out := a.humanOutput()
	if len(d.Repos) == 0 {
		fmt.Fprintln(out, "No repository. Add one: writ repo add NAME HOST/PROJECT --path PATH, or writ repo add . under writ serve --dev.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPROJECT\tPATH\tBRANCH\tHEAD\tSTACKS\tLAST POLL")
	for _, rp := range d.Repos {
		n := "-"
		if rp.Stacks != nil {
			n = strconv.Itoa(*rp.Stacks)
		}
		poll := rp.PolledAt
		if rp.Error != "" {
			poll += " failed: " + rp.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", rp.Name, escapeHuman(rp.Project, true), dirWords(rp.Path), escapeHuman(rp.DefaultBranch, true), dash(forge.Short(rp.Head)), n, escapeHuman(dash(poll), true))
	}
	return tw.Flush()
}
