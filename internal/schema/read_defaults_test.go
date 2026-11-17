package schema

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestReadDefaultsHaveProviderCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/read-default-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string][]struct {
		Wire              string
		Defaults, Changes map[string]string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]map[string]string{}
	for version, entries := range cases {
		want[version] = map[string]map[string]string{}
		for _, entry := range entries {
			want[version][entry.Wire] = entry.Defaults
			for field := range entry.Defaults {
				if _, ok := entry.Changes[field]; !ok {
					t.Fatalf("%s %s lacks change proof for %s", version, entry.Wire, field)
				}
			}
		}
	}
	got := map[string]map[string]map[string]string{}
	for _, ann := range annotations() {
		for wire, a := range ann.Types {
			if a.ReadDefaultsVersion == "" {
				continue
			}
			if got[a.ReadDefaultsVersion] == nil {
				got[a.ReadDefaultsVersion] = map[string]map[string]string{}
			}
			got[a.ReadDefaultsVersion][wire] = a.ReadDefaults
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read-default annotations = %v, provider cases = %v", got, want)
	}
}
