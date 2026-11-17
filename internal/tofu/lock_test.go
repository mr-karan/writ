package tofu

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// sopsTerraform is what Terraform 1.17.1 wrote for carlpett/sops 1.1.1 on
// two platforms. OpenTofu's registry serves the same archives: tofu
// providers lock wrote the same zh: hashes, and both h1: hashes among its
// own.
const sopsTerraform = `provider "registry.terraform.io/carlpett/sops" {
  version     = "1.1.1"
  constraints = "1.1.1"
  hashes = [
    "h1:0lv+4VCaIRTkBAMXmCxSJC1dsYPuoyBAvnrLsofcseA=",
    "zh:175ec198e1b4d1cad1ae559ebe8cdf574617805010c22dfb8af93a2057ba8332",
  ]
}
`

func TestFromTerraform(t *testing.T) {
	tofuLock, err := os.ReadFile("testdata/lock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	// testdata/terraform.lock.hcl is what Terraform 1.17.1 wrote for the
	// providers of testdata/lock.hcl.
	tfLock, err := os.ReadFile("testdata/terraform.lock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	sopsHashes := []string{"h1:0lv+4VCaIRTkBAMXmCxSJC1dsYPuoyBAvnrLsofcseA=", "zh:175ec198e1b4d1cad1ae559ebe8cdf574617805010c22dfb8af93a2057ba8332"}
	none := func(string) bool { return false }
	for _, tc := range []struct {
		name  string
		lock  string
		named func(string) bool
		// want is nil when the lock must come back as it was.
		want  []Provider
		moved []Moved
		err   string
	}{
		{name: "a lock that OpenTofu wrote is kept", lock: string(tofuLock), named: none},
		{
			name: "hashicorp/ providers move to OpenTofu's registry without their hashes", lock: string(tfLock), named: none,
			want: []Provider{
				{Source: "registry.opentofu.org/hashicorp/local", Version: "2.9.1"},
				{Source: "registry.opentofu.org/hashicorp/random", Version: "3.9.1"},
				{Source: "registry.opentofu.org/hashicorp/tls", Version: "4.4.2"},
			},
			moved: []Moved{
				{From: "registry.terraform.io/hashicorp/local", To: "registry.opentofu.org/hashicorp/local", Rehashed: true},
				{From: "registry.terraform.io/hashicorp/random", To: "registry.opentofu.org/hashicorp/random", Rehashed: true},
				{From: "registry.terraform.io/hashicorp/tls", To: "registry.opentofu.org/hashicorp/tls", Rehashed: true},
			},
		},
		{
			name: "a provider of another namespace keeps its hashes", lock: sopsTerraform, named: none,
			want:  []Provider{{Source: "registry.opentofu.org/carlpett/sops", Version: "1.1.1", Hashes: sopsHashes}},
			moved: []Moved{{From: "registry.terraform.io/carlpett/sops", To: "registry.opentofu.org/carlpett/sops"}},
		},
		{
			name: "a provider the code names by Terraform's registry stays there", lock: sopsTerraform,
			named: func(s string) bool { return s == "registry.terraform.io/carlpett/sops" },
		},
		{
			name: "a hashicorp/ provider the code names by Terraform's registry is refused", lock: string(tfLock),
			named: func(s string) bool { return s == "registry.terraform.io/hashicorp/random" },
			err:   "name it hashicorp/random",
		},
		{
			name: "an entry of OpenTofu's registry is kept beside one of Terraform's, which is dropped",
			lock: string(tofuLock) + strings.Replace(sopsTerraform, "carlpett/sops", "hashicorp/random", 1), named: none,
			moved: []Moved{{From: "registry.terraform.io/hashicorp/random", To: "registry.opentofu.org/hashicorp/random"}},
		},
		{
			name: "a hash that could break out of its string is refused",
			lock: strings.Replace(sopsTerraform, "zh:175e", `zh:\"}`, 1), named: none,
			err: "has a hash that is not",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, moved, err := FromTerraform([]byte(tc.lock), tc.named)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("FromTerraform: %v, want an error that says %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(moved, tc.moved) {
				t.Errorf("moved %+v, want %+v", moved, tc.moved)
			}
			if tc.want == nil && tc.moved == nil {
				if string(got) != tc.lock {
					t.Fatalf("the lock changed:\n%s", got)
				}
				return
			}
			providers, err := LockedProviders(got)
			if err != nil {
				t.Fatalf("the lock FromTerraform wrote does not read back: %v\n%s", err, got)
			}
			if tc.want == nil {
				// The OpenTofu entries are kept as they were.
				if tc.want, err = LockedProviders(tofuLock); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(providers, tc.want) {
				t.Fatalf("the lock pins %+v, want %+v\n%s", providers, tc.want, got)
			}
		})
	}
}
