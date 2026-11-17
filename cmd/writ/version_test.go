package main

import "testing"

func TestModuleVersion(t *testing.T) {
	tests := []struct {
		name, mod, want string
	}{
		{name: "go install of a tag", mod: "v0.5.0", want: "v0.5.0"},
		{name: "go build in a checkout", mod: "v0.0.0-20261110153300-3d7af5a1b2c3+dirty", want: "v0.0.0-20261110153300-3d7af5a1b2c3+dirty"},
		{name: "no module version", mod: "(devel)", want: "dev"},
		{name: "no build info version", mod: "", want: "dev"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := moduleVersion(tc.mod, "dev"); got != tc.want {
				t.Fatalf("moduleVersion(%q) = %q, want %q", tc.mod, got, tc.want)
			}
		})
	}
}
