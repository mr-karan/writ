package schema

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mr-karan/writ/internal/types"
)

func TestParseSource(t *testing.T) {
	tests := []struct {
		in      string
		want    Source
		short   string
		wantErr bool
	}{
		{in: "hashicorp/tls", want: Source{Namespace: "hashicorp", Type: "tls"}, short: "hashicorp/tls"},
		{in: "registry.opentofu.org/hashicorp/tls", want: Source{Namespace: "hashicorp", Type: "tls"}, short: "hashicorp/tls"},
		{in: "example.com/acme/thing-2", want: Source{Host: "example.com", Namespace: "acme", Type: "thing-2"}, short: "example.com/acme/thing-2"},
		{in: "tls", wantErr: true},
		{in: "a/b/c/d", wantErr: true},
		{in: "HashiCorp/tls", wantErr: true},
		{in: "hashicorp/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseSource(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSource(%q) = %v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSource(%q): %v", tt.in, err)
			}
			if got != tt.want || got.String() != tt.short {
				t.Fatalf("ParseSource(%q) = %#v (%s), want %#v (%s)", tt.in, got, got, tt.want, tt.short)
			}
		})
	}
}

func tlsFixture(t *testing.T) *Provider {
	t.Helper()
	subject := NestedBlock{Name: "subject", Nesting: NestList, Block: Block{Attributes: []Attribute{
		{Name: "common_name", Type: types.TString, Optional: true},
	}}}
	raw := []RawResource{
		{Wire: "tls_private_key", Block: Block{Attributes: []Attribute{
			{Name: "id", Type: types.TString, Computed: true},
			{Name: "algorithm", Type: types.TString, Required: true},
			{Name: "rsa_bits", Type: types.TInt, Optional: true, Computed: true},
			{Name: "private_key_pem", Type: types.TString, Computed: true, Sensitive: true},
		}}},
		{Wire: "tls_self_signed_cert", Version: 1, Block: Block{
			Attributes: []Attribute{
				{Name: "private_key_pem", Type: types.TString, Required: true, Sensitive: true},
				{Name: "cert_pem", Type: types.TString, Computed: true},
			},
			Blocks: []NestedBlock{subject},
		}},
		{Wire: "tls_locally_signed_cert", Block: Block{
			Attributes: []Attribute{
				{Name: "cert_request_pem", Type: types.TString, Required: true},
				{Name: "ca_cert_pem", Type: types.TString, Required: true},
			},
			Blocks: []NestedBlock{{Name: "extra", Nesting: NestList, MaxItems: 1, Block: Block{}}, {Name: "many", Nesting: NestSet, Block: Block{}}},
		}},
	}
	p, err := NewProvider(Source{Namespace: "hashicorp", Type: "tls"}, "4.4.2", Block{}, raw)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func TestNewProviderNamesAndAnnotations(t *testing.T) {
	p := tlsFixture(t)

	if got, want := p.TypeNames(), []string{"LocallySignedCert", "PrivateKey", "SelfSignedCert"}; !equalStrings(got, want) {
		t.Fatalf("TypeNames() = %v, want %v", got, want)
	}

	key, ok := p.Resource("PrivateKey")
	if !ok || key.Wire != "tls_private_key" || key.Name.String() != "tls.PrivateKey" {
		t.Fatalf("PrivateKey = %+v, %v; want tls.PrivateKey with wire tls_private_key", key, ok)
	}
	checks := map[string]string{
		"id":              "Id<tls.PrivateKey>",
		"algorithm":       `"ECDSA" | "ED25519" | "RSA"`,
		"private_key_pem": "Secret<Pem<tls.PrivateKey>>",
		"rsa_bits":        "Int",
	}
	for field, want := range checks {
		a, ok := key.Block.Lookup([]string{field})
		if !ok || a.Type.String() != want {
			t.Errorf("PrivateKey.%s has type %v (present %v), want %s", field, a.Type, ok, want)
		}
	}
	if id, _ := key.Block.Lookup([]string{"id"}); id.Optional || !id.Computed {
		t.Errorf("id = %+v, want computed and not settable", id)
	}
	if _, ok := p.Resource("tls_private_key"); ok {
		t.Error("Resource looks up Writ names, not wire names")
	}
}

func TestNestedBlockShapes(t *testing.T) {
	p := tlsFixture(t)
	block := func(r *Resource, name string) NestedBlock {
		t.Helper()
		for _, nb := range r.Block.Blocks {
			if nb.Name == name {
				return nb
			}
		}
		t.Fatalf("%s has no block %s", r.Wire, name)
		return NestedBlock{}
	}

	cert, _ := p.Resource("SelfSignedCert")
	if subject := block(cert, "subject"); !subject.IsRecord() || subject.MinItems != 0 {
		t.Errorf("subject = %+v; the annotation marks it single, so it is an optional record", subject)
	}

	lsc, _ := p.Resource("LocallySignedCert")
	if !block(lsc, "extra").IsRecord() {
		t.Error("a list block with max_items 1 is a record")
	}
	if block(lsc, "many").IsRecord() {
		t.Error("an unbounded set block is a list of records")
	}
	if a, _ := lsc.Block.Lookup([]string{"ca_cert_pem"}); a.Type.String() != "Pem<tls.LocallySignedCert | tls.SelfSignedCert>" {
		t.Errorf("ca_cert_pem = %v, want a Pem of either certificate type", a.Type)
	}
}

func TestLookupCrossesNestedBlocks(t *testing.T) {
	p := tlsFixture(t)
	cert, _ := p.Resource("SelfSignedCert")
	if a, ok := cert.Block.Lookup([]string{"subject", "common_name"}); !ok || a.Name != "common_name" {
		t.Fatalf("Lookup(subject.common_name) = %+v, %v", a, ok)
	}
	if _, ok := cert.Block.Lookup([]string{"subject", "missing"}); ok {
		t.Fatal("Lookup of a missing attribute must fail")
	}
	if _, ok := cert.Block.Lookup(nil); ok {
		t.Fatal("Lookup of an empty path must fail")
	}
}

func TestNewProviderRejectsForeignResourceNames(t *testing.T) {
	_, err := NewProvider(Source{Namespace: "hashicorp", Type: "tls"}, "4.4.2", Block{}, []RawResource{{Wire: "random_pet"}})
	if err == nil {
		t.Fatal("a resource type without the provider prefix must be rejected")
	}
}

func TestAnnotationsRefineStrings(t *testing.T) {
	for src, pa := range annotations() {
		for wire, ann := range pa.Types {
			for path, refined := range ann.Attrs {
				if m, ok := types.Assign(types.TString, refined); !ok {
					t.Errorf("%s %s.%s: %s", src, wire, path, m.Message(path))
				}
			}
		}
	}
}

// TestAnnotationReferencesNameFamilies guards against a reference that can
// never apply: a target outside every family is skipped silently.
func TestAnnotationReferencesNameFamilies(t *testing.T) {
	for src, pa := range annotations() {
		members := map[string]bool{}
		for _, f := range pa.Families {
			for _, w := range f.Wires {
				if members[w] {
					t.Errorf("%s: %s is in two families", src, w)
				}
				members[w] = true
			}
		}
		for name, w := range pa.Names {
			if !members[w] {
				t.Errorf("%s: name %s refers to %s, which is in no family", src, name, w)
			}
		}
		for suffix, ws := range pa.Suffixes {
			if _, _, ok := slotStem(suffix, types.TString); !ok {
				t.Errorf("%s: suffix %s ends with neither _id nor _arn", src, suffix)
			}
			for _, w := range ws {
				if !members[w] {
					t.Errorf("%s: suffix %s refers to %s, which is in no family", src, suffix, w)
				}
			}
		}
		for wire, ann := range mergeAnnotations(pa.Types, pa.Data) {
			for path, w := range ann.Refs {
				if !members[w] {
					t.Errorf("%s %s.%s refers to %s, which is in no family", src, wire, path, w)
				}
			}
		}
	}
}

func TestLazyProviderLoadsEachTypeOnce(t *testing.T) {
	var calls atomic.Int32
	load := func(wire string) (RawResource, error) {
		calls.Add(1)
		if wire == "random_bytes" {
			return RawResource{}, errors.New("unreadable")
		}
		return RawResource{Wire: wire, Block: Block{Attributes: []Attribute{{Name: "id", Type: types.TString, Computed: true}}}}, nil
	}
	p, err := NewLazy(Source{Namespace: "hashicorp", Type: "random"}, "3.9.1", Block{}, Types{Wires: []string{"random_pet", "random_id", "random_bytes"}, Load: load}, Types{})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.TypeNames(); !equalStrings(got, []string{"Bytes", "Id", "Pet"}) || calls.Load() != 0 {
		t.Fatalf("TypeNames = %v after %d loads, want every name and no loads", got, calls.Load())
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, ok := p.Resource("Pet"); !ok {
				t.Error("Pet did not load")
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("eight lookups of one type loaded it %d times, want 1", calls.Load())
	}
	if p.Err() != nil {
		t.Fatalf("Err = %v before any failure", p.Err())
	}
	if _, ok := p.Resource("Bytes"); ok {
		t.Fatal("a type that failed to load was found")
	}
	if _, err := p.ResourceByWire("random_bytes"); err == nil {
		t.Error("ResourceByWire hid the load error")
	}
	if err := p.Err(); err == nil {
		t.Error("Err did not report the failed load")
	}
	if _, ok := p.Resource("Id"); !ok {
		t.Error("a failed load of one type hid another")
	}
}

// Without families in the annotation table, a provider gets no inferred
// references, only its own typed ids.
func TestNoInferenceWithoutFamilies(t *testing.T) {
	raw := []RawResource{
		{Wire: "random_pet", Block: Block{Attributes: []Attribute{{Name: "id", Type: types.TString, Computed: true}}}},
		{Wire: "random_string", Block: Block{Attributes: []Attribute{
			{Name: "id", Type: types.TString, Optional: true, Computed: true},
			{Name: "pet_id", Type: types.TString, Optional: true},
		}}},
	}
	p, err := NewProvider(Source{Namespace: "hashicorp", Type: "random"}, "3.9.1", Block{}, raw)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := p.Resource("String")
	if a, _ := r.Block.Lookup([]string{"pet_id"}); a.Type.String() != "String" {
		t.Errorf("pet_id = %s, want String", a.Type)
	}
	if a, _ := r.Block.Lookup([]string{"id"}); a.Type.String() != "Id<random.String>" || a.Optional {
		t.Errorf("id = %+v, want a read-only Id<random.String>", a)
	}
}

func mergeAnnotations(a, b map[string]annotation) map[string]annotation {
	out := map[string]annotation{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out["data "+k] = v
	}
	return out
}

func awsDataFixture(t *testing.T) *Provider {
	t.Helper()
	str := func(name string, optional, computed bool) Attribute {
		return Attribute{Name: name, Type: types.TString, Optional: optional, Computed: computed}
	}
	resources := map[string]RawResource{
		"aws_vpc":    {Wire: "aws_vpc", Block: Block{Attributes: []Attribute{str("id", true, true)}}},
		"aws_subnet": {Wire: "aws_subnet", Block: Block{Attributes: []Attribute{str("id", true, true), str("vpc_id", false, false)}}},
	}
	data := map[string]RawResource{
		"aws_vpc": {Wire: "aws_vpc", Block: Block{
			Attributes: []Attribute{str("id", true, true), str("cidr_block", true, true), str("owner_id", false, true)},
			Blocks: []NestedBlock{
				{Name: "filter", Nesting: NestSet, Block: Block{Attributes: []Attribute{{Name: "name", Type: types.TString, Required: true}}}},
				{Name: "timeouts", Nesting: NestSingle, Block: Block{Attributes: []Attribute{str("read", true, false)}}},
			},
		}},
		"aws_subnet":  {Wire: "aws_subnet", Block: Block{Attributes: []Attribute{str("id", true, true), str("vpc_id", true, true)}}},
		"aws_subnets": {Wire: "aws_subnets", Block: Block{Attributes: []Attribute{str("id", false, true), {Name: "ids", Type: types.ListOf(types.TString), Computed: true}}}},
		"aws_region": {Wire: "aws_region", Block: Block{Attributes: []Attribute{
			str("id", true, true), str("name", true, true),
			{Name: "token", Type: types.TString, Computed: true, Sensitive: true},
		}}},
	}
	loader := func(m map[string]RawResource) Types {
		var wires []string
		for w := range m {
			wires = append(wires, w)
		}
		return Types{Wires: wires, Load: func(w string) (RawResource, error) { return m[w], nil }}
	}
	p, err := NewLazy(Source{Namespace: "hashicorp", Type: "aws"}, "6.70.0", Block{}, loader(resources), loader(data))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDataSources(t *testing.T) {
	p := awsDataFixture(t)
	if got := p.DataSourceNames(); !equalStrings(got, []string{"Region", "Subnet", "Subnets", "Vpc"}) {
		t.Fatalf("DataSourceNames = %v", got)
	}
	if _, ok := p.DataSource("aws_vpc"); ok {
		t.Error("DataSource looks up Writ names, not wire names")
	}
	if _, err := p.DataSourceByWire("aws_ami"); err == nil {
		t.Error("DataSourceByWire found a data source the provider lacks")
	}
	vpc, ok := p.DataSource("Vpc")
	if !ok || vpc.Wire != "aws_vpc" || vpc.Name.String() != "aws.Vpc" {
		t.Fatalf("DataSource(Vpc) = %+v, %v", vpc, ok)
	}
	tests := []struct {
		ds, attr, want string
		filled         bool
	}{
		// The id of a data source is the id of the resource type it shares a
		// name with, so it flows into that resource's reference slots.
		{"Vpc", "id", "Id<aws.Vpc>", true},
		{"Vpc", "cidr_block", "String", true},
		{"Subnet", "id", "Id<aws.Subnet>", true},
		{"Subnet", "vpc_id", "Id<aws.Vpc>", true},
		{"Subnets", "ids", "[Id<aws.Subnet>]", true},
		// No resource type is named aws_region, so its id is a plain string.
		{"Region", "id", "String", true},
		// No Filled entry names token, so the provider may leave it null.
		{"Region", "token", "Secret<String>", false},
	}
	for _, tt := range tests {
		t.Run(tt.ds+"."+tt.attr, func(t *testing.T) {
			ds, ok := p.DataSource(tt.ds)
			if !ok {
				t.Fatalf("%s did not load: %v", tt.ds, p.Err())
			}
			a, ok := ds.Block.Lookup([]string{tt.attr})
			if !ok {
				t.Fatalf("%s has no attribute %s", tt.ds, tt.attr)
			}
			if a.Type.String() != tt.want || a.Filled != tt.filled {
				t.Errorf("%s.%s = %s (filled %v), want %s (filled %v)", tt.ds, tt.attr, a.Type, a.Filled, tt.want, tt.filled)
			}
		})
	}
	if a, ok := vpc.Block.Lookup([]string{"id"}); !ok || !a.Optional {
		t.Errorf("a data source's id is a lookup argument, got %+v, %v", a, ok)
	}
	for _, nb := range vpc.Block.Blocks {
		if want := nb.Name == "timeouts"; nb.IsRecord() != want {
			t.Errorf("block %s is a record: %v, want %v", nb.Name, nb.IsRecord(), want)
		}
	}
}

func TestDataSourceNamesNeedThePrefix(t *testing.T) {
	_, err := NewLazy(Source{Namespace: "hashicorp", Type: "local"}, "2.9.1", Block{}, Types{}, Types{Wires: []string{"tls_certificate"}})
	if err == nil {
		t.Fatal("a data source without the provider prefix must be rejected")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Slot names that end with a suffix in the annotation table refer to its
// families, unless the owner's service has its own type of that name or a
// longer entry stops the match.
func TestSuffixInference(t *testing.T) {
	str := func(name string) Attribute { return Attribute{Name: name, Type: types.TString, Optional: true} }
	computed := func(name string) Attribute { return Attribute{Name: name, Type: types.TString, Computed: true} }
	own := Block{Attributes: []Attribute{computed("id"), computed("arn")}}
	raw := []RawResource{
		{Wire: "aws_subnet", Block: own},
		{Wire: "aws_iam_role", Block: own},
		{Wire: "aws_iam_service_linked_role", Block: own},
		{Wire: "aws_acm_certificate", Block: own},
		{Wire: "aws_dms_certificate", Block: own},
		{Wire: "aws_instance", Block: Block{Blocks: []NestedBlock{{Name: "secondary_network_interface", Nesting: NestList, Block: Block{
			Attributes: []Attribute{str("subnet_id"), str("secondary_subnet_id")},
		}}}}},
		{Wire: "aws_ecs_task_definition", Block: Block{Attributes: []Attribute{
			str("execution_role_arn"), str("iam_role_arn"),
			{Name: "role_arns", Type: types.ListOf(types.TString), Optional: true},
		}}},
		{Wire: "aws_dms_endpoint", Block: Block{Attributes: []Attribute{str("certificate_arn")}}},
		{Wire: "aws_api_gateway_domain_name", Block: Block{Attributes: []Attribute{str("regional_certificate_arn")}}},
	}
	p, err := NewProvider(Source{Namespace: "hashicorp", Type: "aws"}, "6.70.0", Block{}, raw)
	if err != nil {
		t.Fatal(err)
	}
	role := "Arn<aws.IamRole | aws.IamServiceLinkedRole>"
	for _, tt := range []struct{ wire, path, want string }{
		{"aws_iam_role", "arn", "Arn<aws.IamRole>"},
		{"aws_ecs_task_definition", "execution_role_arn", role},
		{"aws_ecs_task_definition", "role_arns", "[" + role + "]"},
		// aws_iam_role is the type in the slot's name, and the table widens
		// it to every role.
		{"aws_ecs_task_definition", "iam_role_arn", role},
		{"aws_api_gateway_domain_name", "regional_certificate_arn", "Arn<aws.AcmCertificate>"},
		{"aws_dms_endpoint", "certificate_arn", "String"},
		{"aws_instance", "secondary_network_interface.subnet_id", "Id<aws.Subnet>"},
		{"aws_instance", "secondary_network_interface.secondary_subnet_id", "String"},
	} {
		t.Run(tt.wire+"."+tt.path, func(t *testing.T) {
			r, err := p.ResourceByWire(tt.wire)
			if err != nil {
				t.Fatal(err)
			}
			a, ok := r.Block.Lookup(strings.Split(tt.path, "."))
			if !ok {
				t.Fatalf("%s has no attribute %s", tt.wire, tt.path)
			}
			if got := a.Type.String(); got != tt.want {
				t.Errorf("type = %s, want %s", got, tt.want)
			}
		})
	}
}
