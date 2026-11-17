//go:build e2e_import

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// nomadHelpers returns the processes descended from the agent: the logmon
// and executor helpers of its allocations. Nomad detaches them so tasks
// survive an agent restart, so they outlive a stopped test agent unless the
// test stops them. Call it while the agent still runs, because an orphan's
// parent is no longer the agent.
func nomadHelpers(agent int) []int {
	children := map[int][]int{}
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, p := range stats {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// The command name in parentheses may hold spaces; fields follow it.
		_, rest, ok := strings.Cut(string(b), ") ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(filepath.Base(filepath.Dir(p)))
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 == nil && err2 == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var out []int
	queue := children[agent]
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		out = append(out, pid)
		queue = append(queue, children[pid]...)
	}
	return out
}

// stopNomadHelpers kills the helpers that outlived the agent. A pid is
// killed only if it still runs nomad, so a reused pid is left alone.
func stopNomadHelpers(pids []int) {
	for _, pid := range pids {
		exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
		if err != nil || filepath.Base(exe) != "nomad" {
			continue
		}
		syscall.Kill(pid, syscall.SIGKILL)
	}
}
