package tofu

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The cgroup v2 controllers every step's cgroup needs: memory and pids
// hold Limits' Memory and Pids, and cpu its CPUs.
var cgroupControllers = []string{"memory", "pids", "cpu"}

// cgroupMount is where systemd and most hosts mount the cgroup v2
// filesystem, and where writ mounts it as root when nothing is mounted
// there, as in a task of Nomad's exec driver, which mounts /sys but no
// cgroup filesystem.
const cgroupMount = "/sys/fs/cgroup"

// serveCgroup is the leaf cgroup that writ moves itself into, within the
// cgroup it started in, so that cgroup may give its children controllers:
// cgroup v2 lets a cgroup other than the root enable controllers for its
// children only while it holds no process.
const serveCgroup = "serve"

// runsCgroup is the cgroup, beside serveCgroup, that holds every step's
// cgroup, so that the steps together stay within the limits it sets.
const runsCgroup = "runs"

// What writ serve keeps for itself of the processes and memory of the
// cgroup it started in, which it shares with every step: runsCgroup gets
// the rest, so steps that use all of theirs leave writ serve room for Go's
// runtime threads, its heap, and the processes it starts outside a step,
// such as git and each step's helper before it moves into its cgroup.
const (
	servePids   = 128
	serveMemory = 512 << 20
)

// setUpCgroups makes runsCgroup the base of every step's cgroup, once for
// the process.
var setUpCgroups = sync.OnceValues(func() (cgroupSetup, error) { return cgroupBase(true) })

// cgroupBase finds the cgroup this process runs in and checks that this
// process may make children of it with cgroupControllers: that the cgroup
// is delegated to writ's uid, offers the controllers, and holds no other
// process, as systemd's Delegate=yes gives a unit. It works out the limits
// of runsCgroup from the cgroup's. With apply, it moves this process into
// serveCgroup, enables the controllers, and makes runsCgroup with those
// limits. Without, it changes nothing but, as root, the mount of an absent
// cgroup filesystem.
func cgroupBase(apply bool) (cgroupSetup, error) {
	var none cgroupSetup
	own, err := ownCgroup()
	if err != nil {
		return none, err
	}
	if !apply && filepath.Base(own) == ".control" {
		// systemd runs a delegated unit's ExecStartPre, which runs writ
		// serve --check, in the subgroup .control of the unit's cgroup,
		// where writ serve itself runs.
		own = filepath.Dir(own)
	}
	euid := os.Geteuid()
	for _, p := range []string{own, filepath.Join(own, "cgroup.procs"), filepath.Join(own, "cgroup.subtree_control")} {
		if err := unix.Faccessat(unix.AT_FDCWD, p, unix.W_OK, unix.AT_EACCESS); err != nil {
			return none, fmt.Errorf("writ's cgroup %s is not delegated to uid %d, which must write %s (%v); run writ serve under systemd with Delegate=yes", own, euid, filepath.Base(p), err)
		}
	}
	text, err := os.ReadFile(filepath.Join(own, "cgroup.controllers"))
	if err != nil {
		return none, fmt.Errorf("read the controllers of writ's cgroup %s: %w", own, err)
	}
	offered := strings.Fields(string(text))
	for _, c := range cgroupControllers {
		if !slices.Contains(offered, c) {
			return none, fmt.Errorf("writ's cgroup %s offers no %s controller, only %q: its parent's cgroup.subtree_control must enable %s", own, c, strings.Join(offered, " "), strings.Join(cgroupControllers, ", "))
		}
	}
	pids, err := cgroupProcs(own)
	if err != nil {
		return none, err
	}
	// A process of another pid namespace reads as 0.
	if others := slices.DeleteFunc(pids, func(p int) bool { return p == os.Getpid() }); len(others) > 0 {
		return none, fmt.Errorf("writ's cgroup %s holds processes other than writ serve, %v, and a cgroup whose children have limits may hold none; run writ serve alone in its cgroup, as a systemd unit with Delegate=yes or a Nomad task does", own, others)
	}
	set := cgroupSetup{base: filepath.Join(own, runsCgroup)}
	if set.pids, err = runsLimit(own, "pids.max", servePids, "processes and threads", "TasksMax"); err != nil {
		return none, err
	}
	if set.memory, err = runsLimit(own, "memory.max", serveMemory, "bytes of memory", "MemoryMax"); err != nil {
		return none, err
	}
	set.swapOff = swapOff(own)
	if !apply {
		return set, nil
	}
	leaf := filepath.Join(own, serveCgroup)
	if err := os.Mkdir(leaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return none, fmt.Errorf("make the cgroup %s for writ serve: %w", leaf, err)
	}
	if err := checkKill(leaf); err != nil {
		_ = os.Remove(leaf)
		return none, err
	}
	if err := writeCgroup(leaf, "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
		return none, fmt.Errorf("move writ serve into %s: %w", leaf, err)
	}
	enable := "+" + strings.Join(cgroupControllers, " +")
	if err := writeCgroup(own, "cgroup.subtree_control", enable); err != nil {
		return none, fmt.Errorf("enable %s for the children of %s: %w", strings.Join(cgroupControllers, ", "), own, err)
	}
	if err := os.Mkdir(set.base, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return none, fmt.Errorf("make the cgroup %s for the steps: %w", set.base, err)
	}
	for file, n := range map[string]int64{"pids.max": set.pids, "memory.max": set.memory} {
		if n > 0 {
			if err := writeCgroup(set.base, file, strconv.FormatInt(n, 10)); err != nil {
				return none, fmt.Errorf("set %s of %s: %w", file, set.base, err)
			}
		}
	}
	if err := writeCgroup(set.base, "cgroup.subtree_control", enable); err != nil {
		return none, fmt.Errorf("enable %s for the children of %s: %w", strings.Join(cgroupControllers, ", "), set.base, err)
	}
	return set, nil
}

// checkKill checks that the cgroup dir has cgroup.kill.
func checkKill(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err != nil {
		return fmt.Errorf("the kernel has no cgroup.kill, which ends every process of a cgroup at once and needs Linux 5.14: %w", err)
	}
	return nil
}

// swapOff is why a step may swap past its memory.max although writ sets
// its memory.swap.max to 0, or "": a kernel that keeps no swap account for
// cgroups, as cgroup.memory=noswap makes it, gives own and each step no
// memory.swap.max. A kernel built without swap has no /proc/swaps, and no
// step of it can swap.
func swapOff(own string) string {
	if _, err := os.Stat(filepath.Join(own, "memory.swap.max")); err == nil {
		return ""
	}
	if _, err := os.Stat("/proc/swaps"); err != nil {
		return ""
	}
	return fmt.Sprintf("writ's cgroup %s has no memory.swap.max, as when the kernel boots with cgroup.memory=noswap, so writ can't keep a step from swapping past limits.run_memory", own)
}

// runsLimit returns what runsCgroup may have of the limit file under own:
// the least that own and each cgroup above it allow, less keep for writ
// serve, or 0 when none of them sets one. what and unit name the limit in
// a refusal, and the systemd setting that raises it.
func runsLimit(own, file string, keep int64, what, unit string) (int64, error) {
	var least int64
	for dir := own; ; dir = filepath.Dir(dir) {
		text, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			// The root cgroup, and what lies above the mount, have none.
			break
		}
		if v := strings.TrimSpace(string(text)); v != "max" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("the cgroup %s has %s %q", dir, file, v)
			}
			if least == 0 || n < least {
				least = n
			}
		}
		if dir == "/" {
			break
		}
	}
	if least == 0 {
		return 0, nil
	}
	if least <= keep {
		return 0, fmt.Errorf("writ's cgroup %s allows %d %s, no more than the %d that writ serve keeps for itself; raise the unit's %s", own, least, what, keep, unit)
	}
	return least - keep, nil
}

// checkCgroups reports, for writ serve --check, what cgroupBase would set
// up, or why it can't: cgroupBase's checks, and then, in a cgroup of the
// probe's own beside where runsCgroup would be, which it removes, what
// cgroupBase does only with apply: it checks cgroup.kill, writes
// cgroup.subtree_control, and starts a process as each step's first
// process starts. The checks alone can pass where those then fail.
func checkCgroups() (cgroupSetup, error) {
	set, err := cgroupBase(false)
	if err != nil {
		return set, err
	}
	own := filepath.Dir(set.base)
	probe := filepath.Join(own, "check-"+strings.ToLower(rand.Text()))
	if err := os.Mkdir(probe, 0o755); err != nil {
		return cgroupSetup{}, fmt.Errorf("make a cgroup under %s for a probe: %w", own, err)
	}
	defer os.Remove(probe)
	if err := checkKill(probe); err != nil {
		return cgroupSetup{}, err
	}
	if err := probeHelper(probe); err != nil {
		return cgroupSetup{}, fmt.Errorf("writ could not start a step's first process in a cgroup of its own under %s: %w", own, err)
	}
	// After the probe has exited: a cgroup whose children have
	// controllers may hold no process. The probe's cgroup offers only
	// what own's cgroup.subtree_control enables, none before writ serve
	// first starts, and --check must not change own's: systemd could
	// then start no process in it.
	text, err := os.ReadFile(filepath.Join(probe, "cgroup.controllers"))
	if err != nil {
		return cgroupSetup{}, fmt.Errorf("read the controllers of %s: %w", probe, err)
	}
	var enable []string
	for _, c := range strings.Fields(string(text)) {
		if slices.Contains(cgroupControllers, c) {
			enable = append(enable, "+"+c)
		}
	}
	if err := writeCgroup(probe, "cgroup.subtree_control", strings.Join(enable, " ")); err != nil {
		return cgroupSetup{}, fmt.Errorf("write cgroup.subtree_control of %s: %w", probe, err)
	}
	return set, nil
}

// ownCgroup returns the directory of this process's cgroup in the cgroup
// v2 filesystem. When none is mounted and writ runs as root, it mounts
// one at cgroupMount, if that is an empty directory and no mount point.
func ownCgroup() (string, error) {
	text, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read writ's cgroup: %w", err)
	}
	var path string
	for line := range strings.Lines(string(text)) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			path = p
		}
	}
	if path == "" {
		return "", errors.New("writ is in no cgroup v2 hierarchy: the host runs cgroup v1 only")
	}
	mounts, err := cgroup2Mounts()
	if err != nil {
		return "", err
	}
	if len(mounts) == 0 && os.Geteuid() == 0 {
		if err := mountCgroup2(); err != nil {
			return "", err
		}
		if mounts, err = cgroup2Mounts(); err != nil {
			return "", err
		}
	}
	if len(mounts) == 0 {
		return "", fmt.Errorf("no cgroup v2 filesystem is mounted, and writ mounts one at %s only as root", cgroupMount)
	}
	m := mounts[0]
	for _, c := range mounts {
		if c.at == cgroupMount {
			m = c
		}
	}
	rel, err := filepath.Rel(m.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("writ's cgroup %s is outside the cgroup filesystem mounted at %s", path, m.at)
	}
	return filepath.Join(m.at, rel), nil
}

type mount struct{ root, at string }

// cgroup2Mounts lists the mounts of the cgroup v2 filesystem in this
// process's mount namespace.
func cgroup2Mounts() ([]mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []mount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// ID PARENT MAJ:MIN ROOT MOUNTPOINT OPTIONS [OPTIONAL...] - TYPE SOURCE SUPER
		left, right, ok := strings.Cut(sc.Text(), " - ")
		fields, after := strings.Fields(left), strings.Fields(right)
		if !ok || len(fields) < 5 || len(after) == 0 || after[0] != "cgroup2" {
			continue
		}
		out = append(out, mount{root: unescapeMount(fields[3]), at: unescapeMount(fields[4])})
	}
	return out, sc.Err()
}

// unescapeMount undoes mountinfo's octal escapes of space, tab, newline,
// and backslash.
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// mountCgroup2 mounts the cgroup v2 filesystem at cgroupMount, which must
// be an empty directory and no mount point.
func mountCgroup2() error {
	ents, err := os.ReadDir(cgroupMount)
	if err != nil || len(ents) > 0 {
		return fmt.Errorf("no cgroup v2 filesystem is mounted, and %s is not an empty directory to mount one at", cgroupMount)
	}
	if err := unix.Mount("cgroup2", cgroupMount, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC|unix.MS_RELATIME, ""); err != nil {
		return fmt.Errorf("no cgroup v2 filesystem is mounted, and writ could not mount one at %s: %w", cgroupMount, err)
	}
	return nil
}

// cgroupProcs reads the ids of the processes in the cgroup dir.
func cgroupProcs(dir string) ([]int, error) {
	text, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, fmt.Errorf("read the processes of the cgroup %s: %w", dir, err)
	}
	var pids []int
	for f := range strings.FieldsSeq(string(text)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("the cgroup %s lists a process %q", dir, f)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func writeCgroup(dir, file, value string) error {
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	return errors.Join(err, f.Close())
}

// stepCgroup is the cgroup of one step: its processes, and only they,
// run in it, from before the helper does anything of its own.
type stepCgroup struct {
	dir    string
	limits Limits
}

// newStepCgroup makes a cgroup for a step under base with l's limits:
// memory.max with no swap, and the whole cgroup killed when one of its
// processes is, pids.max, and cpu.max.
func newStepCgroup(base string, l Limits) (*stepCgroup, error) {
	c := &stepCgroup{dir: filepath.Join(base, "run-"+strings.ToLower(rand.Text())), limits: l}
	if err := os.Mkdir(c.dir, 0o755); err != nil {
		return nil, err
	}
	set := [][2]string{{"memory.oom.group", "1"}}
	if l.Memory > 0 {
		set = append(set, [2]string{"memory.max", strconv.FormatInt(l.Memory, 10)})
		if _, err := os.Stat(filepath.Join(c.dir, "memory.swap.max")); err == nil {
			set = append(set, [2]string{"memory.swap.max", "0"})
		}
	}
	if l.Pids > 0 {
		set = append(set, [2]string{"pids.max", strconv.Itoa(l.Pids)})
	}
	if l.CPUs > 0 {
		const period = 100000
		set = append(set, [2]string{"cpu.max", fmt.Sprintf("%d %d", max(1000, int64(l.CPUs*period)), period)})
	}
	for _, s := range set {
		if err := writeCgroup(c.dir, s[0], s[1]); err != nil {
			_ = os.Remove(c.dir)
			return nil, fmt.Errorf("set %s of %s: %w", s[0], c.dir, err)
		}
	}
	return c, nil
}

// enter moves the process pid, with all its threads, into the cgroup.
func (c *stepCgroup) enter(pid int) error {
	return writeCgroup(c.dir, "cgroup.procs", strconv.Itoa(pid))
}

// kill ends every process of the cgroup, wherever it went in its pid
// namespace and whatever its uid, and waits until none is left.
func (c *stepCgroup) kill() error {
	if err := writeCgroup(c.dir, "cgroup.kill", "1"); err != nil {
		return fmt.Errorf("kill the step's cgroup: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ev, err := cgroupKeys(c.dir, "cgroup.events")
		if err == nil && ev["populated"] == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes of the step's cgroup %s did not exit", c.dir)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hit is which limit the step reached, in writ's words, or "".
func (c *stepCgroup) hit() string {
	if ev, err := cgroupKeys(c.dir, "memory.events"); err == nil && (ev["oom_kill"] > 0 || ev["oom_group_kill"] > 0) {
		return fmt.Sprintf("the run used all of limits.run_memory, %s, and the kernel killed it", formatSize(c.limits.Memory))
	}
	if ev, err := cgroupKeys(c.dir, "pids.events"); err == nil && ev["max"] > 0 {
		return fmt.Sprintf("the run reached limits.run_pids, %d processes and threads, and could start no more", c.limits.Pids)
	}
	return ""
}

// remove removes the cgroup, which kill emptied.
func (c *stepCgroup) remove() error {
	return os.Remove(c.dir)
}

// heldRemoved returns the bytes on disk of the files on the filesystem dev
// that no directory names but a process of the cgroup holds open or maps,
// each counted once and as at least scratchInode. A mapped file counts as
// the end of its furthest mapping, since a mapping, unlike a descriptor,
// gives no way to stat the file.
func (c *stepCgroup) heldRemoved(dev uint64) (int64, error) {
	pids, err := cgroupProcs(c.dir)
	if err != nil {
		return 0, err
	}
	held := map[uint64]int64{}
	for _, pid := range pids {
		if err := heldByProcess(pid, dev, held); err != nil {
			return 0, err
		}
	}
	var total int64
	for _, n := range held {
		total += max(n, scratchInode)
	}
	return total, nil
}

// heldByProcess adds to held, by inode, the bytes of each removed file on
// dev that the process pid holds open or maps. A process that exits
// meanwhile holds nothing.
func heldByProcess(pid int, dev uint64, held map[uint64]int64) error {
	proc := "/proc/" + strconv.Itoa(pid)
	f, err := os.Open(proc + "/fd")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the descriptors of the step's process %d: %w", pid, err)
	}
	defer f.Close()
	for {
		names, err := f.Readdirnames(256)
		if errors.Is(err, io.EOF) || errors.Is(err, unix.ESRCH) || errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return fmt.Errorf("read the descriptors of the step's process %d: %w", pid, err)
		}
		for _, n := range names {
			var st unix.Stat_t
			err := unix.Stat(proc+"/fd/"+n, &st)
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
				// Closed meanwhile.
				continue
			}
			if err != nil {
				return fmt.Errorf("read a descriptor of the step's process %d: %w", pid, err)
			}
			if st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 0 && st.Dev == dev {
				held[st.Ino] = max(held[st.Ino], st.Blocks*512)
			}
		}
	}
	maps, err := os.Open(proc + "/maps")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the mappings of the step's process %d: %w", pid, err)
	}
	defer maps.Close()
	want := fmt.Sprintf("%02x:%02x", unix.Major(dev), unix.Minor(dev))
	sc := bufio.NewScanner(maps)
	for sc.Scan() {
		// START-END PERMS OFFSET MAJ:MIN INODE PATH (deleted)
		line := sc.Text()
		if !strings.HasSuffix(line, " (deleted)") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 6 || f[3] != want {
			continue
		}
		start, end, _ := strings.Cut(f[0], "-")
		lo, err1 := strconv.ParseUint(start, 16, 64)
		hi, err2 := strconv.ParseUint(end, 16, 64)
		off, err3 := strconv.ParseUint(f[2], 16, 64)
		ino, err4 := strconv.ParseUint(f[4], 10, 64)
		if err := errors.Join(err1, err2, err3, err4); err != nil || ino == 0 {
			continue
		}
		held[ino] = max(held[ino], int64(off+hi-lo))
	}
	if err := sc.Err(); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("read the mappings of the step's process %d: %w", pid, err)
	}
	return nil
}

// cgroupKeys reads a cgroup file of "KEY VALUE" lines.
func cgroupKeys(dir, file string) (map[string]int64, error) {
	text, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for line := range strings.Lines(string(text)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			out[k] = n
		}
	}
	return out, nil
}
