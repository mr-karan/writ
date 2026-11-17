package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// GitCmd is one git command. Unless Inherit is set, it runs hardened: it
// starts no program that the repository's config names, and reads no
// config but the repository's. This is how writ runs git on a repository
// that someone other than the person who runs writ can write, such as a
// project directory an assistant works in.
type GitCmd struct {
	// Dir is the directory git runs in, given to git as -C.
	Dir string
	// Env holds variables added to the environment, such as GIT_DIR.
	Env []string
	// ExtraFiles become descriptors 3 and up in git.
	ExtraFiles []*os.File
	// Max bounds what git may print on stdout. Past it, the command fails
	// with ErrGitOutputTooLong.
	Max int
	// Timeout bounds the command. Zero means CallTimeout.
	Timeout time.Duration
	// Inherit runs git with writ's environment and every config git reads,
	// and none of the hardening. It is for a work tree the person runs
	// writ in by hand, where their own config applies.
	Inherit bool
}

// hardenedGitArgs come before every hardened git command. A hook or a
// core.fsmonitor program that the repository's config names does not run,
// and git takes no optional lock.
var hardenedGitArgs = []string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}

// hardenedGitEnv is added to writ's environment, with every GIT_ variable
// removed, such as GIT_CONFIG_PARAMETERS or GIT_ALTERNATE_OBJECT_DIRECTORIES.
//
// A partial clone fetches a missing object from its promisor remote, and
// the fetch runs a program the repository's config can name:
// remote.NAME.uploadpack, core.sshCommand, or an ext:: URL. An empty
// GIT_ALLOW_PROTOCOL lets git use no transport at all, and it overrides
// every protocol.NAME.allow in the repository's config, so such a fetch
// fails before it connects, on every git version. Git 2.44 and later also
// honour GIT_NO_LAZY_FETCH, and do not start the fetch. The option
// --no-lazy-fetch does the same, but git before 2.44 refuses it as an
// unknown option, so writ sets only the variable.
//
// A file in .git/refs/replace makes git serve another object under an
// object's id, so a commit would read another commit's tree.
// GIT_NO_REPLACE_OBJECTS makes git read each object by its own id.
var hardenedGitEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1",
	"GIT_OPTIONAL_LOCKS=0", "GIT_LITERAL_PATHSPECS=1", "GIT_TERMINAL_PROMPT=0",
}

// gitWaitDelay bounds how long the call waits for git's output after git
// exits or is killed. A process git started can hold stdout open after
// that, and the call does not wait for it.
const gitWaitDelay = 2 * time.Second

// gitStderrMax bounds the error text a git command keeps.
const gitStderrMax = 4 << 10

// ErrGitOutputTooLong is the error of a git command that printed more
// than its GitCmd.Max.
var ErrGitOutputTooLong = errors.New("the output is too long")

// GitError is the error of a git command that failed, timed out, or left
// its output open.
type GitError struct {
	Args []string
	Err  error
	// Stderr is what git printed on stderr, trimmed, at most 4 KiB.
	Stderr string
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *GitError) Unwrap() error { return e.Err }

// Run runs git with args and returns what it printed on stdout. With no
// git on PATH it returns ErrNoGit.
func (c GitCmd) Run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = CallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	gitArgs := append([]string{"-C", c.Dir}, args...)
	if !c.Inherit {
		gitArgs = slices.Concat(hardenedGitArgs, gitArgs)
	}
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.WaitDelay = gitWaitDelay
	cmd.ExtraFiles = c.ExtraFiles
	if !c.Inherit || len(c.Env) > 0 {
		env := os.Environ()
		if !c.Inherit {
			env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
			env = append(env, hardenedGitEnv...)
		}
		cmd.Env = append(env, c.Env...)
	}
	out, errb := &cappedBuffer{max: c.Max}, &cappedBuffer{max: gitStderrMax, drop: true}
	cmd.Stdout, cmd.Stderr = out, errb
	err := cmd.Run()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, ErrNoGit
	case out.over:
		return nil, fmt.Errorf("git %s: %w: over %d bytes", strings.Join(args, " "), ErrGitOutputTooLong, c.Max)
	case err != nil:
		return nil, &GitError{Args: args, Err: err, Stderr: strings.TrimSpace(errb.buf.String())}
	}
	// An empty file is content, not an absent one, so it is never nil.
	if out.buf.Len() == 0 {
		return []byte{}, nil
	}
	return out.buf.Bytes(), nil
}

// cappedBuffer keeps at most max bytes. Past that, it fails the write, so
// git gets a broken pipe, or with drop set, discards the rest. It holds
// its buffer in a field: an embedded bytes.Buffer would give it ReadFrom,
// which io.Copy calls in place of Write, past the bound.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	drop bool
	over bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(p) > room {
		c.over = true
		if !c.drop {
			return 0, ErrGitOutputTooLong
		}
		c.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.buf.Write(p)
}
