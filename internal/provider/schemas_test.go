package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/schema"
)

var awsSrc = schema.Source{Namespace: "hashicorp", Type: "aws"}

// awsFixture is the hashicorp/aws 6.70.0 schema as GetProviderSchema returned
// it, cut down to keep the repository small: every resource type name is
// there, because inference reads the names, but only the types these tests
// read keep their blocks.
func awsFixture(t *testing.T) *cachedSchema {
	t.Helper()
	data, err := os.ReadFile("testdata/aws-6.70.0-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs cachedSchema
	if err := json.Unmarshal(data, &cs); err != nil {
		t.Fatal(err)
	}
	return &cs
}

func cacheAWS(t *testing.T) (Dirs, *Lock, []Requirement) {
	t.Helper()
	dirs := Dirs{Cache: t.TempDir()}
	entry := LockEntry{Source: awsSrc, Version: "6.70.0", Platform: CurrentPlatform(), Archive: hash(1), Binary: hash(2)}
	if err := writeSchemaCache(dirs, entry.Binary, awsFixture(t)); err != nil {
		t.Fatal(err)
	}
	return dirs, (&Lock{}).with(entry), []Requirement{{Name: "aws", Source: awsSrc, Version: "6.70.0"}}
}

// typeFile is the cache file that holds one resource type.
func typeFile(t *testing.T, dirs Dirs, wire string) string {
	t.Helper()
	c, err := openSchemaCache(dirs, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(c.dir, strconv.Itoa(c.pos[wire])+".json")
}

func TestSchemasReadOnlyTheTypesUsed(t *testing.T) {
	dirs, lock, reqs := cacheAWS(t)
	// Damage every type file but one: a lookup of that one type must still
	// work, which it can only if nothing else was read.
	c, err := openSchemaCache(dirs, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	keep := typeFile(t, dirs, "aws_subnet")
	for i := range c.index.Resources {
		if f := filepath.Join(c.dir, strconv.Itoa(i)+".json"); f != keep {
			if err := os.WriteFile(f, []byte("{"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	schemas, err := Schemas(dirs, lock, reqs)
	if err != nil {
		t.Fatal(err)
	}
	p := schemas[awsSrc]
	if len(p.TypeNames()) != len(c.index.Resources) {
		t.Errorf("TypeNames has %d names, want all %d", len(p.TypeNames()), len(c.index.Resources))
	}
	if _, ok := p.Resource("Subnet"); !ok {
		t.Fatalf("Subnet did not load: %v", p.Err())
	}
	if err := p.Err(); err != nil {
		t.Fatalf("Err = %v after loading only an intact type", err)
	}
	if _, ok := p.Resource("Vpc"); ok {
		t.Fatal("Vpc loaded from a damaged file")
	}
	if err := p.Err(); err == nil || !strings.Contains(err.Error(), "aws_vpc") {
		t.Errorf("Err = %v, want one naming aws_vpc", err)
	}
}

func TestSchemaCacheFormatMismatchIsMissing(t *testing.T) {
	dirs, lock, reqs := cacheAWS(t)
	index := filepath.Join(filepath.Dir(typeFile(t, dirs, "aws_vpc")), "index.json")
	data, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Replace(string(data), fmt.Sprintf(`"format":%d`, schemaFormat), `"format":1`, 1)
	if err := os.WriteFile(index, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Schemas(dirs, lock, reqs)
	var missing *MissingError
	if !errors.As(err, &missing) {
		t.Fatalf("Schemas error = %v, want a *MissingError", err)
	}
}

func TestWriteSchemaCacheReplaces(t *testing.T) {
	dirs, lock, reqs := cacheAWS(t)
	if err := os.WriteFile(typeFile(t, dirs, "aws_vpc"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSchemaCache(dirs, hash(2), awsFixture(t)); err != nil {
		t.Fatal(err)
	}
	schemas, err := Schemas(dirs, lock, reqs)
	if err != nil {
		t.Fatal(err)
	}
	if err := loadAll(schemas[awsSrc]); err != nil {
		t.Fatalf("rewritten cache does not load: %v", err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(dirs.SchemaDir(hash(2))), ".tmp-*"))
	if len(leftovers) > 0 {
		t.Errorf("temporary directories left behind: %v", leftovers)
	}
}

func TestSchemasCacheDataSources(t *testing.T) {
	dirs, lock, reqs := cacheAWS(t)
	schemas, err := Schemas(dirs, lock, reqs)
	if err != nil {
		t.Fatal(err)
	}
	p := schemas[awsSrc]
	if got, want := len(p.DataSourceNames()), len(awsFixture(t).DataSources); got != want {
		t.Errorf("DataSourceNames has %d names, want %d", got, want)
	}
	ds, ok := p.DataSource("Ami")
	if !ok {
		t.Fatalf("Ami did not load: %v", p.Err())
	}
	if _, ok := ds.Block.Lookup([]string{"owners"}); !ok {
		t.Error("aws_ami lost its owners attribute in the cache")
	}
	if _, ok := p.Resource("Ami"); !ok {
		t.Error("the resource aws_ami must stay separate from the data source")
	}
}

func TestSchemasKeepWriteOnlyAndDeprecated(t *testing.T) {
	dirs, lock, reqs := cacheAWS(t)
	schemas, err := Schemas(dirs, lock, reqs)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		typ, attr             string
		writeOnly, deprecated bool
	}{
		{"DbInstance", "password_wo", true, false},
		{"DbInstance", "password", false, false},
		{"IamRole", "managed_policy_arns", false, true},
		{"IamRole", "name", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.typ+"."+tt.attr, func(t *testing.T) {
			r, ok := schemas[awsSrc].Resource(tt.typ)
			if !ok {
				t.Fatalf("%s did not load: %v", tt.typ, schemas[awsSrc].Err())
			}
			a, ok := r.Block.Lookup([]string{tt.attr})
			if !ok {
				t.Fatalf("%s has no attribute %s", tt.typ, tt.attr)
			}
			if a.WriteOnly != tt.writeOnly || a.Deprecated != tt.deprecated {
				t.Errorf("write-only %v, deprecated %v; want %v, %v", a.WriteOnly, a.Deprecated, tt.writeOnly, tt.deprecated)
			}
		})
	}
}

func TestNomadPatchCache(t *testing.T) {
	src := schema.Source{Namespace: "mr-karan", Type: "nomad"}
	for _, sensitive := range []bool{false, true} {
		t.Run(fmt.Sprint(sensitive), func(t *testing.T) {
			dirs := Dirs{Cache: t.TempDir()}
			cs := &cachedSchema{Resources: map[string]rawResource{"nomad_job": {Block: rawBlock{}}}}
			r := cs.Resources["nomad_job"]
			for _, name := range []string{"plan_response", "register_modify_index", "register_eval_id", "register_eval_create_index"} {
				r.Block.Attributes = append(r.Block.Attributes, rawAttribute{Name: name, Type: json.RawMessage(`"string"`), Computed: true, Sensitive: sensitive && name == "plan_response"})
			}
			cs.Resources["nomad_job"] = r
			if err := writeSchemaCache(dirs, hash(2), cs); err != nil {
				t.Fatal(err)
			}
			lock := (&Lock{}).with(LockEntry{Source: src, Version: "2.6.1-writ.1", Platform: CurrentPlatform(), Binary: hash(2)})
			ps, err := Schemas(dirs, lock, []Requirement{{Source: src, Version: "2.6.1-writ.1"}})
			if err != nil {
				t.Fatal(err)
			}
			job, err := ps[src].ResourceByWire("nomad_job")
			if err != nil || !job.PatchedNomad() {
				t.Fatal("cached patch was not recognized", err)
			}
			a, _ := job.Block.Lookup([]string{"plan_response"})
			if !a.Type.IsSecret() {
				t.Fatal("plan_response was not sensitive")
			}
			r.Block.Attributes = r.Block.Attributes[:3]
			cs.Resources["nomad_job"] = r
			if err := writeSchemaCache(dirs, hash(2), cs); err != nil {
				t.Fatal(err)
			}
			ps, err = Schemas(dirs, lock, []Requirement{{Source: src, Version: "2.6.1-writ.1"}})
			if err != nil {
				t.Fatal(err)
			}
			job, err = ps[src].ResourceByWire("nomad_job")
			if err != nil || job.PatchedNomad() {
				t.Fatal("incomplete patch accepted", err)
			}
		})
	}
}

func TestSchemaCacheUsesDeprecationFormat(t *testing.T) {
	dirs, _, _ := cacheAWS(t)
	file := filepath.Join(dirs.Cache, "schemas", "v5", fmt.Sprintf("%x", hash(2)), "index.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var index struct{ Format int }
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if index.Format != 5 {
		t.Fatalf("deprecation metadata requires format 5, got %d", index.Format)
	}
}
