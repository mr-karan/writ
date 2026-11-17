package schema

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// filledFixtures maps each provider that has Filled entries to the schema
// its evidence came from. The fixtures are schemas real providers returned.
var filledFixtures = map[Source]string{
	{Namespace: "hashicorp", Type: "aws"}:    "aws-6.70.0-schema.json",
	{Namespace: "hashicorp", Type: "tls"}:    "tls-4.4.2-schema.json",
	{Namespace: "hashicorp", Type: "random"}: "random-3.9.1-schema.json",
	{Namespace: "hashicorp", Type: "local"}:  "local-2.9.1-schema.json",
}

// TestFilledAttributesExist checks every Filled entry against the schema of
// the provider version its evidence came from: the resource type or data
// source exists and the entry names a top-level computed attribute of it.
func TestFilledAttributesExist(t *testing.T) {
	schemas := map[Source]fixtureSchema{}
	for src, name := range filledFixtures {
		schemas[src] = readFixtureSchema(t, name)
	}
	for _, err := range filledErrors(annotations(), schemas) {
		t.Error(err)
	}
}

// TestFilledErrorsReportsBadTables checks that filledErrors fails a table
// that is empty or has a wrong entry, so TestFilledAttributesExist cannot
// pass because it checked nothing.
func TestFilledErrorsReportsBadTables(t *testing.T) {
	local := Source{Namespace: "hashicorp", Type: "local"}
	tls := Source{Namespace: "hashicorp", Type: "tls"}
	schemas := map[Source]fixtureSchema{local: readFixtureSchema(t, "local-2.9.1-schema.json")}
	good := annotation{Filled: []string{"id"}}
	both := func(types, data map[string]annotation) map[Source]providerAnnotations {
		return map[Source]providerAnnotations{local: {Types: types, Data: data}}
	}
	file := map[string]annotation{"local_file": good}
	tests := []struct {
		name string
		anns map[Source]providerAnnotations
		want []string // a part of each error, in order
	}{
		{"good", both(file, file), nil},
		{"no entries", both(nil, nil), []string{"no resource type has a Filled entry", "no data source has a Filled entry"}},
		{"no data source entries", both(file, nil), []string{"no data source has a Filled entry"}},
		{"no such resource type", both(map[string]annotation{"local_dir": good}, file), []string{"local_dir: no such resource type"}},
		{"no such data source", both(file, map[string]annotation{"local_dir": good}), []string{"local_dir: no such data source"}},
		{"no such attribute", both(map[string]annotation{"local_file": {Filled: []string{"size"}}}, file), []string{"local_file.size: no such attribute"}},
		{"not computed", both(map[string]annotation{"local_file": {Filled: []string{"filename"}}}, file), []string{"resource type local_file.filename is not computed"}},
		{"data source not computed", both(file, map[string]annotation{"local_file": {Filled: []string{"filename"}}}), []string{"data source local_file.filename is not computed"}},
		{"no fixture", map[Source]providerAnnotations{local: {Types: file, Data: file}, tls: {Types: map[string]annotation{"tls_private_key": good}}}, []string{"hashicorp/tls has Filled entries but no schema fixture"}},
		{"no annotations", map[Source]providerAnnotations{}, []string{"hashicorp/local has a schema fixture but no annotations"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := filledErrors(tt.anns, schemas)
			ok := len(errs) == len(tt.want)
			for i := 0; ok && i < len(errs); i++ {
				ok = strings.Contains(errs[i].Error(), tt.want[i])
			}
			if !ok {
				t.Fatalf("errors = %v, want ones that say %q", errs, tt.want)
			}
		})
	}
}

// filledErrors checks the Filled entries of anns against schemas. A provider
// with a fixture must have at least one resource and one data source entry,
// if its schema has data sources, so an emptied table fails.
func filledErrors(anns map[Source]providerAnnotations, schemas map[Source]fixtureSchema) []error {
	var errs []error
	for src, pa := range anns {
		fs, ok := schemas[src]
		if !ok {
			if hasFilled(pa.Types) || hasFilled(pa.Data) {
				errs = append(errs, fmt.Errorf("%s has Filled entries but no schema fixture", src))
			}
			continue
		}
		if !hasFilled(pa.Types) {
			errs = append(errs, fmt.Errorf("%s: no resource type has a Filled entry", src))
		}
		if len(fs.DataSources) > 0 && !hasFilled(pa.Data) {
			errs = append(errs, fmt.Errorf("%s: no data source has a Filled entry", src))
		}
		errs = append(errs, kindErrors(src, "resource type", pa.Types, fs.Resources)...)
		errs = append(errs, kindErrors(src, "data source", pa.Data, fs.DataSources)...)
	}
	for src := range schemas {
		if _, ok := anns[src]; !ok {
			errs = append(errs, fmt.Errorf("%s has a schema fixture but no annotations", src))
		}
	}
	return errs
}

// kindErrors checks the Filled entries of one kind, "resource type" or
// "data source", against the fixture blocks of that kind.
func kindErrors(src Source, kind string, anns map[string]annotation, blocks map[string]fixtureBlock) []error {
	var errs []error
	for _, wire := range slices.Sorted(maps.Keys(anns)) {
		filled := anns[wire].Filled
		if len(filled) == 0 {
			continue
		}
		b, ok := blocks[wire]
		if !ok {
			errs = append(errs, fmt.Errorf("%s %s: no such %s in the %s fixture", kind, wire, kind, src))
			continue
		}
		attrs := map[string]fixtureAttr{}
		for _, a := range b.Block.Attributes {
			attrs[a.Name] = a
		}
		for _, name := range filled {
			a, ok := attrs[name]
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("%s %s.%s: no such attribute in the %s fixture", kind, wire, name, src))
			case !a.Computed:
				errs = append(errs, fmt.Errorf("%s %s.%s is not computed, so the provider never fills it", kind, wire, name))
			}
		}
	}
	return errs
}

func hasFilled(m map[string]annotation) bool {
	for _, a := range m {
		if len(a.Filled) > 0 {
			return true
		}
	}
	return false
}

type fixtureAttr struct {
	Name     string `json:"name"`
	Computed bool   `json:"computed"`
}

type fixtureBlock struct {
	Block struct {
		Attributes []fixtureAttr `json:"attributes"`
	} `json:"block"`
}

type fixtureSchema struct {
	Resources   map[string]fixtureBlock `json:"resources"`
	DataSources map[string]fixtureBlock `json:"data_sources"`
}

func readFixtureSchema(t *testing.T, name string) fixtureSchema {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "provider", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var fs fixtureSchema
	if err := json.Unmarshal(data, &fs); err != nil {
		t.Fatal(err)
	}
	return fs
}
