package tofu

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
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

// setUpCgroups makes this process's cgroup the base of every step's
// cgroup, once for the process: it moves the process into the base's
// serveCgroup and enables cgroupControllers for the base's children.
var setUpCgroups = sync.OnceValues(func() (string, error) { return cgroupBase(true) })

// cgroupBase finds the cgroup under which each step gets a cgroup of its
// own, the cgroup this process runs in, and checks that this process may
// make children of it with cgroupControllers: that the cgroup is
// delegated to writ's uid, offers the controllers, and holds no other
// process, as systemd's Delegate=yes gives a unit. With apply, it moves
// this process into serveCgroup and enables the controllers. Without, it
// changes nothing but, as root, the mount of an absent cgroup filesystem.
func cgroupBase(apply bool) (string, error) {
	own, err := ownCgroup()
	if err != nil {
		return "", err
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
			return "", fmt.Errorf("writ's cgroup %s is not delegated to uid %d, which must write %s (%v); run writ serve under systemd with Delegate=yes", own, euid, filepath.Base(p), err)
		}
	}
	text, err := os.ReadFile(filepath.Join(own, "cgroup.controllers"))
	if err != nil {
		return "", fmt.Errorf("read the controllers of writ's cgroup %s: %w", own, err)
	}
	offered := strings.Fields(string(text))
	for _, c := range cgroupControllers {
		if !slices.Contains(offered, c) {
			return "", fmt.Errorf("writ's cgroup %s offers no %s controller, only %q: its parent's cgroup.subtree_control must enable %s", own, c, strings.Join(offered, " "), strings.Join(cgroupControllers, ", "))
		}
	}
	pids, err := cgroupProcs(own)
	if err != nil {
		return "", err
	}
	// A process of another pid namespace reads as 0.
	if others := slices.DeleteFunc(pids, func(p int) bool { return p == os.Getpid() }); len(others) > 0 {
		return "", fmt.Errorf("writ's cgroup %s holds processes other than writ serve, %v, and a cgroup whose children have limits may hold none; run writ serve alone in its cgroup, as a systemd unit with Delegate=yes or a Nomad task does", own, others)
	}
	if !apply {
		return own, nil
	}
	leaf := filepath.Join(own, serveCgroup)
	if err := os.Mkdir(leaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("make the cgroup %s for writ serve: %w", leaf, err)
	}
	if _, err := os.Stat(filepath.Join(leaf, "cgroup.kill")); err != nil {
		_ = os.Remove(leaf)
		return "", fmt.Errorf("the kernel has no cgroup.kill, which ends every process of a cgroup at once and needs Linux 5.14: %w", err)
	}
	if err := writeCgroup(leaf, "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
		return "", fmt.Errorf("move writ serve into %s: %w", leaf, err)
	}
	enable := "+" + strings.Join(cgroupControllers, " +")
	if err := writeCgroup(own, "cgroup.subtree_control", enable); err != nil {
		return "", fmt.Errorf("enable %s for the children of %s: %w", strings.Join(cgroupControllers, ", "), own, err)
	}
	return own, nil
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
// run in it, from the helper's first instruction on.
type stepCgroup struct {
	dir    string
	limits Limits
	// f is the cgroup's directory, which the helper is started into
	// with CLONE_INTO_CGROUP.
	f *os.File
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
	f, err := os.Open(c.dir)
	if err != nil {
		_ = os.Remove(c.dir)
		return nil, err
	}
	c.f = f
	return c, nil
}

// fd is the cgroup's directory, for SysProcAttr.CgroupFD.
func (c *stepCgroup) fd() int { return int(c.f.Fd()) }

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
	c.f.Close()
	return os.Remove(c.dir)
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
