package provider

import (
	"slices"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/schema"
)

var (
	tlsSrc    = schema.Source{Namespace: "hashicorp", Type: "tls"}
	randomSrc = schema.Source{Namespace: "hashicorp", Type: "random"}
	localSrc  = schema.Source{Namespace: "hashicorp", Type: "local"}
)

func hash(b byte) [32]byte {
	var h [32]byte
	for i := range h {
		h[i] = b
	}
	return h
}

func entry(src schema.Source, version, platform string, archive, binary byte) LockEntry {
	return LockEntry{Source: src, Version: version, Platform: platform, Archive: hash(archive), Binary: hash(binary)}
}

func TestLockSortsAndFinds(t *testing.T) {
	acme := schema.Source{Host: "example.com", Namespace: "acme", Type: "thing"}
	// Out of order on purpose: entries sort by source, version, platform.
	l := (&Lock{}).
		with(entry(tlsSrc, "4.4.2", "linux_amd64", 1, 2)).
		with(entry(acme, "0.1.0", "linux_amd64", 7, 8)).
		with(entry(randomSrc, "3.9.1", "darwin_arm64", 5, 6)).
		with(entry(randomSrc, "3.9.1", "linux_amd64", 3, 4))
	want := []LockEntry{
		entry(acme, "0.1.0", "linux_amd64", 7, 8),
		entry(randomSrc, "3.9.1", "darwin_arm64", 5, 6),
		entry(randomSrc, "3.9.1", "linux_amd64", 3, 4),
		entry(tlsSrc, "4.4.2", "linux_amd64", 1, 2),
	}
	if got := l.Entries(); !slices.Equal(got, want) {
		t.Errorf("Entries:\n got %+v\nwant %+v", got, want)
	}

	e, ok := l.find(randomSrc, "3.9.1", "darwin_arm64")
	if !ok || e.Archive != hash(5) || e.Binary != hash(6) {
		t.Errorf("find random darwin_arm64 = %+v, %v", e, ok)
	}
	if _, ok := l.find(randomSrc, "3.9.0", "linux_amd64"); ok {
		t.Error("found a version that is not pinned")
	}
	if _, ok := l.find(acme, "0.1.0", "linux_amd64"); !ok {
		t.Error("an entry with an explicit host is not found by its source")
	}
}

func TestLockFindUsesCurrentPlatform(t *testing.T) {
	l := (&Lock{}).
		with(entry(tlsSrc, "4.4.2", CurrentPlatform(), 1, 2)).
		with(entry(tlsSrc, "4.4.2", "plan9_mips", 3, 4))
	e, ok := l.Find(tlsSrc, "4.4.2")
	if !ok || e.Platform != CurrentPlatform() || e.Binary != hash(2) {
		t.Errorf("Find = %+v, %v; want the %s entry", e, ok, CurrentPlatform())
	}
}

func TestLockWith(t *testing.T) {
	l := &Lock{}
	a := LockEntry{Source: tlsSrc, Version: "4.4.2", Platform: "linux_amd64", Archive: hash(1), Binary: hash(2)}
	b := LockEntry{Source: randomSrc, Version: "3.9.1", Platform: "linux_amd64", Archive: hash(3), Binary: hash(4)}
	ab := l.with(a).with(b)
	if !slices.Equal(ab.Entries(), l.with(b).with(a).Entries()) {
		t.Error("the same entries added in another order give another lock")
	}
	if len(l.Entries()) != 0 {
		t.Error("with modified the lock it was called on")
	}
	a2 := a
	a2.Binary = hash(9)
	if got, want := ab.with(a2).Entries(), []LockEntry{b, a2}; !slices.Equal(got, want) {
		t.Errorf("replacing an entry gave %+v, want %+v", got, want)
	}
}

func TestSelectVersion(t *testing.T) {
	vs := registryVersions{Versions: []registryVersion{
		{Version: "3.9.1", Protocols: []string{"5.0"}, Platforms: []registryPlatform{{"linux", "amd64"}, {"darwin", "arm64"}}},
		{Version: "4.0.0", Protocols: []string{"6.0"}, Platforms: []registryPlatform{{"linux", "amd64"}}},
		{Version: "1.0.0", Protocols: []string{"4.0"}, Platforms: []registryPlatform{{"linux", "amd64"}}},
		{Version: "3.9.10", Protocols: []string{"5.1", "6.0"}, Platforms: []registryPlatform{{"linux", "amd64"}}},
	}}
	tests := []struct {
		version, platform, err string
	}{
		{"3.9.1", "linux_amd64", ""},
		{"3.9.1", "darwin_arm64", ""},
		{"4.0.0", "linux_amd64", ""},
		{"3.9.10", "linux_amd64", ""},
		{"3.9", "linux_amd64", "has no version 3.9"},
		{"3.9.2", "linux_amd64", "has no version 3.9.2"},
		{"4.0.0", "darwin_arm64", "has no build for darwin_arm64"},
		{"1.0.0", "linux_amd64", "serves plugin protocols [4.0]"},
	}
	for _, tt := range tests {
		t.Run(tt.version+"/"+tt.platform, func(t *testing.T) {
			v, err := selectVersion(vs, randomSrc, tt.version, tt.platform)
			if tt.err == "" {
				if err != nil || v.Version != tt.version {
					t.Errorf("selectVersion = %q, %v; want exactly %s", v.Version, err, tt.version)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("selectVersion error = %v, want it to contain %q", err, tt.err)
			}
		})
	}
}
