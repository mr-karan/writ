// Package forge reads repositories where they are hosted: a project, the
// head of a branch, the files of a commit, and a file's content by its Git
// blob id. GitLab and GitHub are read through their REST APIs with a
// read-only token, and under writ serve --dev a local Git repository
// through the git binary.
//
// The package owns each forge's wire format. Callers see the types below,
// and never the token, which only the client holds and only a request
// header carries.
package forge

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CallTimeout bounds each call to a forge.
const CallTimeout = 10 * time.Second

// MaxEntries bounds the files one tree lists, so a huge repository cannot
// fill memory. The estate's own files have tighter bounds where they are
// fetched.
const MaxEntries = 20000

// Project is a repository at a forge.
type Project struct {
	// ID is the forge's numeric id, which every later call uses. For a
	// local repository it is the absolute path of its work tree.
	ID string
	// Path is the project's path, such as acme/infra.
	Path          string
	DefaultBranch string
	WebURL        string
}

// File is one regular file of a commit, named from the path the tree was
// listed under.
type File struct {
	Path string
	// Blob is the file's Git blob id: the SHA-1 of its Git object.
	Blob string
}

// Client reads one forge.
type Client interface {
	// Project finds a project by its path or its id.
	Project(ctx context.Context, ref string) (Project, error)
	// Head returns the commit a branch points to.
	Head(ctx context.Context, projectID, branch string) (string, error)
	// Commit returns the full id of a commit, which must exist.
	Commit(ctx context.Context, projectID, sha string) (string, error)
	// OnRef reports whether commit sha is the commit ref names, a full
	// ref such as refs/heads/main, or an ancestor of it.
	OnRef(ctx context.Context, projectID, sha, ref string) (bool, error)
	// Tree lists the regular files of a commit under dir, named from dir.
	// A symbolic link is an error, and a submodule is left out.
	Tree(ctx context.Context, projectID, sha, dir string) ([]File, error)
	// Blob returns a file's content by its blob id, up to max bytes. A
	// longer blob is a *TooLargeError.
	Blob(ctx context.Context, projectID, blob string, max int64) ([]byte, error)
}

// TooLargeError is a read whose answer is over the bytes it may take.
type TooLargeError struct {
	What string
	Max  int64
}

func (e *TooLargeError) Error() string { return fmt.Sprintf("%s is over %d bytes", e.What, e.Max) }

// ErrNotFound means the forge has no such project, branch, or commit, or
// the token may not see it.
var ErrNotFound = errors.New("not found")

// Error is a forge's refusal of a call. It holds no text the forge sent:
// a hostile or broken forge could echo the token in its message, so the
// status alone picks Writ's own reason.
type Error struct {
	// What names the call, such as "read the tree of 5d1e9a0c".
	What   string
	Status int
	// Need is what the read token needs, which a 403 names.
	Need string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: the forge answered %d", e.What, e.Status)
	switch {
	case e.Status == 401:
		msg += "; it refused the read token, so check the forge's read token"
	case e.Status == 301 || e.Status == 302 || e.Status == 307 || e.Status == 308:
		msg += "; writ serve follows no redirect, so add a renamed or moved repository again by its new path"
	case e.Status == 403:
		msg += "; the token needs " + e.Need
	case e.Status == 404:
		msg += "; it has no such thing, or the token may not see it"
	case e.Status >= 500:
		msg += "; the forge failed"
	}
	return msg
}

func (e *Error) Unwrap() error {
	if e.Status == 404 {
		return ErrNotFound
	}
	return nil
}

// RateLimitError is a forge that limits the rate of calls until Until.
// The client sends it no call before then: Status is the answer that
// said so, or 0 for a call the client held back.
type RateLimitError struct {
	What   string
	Status int
	Until  time.Time
}

func (e *RateLimitError) Error() string {
	until := e.Until.UTC().Format(time.RFC3339)
	if e.Status == 0 {
		return fmt.Sprintf("%s: the forge limits the rate of calls until %s, so writ serve sends it none before then", e.What, until)
	}
	return fmt.Sprintf("%s: the forge answered %d; it limits the rate of calls until %s, so writ serve sends it none before then", e.What, e.Status, until)
}

// BlobID is the Git blob id of content: the SHA-1 of "blob SIZE\x00" and
// the content, Git's object format.
func BlobID(content []byte) string {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsSHA reports whether s is a full commit or blob id.
func IsSHA(s string) bool { return shaRE.MatchString(s) }

// CheckDir refuses a directory inside a project that is not "" or a
// clean relative slash path.
func CheckDir(dir string) error {
	if dir == "" {
		return nil
	}
	if len(dir) > 512 || strings.ContainsAny(dir, "\\\x00") || path.IsAbs(dir) || path.Clean(dir) != dir || dir == "." ||
		dir == ".." || strings.HasPrefix(dir, "../") {
		return fmt.Errorf("path %q must be a clean relative path in the project, such as infra, or empty for its root", trunc(dir))
	}
	return nil
}

// under names a file of the project relative to dir, and refuses one that
// is not under dir or not a clean relative path. A refusal leaves the name
// out: it is the forge's text, not yet checked.
func under(dir, name string) (string, error) {
	rel := name
	if dir != "" {
		var ok bool
		if rel, ok = strings.CutPrefix(name, dir+"/"); !ok {
			return "", fmt.Errorf("the tree of %s lists a path that is not under it", dir)
		}
	}
	if rel == "" || strings.ContainsAny(rel, "\\\x00") || path.Clean(rel) != rel || strings.HasPrefix(rel, "../") || rel == ".." || path.IsAbs(rel) {
		return "", errors.New("the tree lists a path that is not a clean relative path")
	}
	return rel, nil
}

func trunc(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
