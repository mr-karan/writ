package main

import (
	"flag"
	"slices"
	"strings"
	"testing"
)

// TestCommandFlagsDoNotCollideWithGlobals registers every command listed in
// the usage text, in both of its sections, on top of the global flags, as
// run does. A command flag with a global's name panics in the flag package.
func TestCommandFlagsDoNotCollideWithGlobals(t *testing.T) {
	_, list, ok := strings.Cut(usage, "commands:\n")
	if !ok {
		t.Fatal("usage has no commands section")
	}
	var names []string
	for line := range strings.SplitSeq(list, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue // a section's heading, prose, or a wrapped description
		}
		if name := strings.Fields(line)[0]; !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) < 15 {
		t.Fatalf("found only %d commands in the usage text: %v", len(names), names)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("registering %s's flags panicked: %v", name, r)
				}
			}()
			fs := flag.NewFlagSet("writ "+name, flag.ContinueOnError)
			var g globals
			g.register(fs)
			if _, err := command(name, fs); err != nil && name != "version" && name != "help" {
				t.Fatalf("command(%q): %v", name, err)
			}
		})
	}
}
