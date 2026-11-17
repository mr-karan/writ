//go:build linux

package tofu

import "testing"

// TestTmpTargetAvoidsWhatTheViewHolds requires that the run's TMPDIR bind
// is never at, under, or above another path the view holds, such as a
// scratch_dir the config sets to /writ-tmp.
func TestTmpTargetAvoidsWhatTheViewHolds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targets []string
		want    string
	}{
		{"nothing near it", []string{"/var/lib/writ/scratch/run", "/nix/store/x-tofu/bin/tofu"}, "/writ-tmp"},
		{"a scratch_dir at it", []string{"/writ-tmp/writ-tofu-1"}, "/writ-tmp-1"},
		{"a plugin cache at it", []string{"/writ-tmp"}, "/writ-tmp-1"},
		{"two taken", []string{"/writ-tmp/a", "/writ-tmp-1"}, "/writ-tmp-2"},
		{"a longer name is no conflict", []string{"/writ-tmpx/a"}, "/writ-tmp"},
	} {
		var binds []bind
		for _, target := range tc.targets {
			binds = append(binds, bind{Target: target})
		}
		if got := tmpTarget(binds); got != tc.want {
			t.Errorf("%s: tmpTarget = %s, want %s", tc.name, got, tc.want)
		}
	}
}
