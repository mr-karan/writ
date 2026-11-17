package server

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxStackName bounds a stack name.
const MaxStackName = 128

var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// CheckStackName refuses a stack name that is not a relative slash path of
// plain segments: no ., no .., no empty segment, no leading slash.
func CheckStackName(s string) error {
	if s == "" || len(s) > MaxStackName {
		return fmt.Errorf("stack name %q must be 1 to %d characters", trunc(s), MaxStackName)
	}
	for seg := range strings.SplitSeq(s, "/") {
		if !segmentRE.MatchString(seg) {
			return fmt.Errorf("stack name %q must be slash-separated names of letters, digits, ., _ and -, with no . or .. segment", trunc(s))
		}
	}
	return nil
}

// ValidEnvName reports whether name is an environment's name: a lowercase
// letter, then up to 62 lowercase letters, digits, _, and -.
func ValidEnvName(name string) bool { return nameRE.MatchString(name) }

func trunc(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
