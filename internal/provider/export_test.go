package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	ctyjson "github.com/zclconf/go-cty/cty/json"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

// ServeListStandIn starts the list stand-in of list_test.go in protocol
// version 5, listing ids, and returns a Client connected to it.
func ServeListStandIn(t *testing.T, ids []string) *Client {
	return serveList(t, 5, &listStandIn{ids: ids, failAt: -1})
}

// ListStandInQuery is a list config the stand-in accepts.
var ListStandInQuery = value.ObjectVal(nil)

func ImportFixture(t *testing.T, wire, state string) (*schema.Resource, value.Value) {
	t.Helper()
	p, w, err := buildSchema(awsSrc, "6.70.0", awsFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.ResourceByWire(wire)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := w.resource(wire)
	if err != nil {
		t.Fatal(err)
	}
	v, err := wb.decode(dynamic{json: []byte(state)})
	if err != nil {
		t.Fatal(err)
	}
	return r, v
}

func OfflineAWS(t *testing.T) (*Client, *schema.Provider) {
	t.Helper()
	if os.Getenv("WRIT_OFFLINE_AWS") != "1" {
		t.Skip("run inside unshare -rn with WRIT_OFFLINE_AWS=1")
	}
	dirs, err := DefaultDirs()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dirs.Cache, "providers", "registry.opentofu.org", "hashicorp", "aws", "6.70.0", "linux_amd64", "terraform-provider-aws_v6.70.0")
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	proc, err := start(ctx, binary, Launch{OwnEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proc.shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	proto := proc.client()
	cs, _, err := proto.getSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("WRIT_UPDATE_AWS_FIXTURE") == "1" {
		fixture := awsFixture(t)
		fixture.Provider = cs.Provider
		for _, wire := range []string{"aws_s3_bucket_acl", "aws_s3_bucket_lifecycle_configuration", "aws_s3_bucket_server_side_encryption_configuration", "aws_s3_bucket_versioning", "aws_iam_role_policy"} {
			fixture.Resources[wire] = cs.Resources[wire]
		}
		for wire, r := range fixture.Resources {
			if len(r.Block.Attributes)+len(r.Block.Blocks) > 0 {
				fixture.Resources[wire] = cs.Resources[wire]
			}
		}
		data, err := json.Marshal(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("testdata/aws-6.70.0-schema.json", data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for wire, fixture := range awsFixture(t).Resources {
		if len(fixture.Block.Attributes)+len(fixture.Block.Blocks) == 0 {
			continue
		}
		got, err := json.Marshal(cs.Resources[wire])
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("schema fixture differs from the real provider for %s", wire)
		}
	}
	p, w, err := buildSchema(awsSrc, "6.70.0", cs)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{src: awsSrc, proc: proc, proto: proto, wire: w}
	cfg := value.ObjectVal(map[string]value.Value{
		"region": value.StringVal("us-east-1"), "access_key": value.StringVal("synthetic"), "secret_key": value.StringVal("synthetic"),
		"skip_credentials_validation": value.BoolVal(true), "skip_requesting_account_id": value.BoolVal(true), "skip_metadata_api_check": value.StringVal("true"), "skip_region_validation": value.BoolVal(true),
	})
	if err := c.configure(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return c, p
}

func IdentityEqual(a, b Identity) bool { return a.equal(b) }

// IdentityOf encodes the identity whose JSON is js as a provider does.
func IdentityOf(t *testing.T, js string) Identity {
	t.Helper()
	ty, err := ctyjson.ImpliedType([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	v, err := ctyjson.Unmarshal([]byte(js), ty)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ctymsgpack.Marshal(v, ty)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{data: dynamic{msgpack: b}}
}

// ServeIdentityStandIn starts the identity stand-in of identity_test.go
// in protocol version 5 or 6. With none it returns no identity; with
// moves its plans keep the state and move the identity.
func ServeIdentityStandIn(t *testing.T, version int, none, moves bool) (*Client, *schema.Provider) {
	return serveIdentity(t, version, &identityStandIn{none: none, moves: moves})
}
