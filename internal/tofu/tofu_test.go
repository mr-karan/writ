package tofu

import (
	"strings"
	"testing"
)

// A step's error names a limit of its own alone, since that limit ended
// the step. A limit that every step shares may have refused another
// step, so the error keeps the step's exit code and Reason, which may say
// why it failed, as well as the limit.
func TestStepErrorNamesALimit(t *testing.T) {
	for _, c := range []struct {
		name string
		err  StepError
		want []string
		not  []string
	}{
		{"its own limit", StepError{Step: "plan", Code: -1, Limit: "the run reached limits.run_pids"}, []string{"tofu plan stopped at a limit: the run reached limits.run_pids"}, []string{"code"}},
		{"a shared limit", StepError{Step: "init", Code: 1, Reason: "a module source needs git", Limit: "raise the unit's TasksMax", Shared: true}, []string{"tofu init exited with code 1", "a module source needs git", "raise the unit's TasksMax"}, nil},
		{"no limit", StepError{Step: "apply", Code: 1}, []string{"tofu apply exited with code 1"}, []string{"limit"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.err.Error()
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q does not say %q", got, w)
				}
			}
			for _, n := range c.not {
				if strings.Contains(got, n) {
					t.Errorf("%q says %q", got, n)
				}
			}
		})
	}
}
