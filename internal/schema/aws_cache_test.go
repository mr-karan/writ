package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestAWSCachedAnnotations(t *testing.T) {
	cache := os.Getenv("WRIT_CACHE_DIR")
	if cache == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		cache = filepath.Join(base, "writ")
	}
	indexes, err := filepath.Glob(filepath.Join(cache, "schemas", "v5", "*", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dir string
	var names []string
	for _, index := range indexes {
		data, err := os.ReadFile(index)
		if err != nil {
			t.Fatal(err)
		}
		var cached struct {
			Resources []string `json:"resources"`
		}
		if err := json.Unmarshal(data, &cached); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(cached.Resources, "aws_acm_certificate") {
			dir, names = filepath.Dir(index), cached.Resources
			break
		}
	}
	if dir == "" {
		t.Skip("real hashicorp/aws schema is not cached; run writ init for a pinned AWS provider first (this test never starts or downloads AWS)")
	}
	t.Logf("checking cached AWS schema %s", dir)
	src := Source{Namespace: "hashicorp", Type: "aws"}
	checks := map[string]map[string]string{}
	for wire, ann := range ownershipTable()[src] {
		checks[wire] = map[string]string{}
		for _, path := range ann.Scaled {
			checks[wire][path] = "number"
		}
	}
	for wire, paths := range checks {
		t.Run(wire, func(t *testing.T) {
			i := slices.Index(names, wire)
			if i < 0 {
				t.Fatalf("annotated resource %s missing", wire)
			}
			data, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(i)+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var raw struct {
				Block struct {
					Attributes []struct {
						Name     string          `json:"name"`
						Type     json.RawMessage `json:"type"`
						Optional bool            `json:"optional"`
						Required bool            `json:"required"`
					} `json:"attributes"`
				} `json:"block"`
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			for path, kind := range paths {
				found := false
				for _, a := range raw.Block.Attributes {
					if a.Name != path {
						continue
					}
					found = true
					if strings.Trim(string(a.Type), `"`) != kind || !(a.Optional || a.Required) {
						t.Errorf("%s.%s: type=%s optional=%v required=%v, want settable %s", wire, path, a.Type, a.Optional, a.Required, kind)
					}
				}
				if !found {
					t.Errorf("%s.%s missing", wire, path)
				}
			}
		})
	}
}
