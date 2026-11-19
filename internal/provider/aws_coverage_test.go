package provider

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
)

// TestAWSReferenceCoverage counts, over every resource type of a real
// hashicorp/aws schema in the cache, the settable top-level slots named
// <x>_id, <x>_ids, <x>_arn, and <x>_arns that are typed references. It
// fails when the count falls under a floor, so a change to inference that
// loses coverage shows. The 6.70.0 schema had 224 such id slots and 18 ARN
// slots typed before ARN slots were inferred.
func TestAWSReferenceCoverage(t *testing.T) {
	dirs, err := DefaultDirs()
	if err != nil {
		t.Fatal(err)
	}
	c := cachedAWSSchema(t, dirs)
	if c == nil {
		t.Skip("real hashicorp/aws schema is not cached; run writ init for a pinned AWS provider first (this test never starts or downloads AWS)")
	}
	p, _, err := c.build(schema.Source{Namespace: "hashicorp", Type: "aws"}, "cached")
	if err != nil {
		t.Fatal(err)
	}
	// plain says, for some slots, whether they must stay plain: the true
	// ones accept more than one kind of object.
	plain := map[string]bool{
		"aws_datasync_location_s3.s3_bucket_arn":       true,
		"aws_appsync_domain_name.certificate_arn":      true,
		"aws_eks_access_policy_association.policy_arn": true,
		"aws_ecs_task_definition.execution_role_arn":   false,
		"aws_lambda_function.kms_key_arn":              false,
	}
	type count struct{ typed, plain int }
	var ids, arns count
	for _, wire := range c.index.Resources {
		r, err := p.ResourceByWire(wire)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range r.Block.Attributes {
			if !a.Optional && !a.Required {
				continue
			}
			if want, ok := plain[wire+"."+a.Name]; ok && isRef(a.Type) == want {
				t.Errorf("%s.%s is %s; want plain: %v", wire, a.Name, a.Type, want)
			}
			var n *count
			switch {
			case strings.HasSuffix(a.Name, "_id"), strings.HasSuffix(a.Name, "_ids"):
				n = &ids
			case strings.HasSuffix(a.Name, "_arn"), strings.HasSuffix(a.Name, "_arns"):
				n = &arns
			default:
				continue
			}
			if isRef(a.Type) {
				n.typed++
			} else {
				n.plain++
			}
		}
	}
	t.Logf("%d resource types in %s", len(c.index.Resources), c.dir)
	t.Logf("id slots: %d typed, %d plain", ids.typed, ids.plain)
	t.Logf("ARN slots: %d typed, %d plain", arns.typed, arns.plain)
	if ids.typed < 230 {
		t.Errorf("%d id slots are typed, want at least 230", ids.typed)
	}
	if arns.typed < 250 {
		t.Errorf("%d ARN slots are typed, want at least 250", arns.typed)
	}
}

// The ownership annotations hold for a real hashicorp/aws schema in the
// cache: each field an autoscaler moves is a settable number there, and
// the built resource type marks it Scaled.
func TestAWSCachedAnnotations(t *testing.T) {
	dirs, err := DefaultDirs()
	if err != nil {
		t.Fatal(err)
	}
	c := cachedAWSSchema(t, dirs)
	if c == nil {
		t.Skip("real hashicorp/aws schema is not cached; run writ init for a pinned AWS provider first (this test never starts or downloads AWS)")
	}
	t.Logf("checking cached AWS schema %s", c.dir)
	p, _, err := c.build(schema.Source{Namespace: "hashicorp", Type: "aws"}, "cached")
	if err != nil {
		t.Fatal(err)
	}
	for wire, scaled := range map[string][]string{
		"aws_autoscaling_group": {"desired_capacity"},
		"aws_ecs_service":       {"desired_count"},
	} {
		t.Run(wire, func(t *testing.T) {
			r, err := p.ResourceByWire(wire)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(r.Scaled, scaled) {
				t.Fatalf("Scaled is %v, want %v", r.Scaled, scaled)
			}
			for _, path := range scaled {
				if a, _ := r.Block.Lookup(strings.Split(path, ".")); a.Type.Kind() != types.Float {
					t.Errorf("%s.%s is %s, want a number", wire, path, a.Type)
				}
			}
		})
	}
}

func isRef(t types.Type) bool {
	return t.Kind() == types.Ref || t.Kind() == types.List && t.Elem().Kind() == types.Ref
}

// cachedAWSSchema returns a cached hashicorp/aws schema, or nil when the
// cache holds none.
func cachedAWSSchema(t *testing.T, dirs Dirs) *schemaCache {
	t.Helper()
	indexes, err := filepath.Glob(filepath.Join(filepath.Dir(dirs.SchemaDir([32]byte{})), "*", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range indexes {
		var binary [32]byte
		b, err := hex.DecodeString(filepath.Base(filepath.Dir(index)))
		if err != nil || len(b) != len(binary) {
			continue
		}
		copy(binary[:], b)
		c, err := openSchemaCache(dirs, binary)
		if errors.Is(err, errNoSchema) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(c.index.Resources, "aws_acm_certificate") {
			return c
		}
	}
	return nil
}
