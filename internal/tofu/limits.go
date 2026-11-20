package tofu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Limits bound what each step of a run may use. A zero field bounds
// nothing.
type Limits struct {
	// Memory bounds the memory of the step's processes together, in
	// bytes, with no swap where the kernel accounts swap to cgroups.
	// Past it the kernel kills every one of them.
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
	// StepsPids and StepsMemory bound the processes and threads, and the
	// bytes of memory, of every step together: the limits of the cgroup
	// writ serve runs in, less what it keeps for itself. 0 bounds
	// nothing.
	StepsPids, StepsMemory int64
	// SwapOff is why a step may swap past Limits' Memory, "" when the
	// kernel holds each step to no swap.
	SwapOff string
	// Seccomp reports whether each tofu process runs under writ's seccomp
	// filter. SeccompOff says why not.
	Seccomp    bool
	SeccompOff string
}

// cgroupSetup is what writ found of the cgroups, or made of them.
type cgroupSetup struct {
	// base is the cgroup under which each step gets a cgroup of its own.
	base string
	// pids and memory are base's pids.max and memory.max, which every
	// step shares, or 0 for no bound.
	pids, memory int64
	// swapOff is why a step may swap past its memory limit, or "".
	swapOff string
}

// Enforcement reports what r enforces.
func (r *Runner) Enforcement() Enforcement {
	e := Enforcement{Cgroup: r.cgroups.base, CgroupOff: r.cgroupOff, StepsPids: r.cgroups.pids, StepsMemory: r.cgroups.memory, SwapOff: r.cgroups.swapOff}
	e.Seccomp, e.SeccompOff = seccompStatus()
	return e
}

// CheckLimits reports what a Runner with limits l would enforce in this
// process, as writ serve --check prints it. It moves no process of its
// own; it starts a probe in a cgroup it makes and removes, and, as root,
// it mounts an absent cgroup filesystem, as New would.
func CheckLimits(l Limits) Enforcement {
	var e Enforcement
	if l.cgroup() {
		if set, err := checkCgroups(); err != nil {
			e.CgroupOff = err.Error()
		} else {
			e.Cgroup, e.StepsPids, e.StepsMemory, e.SwapOff = set.base, set.pids, set.memory, set.swapOff
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
// step runs, at most: it waits at least scratchSpacing times as long as
// the last measure took, so a directory slow to measure costs writ no
// more than a fraction of a CPU.
const (
	scratchEvery   = 500 * time.Millisecond
	scratchSpacing = 4
)

// scratchWithin bounds one measure of the run's directory. A measure that
// takes longer stops the step, as a directory over the limit does: a run
// that makes its directory too slow to measure must not escape the limit.
const scratchWithin = 10 * time.Second

// scratchInode is the least that a measure charges each file and
// directory, the size of a block on most filesystems: a file that holds
// no data still costs the filesystem an inode, and a run of empty files
// must still meet limits.run_scratch.
const scratchInode = 4096

// scratchDepth is how deeply a measure descends into the run's directory.
// Each level holds a descriptor open while the measure is below it.
const scratchDepth = 256

// errScratch is the cause with which writ ends a step whose run's
// directory holds more than Limits.Scratch.
type errScratch struct{ msg string }

func (e *errScratch) Error() string { return e.msg }

// watchScratch measures sc's directory every scratchEvery, or
// scratchSpacing times the last measure's length if longer, while the
// step runs, and ends the step through the context it returns once the
// directory holds more than r's Limits.Scratch. The function it returns
// stops the watch and returns what the step reached, if anything: the
// watch's finding, or a last measure, exact once no process of the step
// is left.
func (r *Runner) watchScratch(ctx context.Context, sc *scratch, cg *stepCgroup) (context.Context, func() string) {
	limit := r.opts.Limits.Scratch
	if limit <= 0 {
		return ctx, func() string { return "" }
	}
	ctx, cancel := context.WithCancelCause(ctx)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTimer(scratchEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			start := time.Now()
			if msg := sc.overScratch(limit, cg); msg != "" {
				cancel(&errScratch{msg})
				return
			}
			t.Reset(max(scratchEvery, scratchSpacing*time.Since(start)))
		}
	}()
	return ctx, func() string {
		close(stop)
		defer cancel(nil)
		select {
		case <-done:
		case <-time.After(2 * scratchWithin):
			// Each measure ends by scratchWithin, so this is a bound
			// that only a measure stuck in the kernel reaches.
			return slowScratch
		}
		if e, ok := errors.AsType[*errScratch](context.Cause(ctx)); ok {
			return e.msg
		}
		return sc.overScratch(limit, cg)
	}
}

// slowScratch is why writ stopped a step whose run's directory it could
// not measure within scratchWithin.
var slowScratch = fmt.Sprintf("writ could not measure the run's directory, which limits.run_scratch bounds, within %s, so it stopped the step", scratchWithin)

// errScratchSlow is a measure that passed scratchWithin.
var errScratchSlow = errors.New("the measure took too long")

// heldMapsMax bounds the bytes of /proc/PID/maps that one measure reads,
// of every process of the step together. Each line is as long as the path
// of the file it maps, which a run may make some 60KB long, and a process
// may have 65,530 mappings: a measure past it stops the step, as one past
// the limit does. A large process, such as a desktop's web browser, has
// some 2,000 mappings and 200KB of maps.
const heldMapsMax = 16 << 20

// errScratchMaps is a measure that read heldMapsMax of the step's maps.
var errScratchMaps = errors.New("the step's processes map too much to read")

// errScratchDeep is a run's directory nested deeper than scratchDepth.
var errScratchDeep = errors.New("the directory nests too deeply")

// overScratch is why sc's directory, with the removed files that cg's
// processes hold, is over max bytes, or "". The words are writ's own: a
// name in the directory is the run's to choose, and may hold a secret.
func (sc *scratch) overScratch(max int64, cg *stepCgroup) string {
	n, err := sc.usage(max, cg, time.Now().Add(scratchWithin))
	switch {
	case errors.Is(err, errScratchSlow):
		return slowScratch
	case errors.Is(err, errScratchMaps):
		return fmt.Sprintf("the step's processes map more than writ reads of them, %s of /proc/PID/maps, to measure the removed files they hold for limits.run_scratch, so it stopped the step", formatSize(heldMapsMax))
	case errors.Is(err, errScratchDeep):
		return fmt.Sprintf("the run's directory nests more than %d directories deep, past which writ does not measure it for limits.run_scratch, so it stopped the step", scratchDepth)
	case err != nil:
		why := "it could not be read"
		if errno, ok := errors.AsType[syscall.Errno](err); ok {
			why = errno.Error()
		}
		return fmt.Sprintf("writ could not measure the run's directory, which limits.run_scratch bounds (%s), so it stopped the step; a run's files must stay readable by its own user", why)
	case n > max:
		return fmt.Sprintf("the run's directory held more than limits.run_scratch, %s, on disk", formatSize(max))
	}
	return ""
}

// usage returns the bytes on disk of sc's directory, read as the run's
// user, counting each file and directory as at least scratchInode, a
// file with several links once in all, as scratchWalk.charge does. With cg, it adds the files that no
// directory names any longer but cg's threads hold open or mapped: a run
// could write into such a file for as long as the step runs. It stops
// once the count passes max, and fails with errScratchSlow past deadline,
// or errScratchMaps past what it reads of the step's mappings, each of
// which a step's measure counts as over max.
func (sc *scratch) usage(max int64, cg *stepCgroup, deadline time.Time) (int64, error) {
	w := scratchWalk{max: max, deadline: deadline}
	err := asUser(sc.user, func() error {
		f, err := sc.run.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			return err
		}
		if err := w.charge(&st); err != nil {
			return err
		}
		return w.dir(f, 0)
	})
	if errors.Is(err, errScratchOver) {
		return w.total, nil
	}
	if err != nil || cg == nil {
		return w.total, err
	}
	held, err := cg.heldRemoved(sc.user, max-w.total, deadline)
	return w.total + held, err
}

// errScratchOver ends a walk whose count passed its max.
var errScratchOver = errors.New("over the limit")

// scratchWalk is one measure of a run's directory. It reads each
// directory through a descriptor of its own, opened from its parent's
// with O_DIRECTORY and O_NOFOLLOW, so a run that swaps a directory for a
// FIFO or a symbolic link while writ walks makes the open fail at once
// rather than block or lead out of the run's directory. It reads names a
// batch at a time and keeps none, so a directory of many files costs writ
// no more memory than one of few.
type scratchWalk struct {
	max      int64
	deadline time.Time
	total    int64
}

// charge adds st's blocks, at least scratchInode, to the count, and fails
// once the count passes max. A file with n links costs a nth of that,
// rounded up, at each, so it costs at least the whole once the walk has
// met every link, and the walk keeps no record of the files it met. A
// link outside the run's directory, of a file the run's user owns
// elsewhere on the filesystem, is a share the measure doesn't count.
func (w *scratchWalk) charge(st *unix.Stat_t) error {
	n := max(int64(st.Blocks)*512, scratchInode)
	if links := int64(st.Nlink); links > 1 && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		n = (n + links - 1) / links
	}
	w.total += n
	if w.total > w.max {
		return errScratchOver
	}
	return nil
}

// dir charges each entry of the directory f, depth levels below the
// run's directory, and walks each directory among them.
func (w *scratchWalk) dir(f *os.File, depth int) error {
	if depth >= scratchDepth {
		return errScratchDeep
	}
	for {
		if time.Now().After(w.deadline) {
			return errScratchSlow
		}
		names, err := f.Readdirnames(256)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := w.entry(int(f.Fd()), name, depth); err != nil {
				return err
			}
		}
	}
}

// entry charges the entry name of the directory fd, and walks it if it is
// a directory. An entry that the run removed or swapped for another kind
// of file since writ listed it is charged as what it is now, or skipped
// once gone: the next measure finds it.
func (w *scratchWalk) entry(fd int, name string, depth int) error {
	var st unix.Stat_t
	err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return w.charge(&st)
	}
	sub, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil
	case errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP):
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil
		}
		return w.charge(&st)
	case err != nil:
		return err
	}
	f := os.NewFile(uintptr(sub), name)
	defer f.Close()
	if err := unix.Fstat(sub, &st); err != nil {
		return err
	}
	if err := w.charge(&st); err != nil {
		return err
	}
	return w.dir(f, depth+1)
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
