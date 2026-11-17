package main

import "runtime/debug"

// A release build sets version with -ldflags. `go install
// github.com/mr-karan/writ/cmd/writ@v0.5.0` sets no flags, so writ reports
// the module version Go recorded in the binary instead.
func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		version = moduleVersion(info.Main.Version, version)
	}
}

// moduleVersion is mod, the main module's version in the build info,
// unless Go recorded none.
func moduleVersion(mod, fallback string) string {
	if mod == "" || mod == "(devel)" {
		return fallback
	}
	return mod
}
