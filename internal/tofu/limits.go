package tofu

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"syscall"
	"time"
)

// Limits bound what each step of a run may use. A zero field bounds
// nothing.
type Limits struct {
	// Memory bounds the memory of the step's processes together, in
	// bytes, with no swap. Past it the kernel kills every one of them.
	Memory int64
	// Pids bounds the processes and threads of the step together.
	Pids int
	// CPUs bounds the CPU time of the step's processes together, in
	// CPUs, such as 2 or 0.5.
	CPUs float64
	// Scratch bounds the bytes on disk of the run's directory, which
	// holds the stack's files, HOME, and TMPDIR.
	Scratch int64
}

// cgroup reports whether l sets a limit that a cgroup holds.
func (l Limits) cgroup() bool { return l.Memory > 0 || l.Pids > 0 || l.CPUs > 0 }

// Enforcement is what a Runner enforces of its Limits and of the seccomp
// filter, and why not.
type Enforcement struct {
	// Cgroup is the cgroup under which each step gets one of its own,
	// which holds Limits' Memory, Pids, and CPUs. It is "" when they are
	// not enforced, and CgroupOff says why.
	Cgroup    string
	CgroupOff string
	// Seccomp reports whether each tofu process runs under writ's seccomp
	// filter. SeccompOff says why not.
	Seccomp    bool
	SeccompOff string
}

// Enforcement reports what r enforces.
func (r *Runner) Enforcement() Enforcement {
	e := Enforcement{Cgroup: r.cgroups, CgroupOff: r.cgroupOff}
	e.Seccomp, e.SeccompOff = seccompStatus()
	return e
}

// CheckLimits reports what a Runner with limits l would enforce in this
// process, as writ serve --check prints it. It makes no cgroup and moves
// no process; as root, it mounts an absent cgroup filesystem, as New
// would.
func CheckLimits(l Limits) Enforcement {
	var e Enforcement
	if l.cgroup() {
		if base, err := cgroupBase(false); err != nil {
			e.CgroupOff = err.Error()
		} else {
			e.Cgroup = base
		}
	}
	e.Seccomp, e.SeccompOff = seccompStatus()
	return e
}

func seccompStatus() (bool, string) {
	if err := seccompReady(); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// scratchEvery is how often writ measures the run's directory while a
// step runs.
const scratchEvery = 500 * time.Millisecond

// errScratch is the cause with which writ ends a step whose run's
// directory holds more than Limits.Scratch.
type errScratch struct{ msg string }

func (e *errScratch) Error() string { return e.msg }

// watchScratch measures sc's directory every scratchEvery while the step
// runs, and ends the step through the context it returns once the
// directory holds more than r's Limits.Scratch. The function it returns
// stops the watch and returns what the step reached, if anything: the
// watch's finding, or a last measure, exact once no process of the step
// is left.
func (r *Runner) watchScratch(ctx context.Context, sc *scratch) (context.Context, func() string) {
	max := r.opts.Limits.Scratch
	if max <= 0 {
		return ctx, func() string { return "" }
	}
	ctx, cancel := context.WithCancelCause(ctx)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(scratchEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if msg := sc.overScratch(max); msg != "" {
				cancel(&errScratch{msg})
				return
			}
		}
	}()
	return ctx, func() string {
		close(stop)
		<-done
		defer cancel(nil)
		if e, ok := errors.AsType[*errScratch](context.Cause(ctx)); ok {
			return e.msg
		}
		return sc.overScratch(max)
	}
}

// overScratch is why sc's directory is over max bytes, or "". The words
// are writ's own: a name in the directory is the run's to choose, and may
// hold a secret.
func (sc *scratch) overScratch(max int64) string {
	n, err := sc.usage()
	if err != nil {
		why := "it could not be read"
		if errno, ok := errors.AsType[syscall.Errno](err); ok {
			why = errno.Error()
		}
		return fmt.Sprintf("writ could not measure the run's directory, which limits.run_scratch bounds (%s), so it stopped the step; a run's files must stay readable by its own user", why)
	}
	if n > max {
		return fmt.Sprintf("the run's directory held %s on disk, more than limits.run_scratch, %s", formatSize(n), formatSize(max))
	}
	return ""
}

// usage returns the bytes on disk of sc's directory, counting each file
// with hard links once, read as the run's user.
func (sc *scratch) usage() (int64, error) {
	var total int64
	seen := map[[2]uint64]bool{}
	err := asUser(sc.user, func() error {
		return fs.WalkDir(sc.run.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				// Removed while writ walked.
				return nil
			}
			if err != nil {
				return err
			}
			info, err := sc.run.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				total += info.Size()
				return nil
			}
			if st.Nlink > 1 && !info.IsDir() {
				id := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
				if seen[id] {
					return nil
				}
				seen[id] = true
			}
			total += int64(st.Blocks) * 512
			return nil
		})
	})
	return total, err
}

// formatSize writes n bytes as the config writes a size, in GiB, MiB, or
// KiB, to one decimal place where it is not a whole number of GiB or MiB.
func formatSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return strconv.FormatInt(n>>30, 10) + "GiB"
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(n)/(1<<30))
	case n >= 1<<20 && n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + "MiB"
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10 && n%(1<<10) == 0:
		return strconv.FormatInt(n>>10, 10) + "KiB"
	}
	return strconv.FormatInt(n, 10) + "B"
}
