package cli

import (
	"slices"
	"testing"

	"github.com/mr-karan/writ/internal/tofu"
)

// writ serve --check warns when the steps that may run at once, each at
// its limits, can need more than writ serve's cgroup leaves them together.
func TestSharedLimitWarnings(t *testing.T) {
	l := tofu.Limits{Memory: 1 << 30, Pids: 64}
	for _, c := range []struct {
		name  string
		slots int
		e     tofu.Enforcement
		want  []string
	}{
		{"no bound on the steps together", 12, tofu.Enforcement{Cgroup: "/runs"}, nil},
		{"room for every slot", 4, tofu.Enforcement{Cgroup: "/runs", StepsPids: 256, StepsMemory: 4 << 30}, nil},
		{"too few processes", 4, tofu.Enforcement{Cgroup: "/runs", StepsPids: 72}, []string{
			"limits.run_pids, 64, times 4 plans and applies at once is 256, more than the 72 processes and threads that writ's cgroup leaves every step together, its pids.max less what writ serve keeps; raise the unit's TasksMax, or lower limits.run_pids, plans, or applies",
		}},
		{"too little memory", 4, tofu.Enforcement{Cgroup: "/runs", StepsPids: 256, StepsMemory: 3 << 30}, []string{
			"limits.run_memory, 1GiB, times 4 plans and applies at once is 4GiB, more than the 3GiB that writ's cgroup leaves every step together, its memory.max less what writ serve keeps; raise the unit's MemoryMax, or lower limits.run_memory, plans, or applies",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := sharedLimitWarnings(l, c.slots, c.e); !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
