package tofu

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/mr-karan/writ/internal/runas"
)

// step runs one tofu command in sc's stack directory, with the run's
// credentials when creds is set, and returns its stdout. On an error it
// also returns the stdout tofu wrote before it, up to the limit, which
// holds the -json events of what tofu did.
func (r *Runner) step(ctx context.Context, sc *scratch, creds bool, name string, args ...string) ([]byte, error) {
	var run, home, work *os.File
	err := asUser(sc.user, func() error {
		// O_DIRECTORY makes an open of anything else fail at once: a run
		// could put a FIFO in place of its stack directory, and an open of
		// a FIFO blocks outside the step's timeout.
		var err error
		if run, err = sc.run.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY, 0); err != nil {
			return err
		}
		if home, err = sc.run.OpenFile("home", os.O_RDONLY|syscall.O_DIRECTORY, 0); err != nil {
			return err
		}
		work, err = sc.run.OpenFile(sc.work, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		return err
	})
	for _, f := range []*os.File{run, home, work} {
		if f != nil {
			defer f.Close()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("tofu %s: open the run's directory: %w", name, err)
	}
	// The kernel names the directory the descriptor holds, so HOME is
	// where the run's directory is now, even if the scratch root moved.
	dir, err := os.Readlink(fdPath(run))
	if err != nil {
		return nil, fmt.Errorf("tofu %s: find the run's directory: %w", name, err)
	}
	homeDir := filepath.Join(dir, "home")
	tmp, err := runTmp(homeDir)
	if err != nil {
		return nil, fmt.Errorf("tofu %s: %w", name, err)
	}
	v := view{run: run, dir: dir, home: home, work: sc.work, cache: r.opts.PluginCache, cacheWrite: sc.fill}
	env := append(slices.Clone(sc.env),
		"HOME="+homeDir,
		"TMPDIR="+tmp,
		"TF_CLI_CONFIG_FILE="+os.DevNull,
		"TF_PLUGIN_CACHE_DIR="+r.opts.PluginCache,
		"TF_IN_AUTOMATION=1",
		"TF_INPUT=0",
		"CHECKPOINT_DISABLE=1",
	)
	env = append(env, gitEnv...)
	if creds {
		env = append(env, sc.creds...)
	}
	if r.git != nil {
		v.tools = r.git.files
		env = append(env, r.git.path(sc.env))
	}
	stdout, stderr, err := command{
		binary: r.opts.Binary,
		args:   args,
		view:   v,
		env:    env,
		user:   sc.user,
		net:    sc.net,
		max:    r.opts.MaxOutput,
		stop:   sc.stop,
		plan:   sc.plan,
	}.run(ctx)
	if err == nil {
		return stdout, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return stdout, &StepError{Step: name, Code: exit.ExitCode(), Stdout: stdout, Stderr: stderr}
	}
	return stdout, fmt.Errorf("tofu %s: %w", name, err)
}

// command is one process that tofu's bounds apply to.
type command struct {
	binary string
	args   []string
	view   view
	env    []string
	user   *User
	// net, when set, is the run's own network, which needs a run's
	// directory in the view.
	net *Net
	max int
	// stop, when set, is how long the process has to end after SIGINT
	// when its context's deadline passes, before it is killed.
	stop time.Duration
	// plan, when set, is the sealed saved plan that the process reads at
	// the path sealPlan returned.
	plan *os.File
}

// view is what of a run a tofu process sees, besides its binary and the
// system's files it needs: the run's directory, which it may write, the
// plugin cache, which only Fill may write, and other programs tofu runs,
// such as git. A command with no run's directory starts in /.
type view struct {
	// run is the run's directory, open, and dir the path it had then.
	// home is its home, open.
	run  *os.File
	dir  string
	home *os.File
	// work is tofu's working directory within dir.
	work       string
	cache      string
	cacheWrite bool
	// tools are programs the view holds, with what exec of each loads.
	tools []string
}

// run starts c in a process group of its own, which tofu's providers join,
// and keeps up to c.max bytes of its stdout and of its stderr. A cancel, a
// timeout, or output over the limit kills the group at once, except that
// with c.stop a timeout sends SIGINT and kills only after c.stop, and the
// group is killed when c exits, so no provider outlives it. When c runs as
// a user, every process of that user is then killed too, which catches a
// process that left the group with setsid. Its error is the context's
// cause when there is one, even if c exited 0.
func (c command) run(ctx context.Context) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	full := func() { cancel(ErrOutputLimit) }
	out := &capped{max: c.max, full: full}
	errOut := &capped{max: c.max, full: full}
	cmd := exec.CommandContext(ctx, c.binary, c.args...)
	cmd.Env = c.env
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.Cancel = func() error {
		if c.stop > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return cmd.Process.Signal(os.Interrupt)
		}
		return killGroup(cmd.Process.Pid)
	}
	cmd.WaitDelay = max(time.Second, c.stop)
	stopNet, err := startConfined(cmd, c)
	if err != nil {
		return nil, nil, err
	}
	// The run's network ends after the deferred kill below, once no
	// process of the run is left to use it.
	defer stopNet()
	defer func() {
		// A live member keeps the group's id from being reused after Wait
		// reaps tofu, so this kills only what tofu left behind.
		_ = killGroup(cmd.Process.Pid)
		if c.user != nil {
			if rerr := runas.Reap(c.user.UID); rerr != nil {
				err = errors.Join(err, fmt.Errorf("tofu: end what the run left as %s: %w", c.user, rerr))
			}
		}
	}()
	if c.user != nil {
		if err := runas.CheckIsolated(cmd.Process.Pid, *c.user); err != nil {
			_ = killGroup(cmd.Process.Pid)
			_ = cmd.Wait()
			return nil, nil, err
		}
	}
	err = cmd.Wait()
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	return out.buf, errOut.buf, err
}

func killGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }

// fdPath is the link through which the kernel names the path of the
// directory f holds.
func fdPath(f *os.File) string { return "/proc/self/fd/" + strconv.Itoa(int(f.Fd())) }

// asUser runs fn as u, or as it is when u is nil.
func asUser(u *User, fn func() error) error {
	if u == nil {
		return fn()
	}
	return runas.AsUser(*u, fn)
}

// capped keeps up to max bytes and calls full once when more arrive. It
// accepts every write so the process never blocks on a full pipe before
// it is killed.
type capped struct {
	buf  []byte
	max  int
	over bool
	full func()
}

func (c *capped) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil
	}
	if len(c.buf)+len(p) > c.max {
		c.over = true
		c.full()
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// scratch is one run's directory, which holds home for HOME and TMPDIR,
// and files for the stack's files. It and everything in it belong to the
// run's user. Writ reaches it only through descriptors it opened, never
// by its path.
type scratch struct {
	// root is the scratch root and name the run's directory in it.
	root *os.Root
	name string
	run  *os.Root
	// work is the stack's directory within run.
	work       string
	env        []string
	creds      []string
	stateCreds bool
	user       *User
	net        *Net
	// fill marks Fill's run, the one that may write the plugin cache.
	fill bool
	stop time.Duration
	// plan is the saved plan that sealPlan sealed, which every later step
	// is given.
	plan *os.File
}

// file is one regular file of a Stack's Files.
type file struct {
	name string
	data []byte
	mode fs.FileMode
}

// openScratchRoot opens the scratch root and checks that no other user can
// move it, or a directory above it, and put another in its place. Writ
// must own the root, and root or writ must own each directory above it,
// up to /. Each must be writable only by its owner, or have the sticky
// bit, under which a user may move only its own entries. The check walks
// paths, so it ends by checking that the root's path still leads to the
// directory it opened.
func openScratchRoot(name string) (*os.Root, error) {
	dir, err := filepath.EvalSymlinks(name)
	if err == nil {
		dir, err = filepath.Abs(dir)
	}
	if err != nil {
		return nil, fmt.Errorf("tofu: the scratch root: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("tofu: the scratch root: %w", err)
	}
	if err := checkScratchRoot(root, dir); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func checkScratchRoot(root *os.Root, dir string) error {
	euid := uint32(os.Geteuid())
	for p := dir; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("tofu: the scratch root: %w", err)
		}
		owner := info.Sys().(*syscall.Stat_t).Uid
		mode := info.Mode()
		switch {
		case p == dir && owner != euid:
			return fmt.Errorf("tofu: the scratch root %s belongs to uid %d, not to writ's uid %d", dir, owner, euid)
		case owner != euid && owner != 0:
			return fmt.Errorf("tofu: the scratch root %s is unsafe: %s belongs to uid %d, which may move what it holds; each directory above the root must belong to root or to writ's uid %d", dir, p, owner, euid)
		case mode.Perm()&0o022 != 0 && mode&os.ModeSticky == 0:
			return fmt.Errorf("tofu: the scratch root %s is unsafe: other users may write %s, which has no sticky bit", dir, p)
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	opened, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("tofu: the scratch root: %w", err)
	}
	if info, err := os.Lstat(dir); err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("tofu: the scratch root %s moved while writ checked it", dir)
	}
	return nil
}

// newScratch refuses a User that is root or writ, then makes the run's
// directory under the scratch root and copies s.Files into it, as s.User.
// The directory is mode 0700 and belongs to s.User, so no other run's uid
// can reach into it, and the scratch root keeps other users from moving
// it. The files are read as writ, which may read what the run's user
// cannot.
func (r *Runner) newScratch(s Stack) (*scratch, error) {
	if s.User != nil {
		if err := runas.CheckForeign(s.User.UID); err != nil {
			return nil, fmt.Errorf("tofu: refuse to run as %s: %w", s.User, err)
		}
	}
	if !fs.ValidPath(s.Dir) {
		return nil, fmt.Errorf("tofu: stack directory %q is not a path within its files", s.Dir)
	}
	var dirs []string
	var files []file
	err := fs.WalkDir(s.Files, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			dirs = append(dirs, p)
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("%s is not a regular file or a directory", p)
		}
		data, err := fs.ReadFile(s.Files, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, file{name: p, data: data, mode: info.Mode().Perm() | 0o600})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tofu: read the stack's files: %w", err)
	}
	if s.Lock != nil {
		lock := path.Join(s.Dir, LockFile)
		files = slices.DeleteFunc(files, func(f file) bool { return f.name == lock })
		files = append(files, file{name: lock, data: s.Lock, mode: 0o600})
	}
	sc, err := r.makeScratch(dirs, files, s.Dir, s.Env, s.User)
	if err != nil {
		return nil, err
	}
	sc.creds, sc.stateCreds, sc.net = s.Creds, s.StateCreds, s.Net
	return sc, nil
}

// makeScratch makes the run's directory under the scratch root and writes
// dirs and files into it, as user.
func (r *Runner) makeScratch(dirs []string, files []file, dir string, env []string, user *User) (*scratch, error) {
	sc := &scratch{root: r.root, work: path.Join("files", dir), env: env, user: user}
	err := asUser(user, func() error {
		name := "writ-tofu-" + rand.Text()
		if err := r.root.Mkdir(name, 0o700); err != nil {
			return err
		}
		sc.name = name
		run, err := r.root.OpenRoot(name)
		if err != nil {
			return err
		}
		sc.run = run
		if err := run.Mkdir("home", 0o700); err != nil {
			return err
		}
		for _, d := range dirs {
			if err := run.MkdirAll(path.Join("files", d), 0o700); err != nil {
				return err
			}
		}
		for _, f := range files {
			if err := run.WriteFile(path.Join("files", f.name), f.data, f.mode); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		sc.remove()
		return nil, fmt.Errorf("tofu: make the run's scratch directory: %w", err)
	}
	return sc, nil
}

func (sc *scratch) remove() {
	if sc.run != nil {
		sc.run.Close()
	}
	if sc.plan != nil {
		sc.plan.Close()
	}
	if sc.name == "" {
		return
	}
	_ = asUser(sc.user, func() error { return sc.root.RemoveAll(sc.name) })
}

// read reads the file name of the stack's directory, as the run's user,
// refusing one over max bytes.
func (sc *scratch) read(name string, max int) ([]byte, error) {
	var data []byte
	err := asUser(sc.user, func() error {
		// O_NONBLOCK keeps an open of a FIFO the run left in the file's
		// place from blocking; only a regular file is read.
		f, err := sc.run.OpenFile(path.Join(sc.work, name), os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		if fi, err := f.Stat(); err != nil {
			return err
		} else if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is a %s, not a file", name, fi.Mode().Type())
		}
		data, err = io.ReadAll(io.LimitReader(f, int64(max)+1))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("tofu: read %s: %w", name, err)
	}
	if len(data) > max {
		return nil, fmt.Errorf("tofu: %s: %w", name, ErrOutputLimit)
	}
	return data, nil
}

// write writes a file of writ's into the stack's directory, as the run's
// user.
func (sc *scratch) write(name string, data []byte) error {
	err := asUser(sc.user, func() error {
		return sc.run.WriteFile(path.Join(sc.work, name), data, 0o600)
	})
	if err != nil {
		return fmt.Errorf("tofu: write %s: %w", name, err)
	}
	return nil
}
