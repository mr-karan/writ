package tofu

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"hash/maphash"
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
	pids, err := cgroupIDs(own, "cgroup.procs")
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
// probe's own beside where runsCgroup would be, which it removes, some of
// what cgroupBase does only with apply: it checks cgroup.kill and starts a
// process as each step's first process starts. The checks alone can pass
// where those then fail. It enables for the probe's children only what
// own already enables for its own, which is nothing before writ serve
// first starts, so it can't check that own may enable the controllers.
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

// cgroupIDs reads the ids that the file of the cgroup dir lists:
// cgroup.procs lists its processes, and cgroup.threads their threads.
func cgroupIDs(dir, file string) ([]int, error) {
	text, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return nil, fmt.Errorf("read %s of the cgroup %s: %w", file, dir, err)
	}
	var ids []int
	for f := range strings.FieldsSeq(string(text)) {
		id, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("the cgroup %s lists %q in %s", dir, f, file)
		}
		ids = append(ids, id)
	}
	return ids, nil
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
	// runs is runsCgroup, which every step shares, and before what its
	// own limits had counted when the step began.
	runs   cgroupSetup
	before runsEvents
}

// runsEvents is what the limits of runsCgroup itself have counted: the
// times its memory.max brought the OOM killer, and the times its pids.max
// refused a process. They count for every step, so a step compares them
// at its end with what they were at its start.
type runsEvents struct{ oom, pidsMax int64 }

func readRunsEvents(dir string) runsEvents {
	var e runsEvents
	if ev, err := localKeys(dir, "memory.events"); err == nil {
		e.oom = ev["oom"]
	}
	if ev, err := localKeys(dir, "pids.events"); err == nil {
		e.pidsMax = ev["max"]
	}
	return e
}

// localKeys reads file.local of the cgroup dir, which counts only dir's
// own limits, not its children's, or file where the kernel has none.
func localKeys(dir, file string) (map[string]int64, error) {
	if ev, err := cgroupKeys(dir, file+".local"); err == nil {
		return ev, nil
	}
	return cgroupKeys(dir, file)
}

// newStepCgroup makes a cgroup for a step under runs.base with l's limits:
// memory.max with no swap, and the whole cgroup killed when one of its
// processes is, pids.max, and cpu.max.
func newStepCgroup(runs cgroupSetup, l Limits) (*stepCgroup, error) {
	c := &stepCgroup{dir: filepath.Join(runs.base, "run-"+strings.ToLower(rand.Text())), limits: l, runs: runs, before: readRunsEvents(runs.base)}
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

// hit is which limit the step reached, in writ's words, or "", and
// whether it is a limit of runsCgroup, which every step shares: its own,
// or runsCgroup's memory, when the kernel killed the step for it, or
// runsCgroup's processes, when it refused the step one.
func (c *stepCgroup) hit() (string, bool) {
	if ev, err := cgroupKeys(c.dir, "memory.events"); err == nil && (ev["oom_kill"] > 0 || ev["oom_group_kill"] > 0) {
		if ev["oom"] == 0 && readRunsEvents(c.runs.base).oom > c.before.oom {
			return fmt.Sprintf("the steps running at once used all of the %s of memory that writ's cgroup leaves them together, and the kernel killed the run; raise the unit's MemoryMax, or lower limits.run_memory, plans, or applies", formatSize(c.runs.memory)), true
		}
		return fmt.Sprintf("the run used all of limits.run_memory, %s, and the kernel killed it", formatSize(c.limits.Memory)), false
	}
	if ev, err := cgroupKeys(c.dir, "pids.events"); err == nil && ev["max"] > 0 {
		if c.ownPidsHit() {
			return fmt.Sprintf("the run reached limits.run_pids, %d processes and threads, and could start no more", c.limits.Pids), false
		}
		return c.sharedPids(), true
	}
	return "", false
}

// ownPidsHit reports whether the step's own pids.max refused a process
// that its pids.events counts. From Linux 6.15, which added
// pids.events.local, pids.events counts only the refusals of the step's
// own limit. Before, it counted every refusal of a process the step
// started, runsCgroup's too, and a step whose pids.peak never reached its
// own limit was refused by runsCgroup's.
func (c *stepCgroup) ownPidsHit() bool {
	if _, err := os.Stat(filepath.Join(c.dir, "pids.events.local")); err == nil || c.runs.pids == 0 {
		return true
	}
	text, err := os.ReadFile(filepath.Join(c.dir, "pids.peak"))
	if err != nil {
		// Linux 6.1 added pids.peak.
		return true
	}
	peak, err := strconv.Atoi(strings.TrimSpace(string(text)))
	return err != nil || c.limits.Pids > 0 && peak >= c.limits.Pids
}

// sharedHit is, for a step that failed and reached no limit of its own,
// the limit of runsCgroup that refused a process while it ran, or "".
// Another step may have been the one refused.
func (c *stepCgroup) sharedHit() string {
	if readRunsEvents(c.runs.base).pidsMax > c.before.pidsMax {
		return c.sharedPids()
	}
	return ""
}

func (c *stepCgroup) sharedPids() string {
	return fmt.Sprintf("while the run ran, the steps running at once reached the %d processes and threads that writ's cgroup leaves them together, and the kernel refused them more; raise the unit's TasksMax, or lower limits.run_pids, plans, or applies", c.runs.pids)
}

// remove removes the cgroup, which kill emptied.
func (c *stepCgroup) remove() error {
	return os.Remove(c.dir)
}

// heldRemoved returns the bytes on disk of the files that no directory
// names but a thread of the cgroup holds open or maps, each counted once
// and as at least scratchInode, on whatever filesystem they lie: a run may
// make a btrfs subvolume in its own directory, whose files stat gives a
// device of their own. It reads the descriptors of every thread, not only
// each process's: once a process's first thread exits alone, /proc/PID
// reads that thread's empty tables while the others still hold the file,
// and a thread cloned without CLONE_FILES has descriptors of its own. It
// reads the mappings of each process once, from a thread of it that is
// alive, since its threads share them. It reads /proc as u, the run's
// user: /proc/TID/fd belongs to the uid of its thread, and the kernel lets
// no other uid without CAP_DAC_READ_SEARCH list it, though writ made the
// user namespace the thread runs in. It stops once what it found passes
// max, and returns that much. It fails with errScratchSlow past deadline,
// with errScratchMaps past heldMapsMax, and with errScratchFiles past
// heldFilesMax.
func (c *stepCgroup) heldRemoved(u *User, max int64, deadline time.Time) (int64, error) {
	tids, err := cgroupIDs(c.dir, "cgroup.threads")
	if err != nil {
		return 0, err
	}
	pids, err := cgroupIDs(c.dir, "cgroup.procs")
	if err != nil {
		return 0, err
	}
	h := newHeldFiles(max, deadline)
	var deniedTids, deniedPids []int
	err = asUser(u, func() error {
		for _, tid := range tids {
			err := h.descriptors(tid)
			if u != nil && errors.Is(err, os.ErrPermission) {
				deniedTids = append(deniedTids, tid)
				continue
			}
			if err != nil {
				return err
			}
		}
		for _, pid := range pids {
			err := h.mappings(pid)
			if u != nil && errors.Is(err, os.ErrPermission) {
				deniedPids = append(deniedPids, pid)
				continue
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	// A process that made itself not dumpable gives its /proc/TID/fd and
	// maps to root of its user namespace, which is writ's uid.
	for _, tid := range deniedTids {
		if err == nil {
			err = h.descriptors(tid)
		}
	}
	for _, pid := range deniedPids {
		if err == nil {
			err = h.mappings(pid)
		}
	}
	switch {
	case errors.Is(err, errScratchOver):
		return h.least(), nil
	case err != nil:
		return 0, err
	}
	return h.total(), nil
}

// heldFiles is what threads hold of files that no directory names.
// /proc/TID/maps gives a file's device as its filesystem's, which is not
// always the one stat gives, as on btrfs, so a mapping is known by its
// device, inode, and path, and is the file of a descriptor when the inode
// and either the device or the path agree. It keeps a hash of each path,
// with a seed of its own that no run can know, not the path, which a run
// may make some 60KB long.
type heldFiles struct {
	seed maphash.Seed
	// open holds the files of descriptors by stat's device and inode.
	open map[[2]uint64]openFile
	// mapped holds the end of each file's furthest mapping, since a
	// mapping, unlike a descriptor, gives no way to stat the file.
	mapped map[mappedFile]int64
	// inos holds the inode of each file of open and mapped, and openBytes
	// what the files of open count: what h counts is at least each.
	inos      map[uint64]struct{}
	openBytes int64
	// max is how far the count may grow before reading stops, mapsLeft
	// how many bytes of maps are left to read, and deadline when reading
	// stops.
	max      int64
	mapsLeft int64
	deadline time.Time
}

type openFile struct {
	blocks int64
	path   uint64
}

type mappedFile struct {
	dev, ino, path uint64
}

func newHeldFiles(max int64, deadline time.Time) *heldFiles {
	return &heldFiles{
		seed:     maphash.MakeSeed(),
		open:     map[[2]uint64]openFile{},
		mapped:   map[mappedFile]int64{},
		inos:     map[uint64]struct{}{},
		max:      max,
		mapsLeft: heldMapsMax,
		deadline: deadline,
	}
}

// inMemory reports whether path, as /proc names a removed file, is a
// memfd or System V shared memory: memory, which run_memory bounds, as
// is the sealed saved plan that writ gives a step.
func inMemory(path []byte) bool {
	return bytes.HasPrefix(path, []byte("/memfd:")) || bytes.HasPrefix(path, []byte("/SYSV"))
}

// least is no more than what total returns: each file counts at least
// scratchInode, and two files of one inode number are two files.
func (h *heldFiles) least() int64 {
	return max(h.openBytes, int64(len(h.inos))*scratchInode)
}

// addOpen adds the removed file of a descriptor, and fails with
// errScratchOver once h counts more than its max, or errScratchFiles once
// it holds more than heldFilesMax files.
func (h *heldFiles) addOpen(dev, ino uint64, blocks int64, path []byte) error {
	id := [2]uint64{dev, ino}
	old, seen := h.open[id]
	f := openFile{blocks: max(old.blocks, blocks), path: maphash.Bytes(h.seed, path)}
	if seen {
		h.openBytes -= max(old.blocks, scratchInode)
	}
	h.openBytes += max(f.blocks, scratchInode)
	h.open[id] = f
	h.inos[ino] = struct{}{}
	return h.check()
}

// addMapped adds a mapping of a removed file that reaches end bytes into
// it, and fails as addOpen does.
func (h *heldFiles) addMapped(dev, ino uint64, end int64, path []byte) error {
	m := mappedFile{dev: dev, ino: ino, path: maphash.Bytes(h.seed, path)}
	h.mapped[m] = max(h.mapped[m], end)
	h.inos[ino] = struct{}{}
	return h.check()
}

func (h *heldFiles) check() error {
	if h.least() > h.max {
		return errScratchOver
	}
	if len(h.open)+len(h.mapped) > heldFilesMax {
		return errScratchFiles
	}
	return nil
}

// total is the bytes of h's files, each at least scratchInode.
func (h *heldFiles) total() int64 {
	if len(h.mapped) == 0 {
		return h.openBytes
	}
	byIno := map[uint64][][2]uint64{}
	for id := range h.open {
		byIno[id[1]] = append(byIno[id[1]], id)
	}
	n := h.openBytes
	for m, end := range h.mapped {
		if !slices.ContainsFunc(byIno[m.ino], func(id [2]uint64) bool {
			return unix.Mkdev(unix.Major(id[0]), unix.Minor(id[0])) == m.dev || h.open[id].path == m.path
		}) {
			n += max(end, scratchInode)
		}
	}
	return n
}

// descriptors adds to h each removed file that the thread tid holds open.
// A thread that exits meanwhile holds nothing.
func (h *heldFiles) descriptors(tid int) error {
	proc := "/proc/" + strconv.Itoa(tid)
	f, err := os.Open(proc + "/fd")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the descriptors of the step's thread %d: %w", tid, err)
	}
	defer f.Close()
	for {
		if time.Now().After(h.deadline) {
			return errScratchSlow
		}
		names, err := f.Readdirnames(256)
		if errors.Is(err, io.EOF) || errors.Is(err, unix.ESRCH) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the descriptors of the step's thread %d: %w", tid, err)
		}
		for _, n := range names {
			var st unix.Stat_t
			fd := proc + "/fd/" + n
			err := unix.Stat(fd, &st)
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
				// Closed meanwhile.
				continue
			}
			if err != nil {
				return fmt.Errorf("read a descriptor of the step's thread %d: %w", tid, err)
			}
			if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 0 {
				continue
			}
			path, err := os.Readlink(fd)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read a descriptor of the step's thread %d: %w", tid, err)
			}
			p := []byte(strings.TrimSuffix(path, deleted))
			if inMemory(p) {
				continue
			}
			if err := h.addOpen(st.Dev, st.Ino, st.Blocks*512, p); err != nil {
				return err
			}
		}
	}
}

// deleted is what /proc writes after the path of a removed file.
const deleted = " (deleted)"

// mappings adds to h each removed file that the process pid maps, read
// from the first of its threads that has mappings: a thread that has
// exited, as the first may alone, has none. A process that exits
// meanwhile maps nothing.
func (h *heldFiles) mappings(pid int) error {
	task := "/proc/" + strconv.Itoa(pid) + "/task"
	f, err := os.Open(task)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the threads of the step's process %d: %w", pid, err)
	}
	defer f.Close()
	for {
		names, err := f.Readdirnames(64)
		if errors.Is(err, io.EOF) || errors.Is(err, unix.ESRCH) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the threads of the step's process %d: %w", pid, err)
		}
		for _, n := range names {
			found, err := h.maps(task+"/"+n+"/maps", pid)
			if found || err != nil {
				return err
			}
		}
	}
}

// maps adds to h each removed file that the maps file path lists, and
// reports whether it lists any mapping. It checks the deadline at each
// line: a process that keeps mapping past the line writ reads, and
// unmapping before it, keeps one read of maps going for as long as it
// runs.
func (h *heldFiles) maps(path string, pid int) (bool, error) {
	if time.Now().After(h.deadline) {
		return false, errScratchSlow
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the mappings of the step's process %d: %w", pid, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(budgeted{f, &h.mapsLeft})
	found := false
	for sc.Scan() {
		found = true
		if time.Now().After(h.deadline) {
			return found, errScratchSlow
		}
		if err := h.mapping(sc.Bytes()); err != nil {
			return found, err
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, unix.ESRCH) {
		return found, fmt.Errorf("read the mappings of the step's process %d: %w", pid, err)
	}
	return found, nil
}

// mapping adds to h the file of a line of maps, if it is removed.
func (h *heldFiles) mapping(line []byte) error {
	line, ok := bytes.CutSuffix(line, []byte(deleted))
	if !ok {
		return nil
	}
	// START-END PERMS OFFSET MAJ:MIN INODE PATH, the path padded with
	// spaces and its own spaces kept.
	var f [5][]byte
	rest := line
	for i := range f {
		if f[i], rest, ok = bytes.Cut(bytes.TrimLeft(rest, " "), []byte(" ")); !ok {
			break
		}
	}
	path := bytes.TrimLeft(rest, " ")
	if !ok || inMemory(path) {
		return nil
	}
	start, end, _ := bytes.Cut(f[0], []byte("-"))
	major, minor, _ := bytes.Cut(f[3], []byte(":"))
	lo, err1 := strconv.ParseUint(string(start), 16, 64)
	hi, err2 := strconv.ParseUint(string(end), 16, 64)
	off, err3 := strconv.ParseUint(string(f[2]), 16, 64)
	maj, err4 := strconv.ParseUint(string(major), 16, 32)
	minr, err5 := strconv.ParseUint(string(minor), 16, 32)
	ino, err6 := strconv.ParseUint(string(f[4]), 10, 64)
	if err := errors.Join(err1, err2, err3, err4, err5, err6); err != nil || ino == 0 {
		return nil
	}
	return h.addMapped(unix.Mkdev(uint32(maj), uint32(minr)), ino, int64(off+hi-lo), path)
}

// budgeted reads from r until *left bytes are read in all, and then fails
// with errScratchMaps.
type budgeted struct {
	r    io.Reader
	left *int64
}

func (b budgeted) Read(p []byte) (int, error) {
	if *b.left <= 0 {
		return 0, errScratchMaps
	}
	if int64(len(p)) > *b.left {
		p = p[:*b.left]
	}
	n, err := b.r.Read(p)
	*b.left -= int64(n)
	return n, err
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
