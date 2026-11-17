package types_test

import (
	"slices"
	"testing"

	"github.com/mr-karan/writ/internal/types"
)

var (
	privateKey     = types.Name{Provider: "tls", Type: "PrivateKey"}
	certRequest    = types.Name{Provider: "tls", Type: "CertRequest"}
	selfSigned     = types.Name{Provider: "tls", Type: "SelfSignedCert"}
	locallySigned  = types.Name{Provider: "tls", Type: "LocallySignedCert"}
	pemCertRequest = types.RefTo(types.PemRef, certRequest)
	pemSelfSigned  = types.RefTo(types.PemRef, selfSigned)
	pemAnyCert     = types.RefTo(types.PemRef, selfSigned, locallySigned)
)

func req(name string, t types.Type) types.Field { return types.Field{Name: name, Type: t} }
func opt(name string, t types.Type) types.Field {
	return types.Field{Name: name, Type: t, Optional: true}
}

func TestString(t *testing.T) {
	tests := []struct {
		typ  types.Type
		want string
	}{
		{types.TString, "String"},
		{types.TInt, "Int"},
		{types.TFloat, "Float"},
		{types.TBool, "Bool"},
		{types.TNull, "null"},
		{types.TInvalid, "invalid"},
		{types.TNever, "Never"},
		{types.ListOf(types.TNever), "[Never]"},
		{types.Nullable(types.TNever), "null"},
		{types.Nullable(types.Secret(types.TNever)), "Secret<null>"},
		{pemCertRequest, "Pem<tls.CertRequest>"},
		{types.RefTo(types.IdRef, types.Name{Provider: "random", Type: "Pet"}), "Id<random.Pet>"},
		{types.RefTo(types.PemRef, selfSigned, locallySigned, selfSigned), "Pem<tls.LocallySignedCert | tls.SelfSignedCert>"},
		{types.RefTo(types.ArnRef, types.Name{Provider: "aws", Type: "LbTargetGroup"}), "Arn<aws.LbTargetGroup>"},
		{types.ListOf(types.RefTo(types.NameRef, types.Name{Provider: "aws", Type: "IamRole"})), "[Name<aws.IamRole>]"},
		{types.Secret(types.TString), "Secret<String>"},
		{types.Secret(types.RefTo(types.PemRef, privateKey)), "Secret<Pem<tls.PrivateKey>>"},
		{types.ListOf(types.TString), "[String]"},
		{types.MapOf(types.TInt), "{String: Int}"},
		{types.MapOf(types.Secret(types.TInt)), "{String: Secret<Int>}"},
		{types.EnumOf("b", "a", "b"), `"a" | "b"`},
		{types.EnumOf("ECDSA"), `"ECDSA"`},
		{types.EnumOf("${x}", `say "hi"`), `"\${x}" | "say \"hi\""`},
		{types.Nullable(types.TString), "String?"},
		{types.Nullable(types.EnumOf("a", "b")), `"a" | "b"?`},
		{types.Secret(types.Nullable(types.TString)), "Secret<String?>"},
		{types.Nullable(types.Secret(types.TString)), "Secret<String?>"},
		{types.RecordOf(opt("b", types.EnumOf("y", "x")), req("a", types.TInt)), `{ a: Int, b?: "x" | "y" }`},
		{types.RecordOf(), "{}"},
		{types.ResourceOf(privateKey, req("algorithm", types.TString)), "tls.PrivateKey"},
		{types.ListOf(types.Nullable(types.ListOf(types.TInt))), "[[Int]?]"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.typ.String(); got != tt.want {
				t.Fatalf("String() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestAssign(t *testing.T) {
	subject := func(cn types.Type) types.Type {
		return types.RecordOf(req("subject", types.RecordOf(req("common_name", cn))))
	}
	tests := []struct {
		name     string
		dst, src types.Type
		// wantMsg is the mismatch rendered with Message(""); empty means allowed.
		wantMsg    string
		secretOnly bool
	}{
		{name: "same scalar", dst: types.TString, src: types.TString},
		{name: "int widens to float", dst: types.TFloat, src: types.TInt},
		{name: "float does not narrow to int", dst: types.TInt, src: types.TFloat, wantMsg: "expects Int, got Float"},
		{name: "bool is not a string", dst: types.TString, src: types.TBool, wantMsg: "expects String, got Bool"},

		{name: "literal into its union", dst: types.EnumOf("RSA", "ECDSA"), src: types.EnumOf("ECDSA")},
		{name: "narrower union into wider", dst: types.EnumOf("a", "b", "c"), src: types.EnumOf("a", "c")},
		{name: "literal outside the union", dst: types.EnumOf("RSA", "ECDSA"), src: types.EnumOf("DSA"),
			wantMsg: `expects "ECDSA" | "RSA", got "DSA"`},
		{name: "wider union into narrower", dst: types.EnumOf("a"), src: types.EnumOf("a", "b"),
			wantMsg: `expects "a", got "a" | "b"`},
		{name: "enum into string", dst: types.TString, src: types.EnumOf("a", "b")},
		{name: "string into enum", dst: types.EnumOf("a", "b"), src: types.TString,
			wantMsg: `expects "a" | "b", got String`},

		{name: "string literal into reference", dst: pemCertRequest, src: types.EnumOf("-----BEGIN CERTIFICATE REQUEST-----")},
		{name: "plain string into reference", dst: pemCertRequest, src: types.TString},
		{name: "reference into string", dst: types.TString, src: pemSelfSigned},
		{name: "same reference", dst: pemCertRequest, src: pemCertRequest},
		{name: "different reference", dst: pemCertRequest, src: pemSelfSigned,
			wantMsg: "expects Pem<tls.CertRequest>, got Pem<tls.SelfSignedCert>"},
		{name: "id is not pem", dst: types.RefTo(types.PemRef, privateKey), src: types.RefTo(types.IdRef, privateKey),
			wantMsg: "expects Pem<tls.PrivateKey>, got Id<tls.PrivateKey>"},
		{name: "arn is not id", dst: types.RefTo(types.ArnRef, privateKey), src: types.RefTo(types.IdRef, privateKey),
			wantMsg: "expects Arn<tls.PrivateKey>, got Id<tls.PrivateKey>"},
		{name: "name is not arn", dst: types.RefTo(types.NameRef, privateKey), src: types.RefTo(types.ArnRef, privateKey),
			wantMsg: "expects Name<tls.PrivateKey>, got Arn<tls.PrivateKey>"},
		{name: "plain string into name", dst: types.RefTo(types.NameRef, privateKey), src: types.TString},
		{name: "arn into string", dst: types.TString, src: types.RefTo(types.ArnRef, privateKey)},
		{name: "reference into union of targets", dst: pemAnyCert, src: pemSelfSigned},
		{name: "union of targets into one target", dst: pemSelfSigned, src: pemAnyCert,
			wantMsg: "expects Pem<tls.SelfSignedCert>, got Pem<tls.LocallySignedCert | tls.SelfSignedCert>"},
		{name: "reference is not an enum", dst: types.EnumOf("a"), src: pemSelfSigned,
			wantMsg: `expects "a", got Pem<tls.SelfSignedCert>`},

		{name: "null into nullable", dst: types.Nullable(types.TString), src: types.TNull},
		{name: "null into non-nullable", dst: types.TString, src: types.TNull, wantMsg: "expects String, got null"},
		{name: "value into nullable", dst: types.Nullable(types.TFloat), src: types.TInt},
		{name: "nullable into non-nullable", dst: types.TString, src: types.Nullable(types.TString),
			wantMsg: "expects String, got String?"},
		{name: "nullable into nullable", dst: types.Nullable(types.TString), src: types.Nullable(types.EnumOf("a"))},

		{name: "secret into plain slot", dst: types.TString, src: types.Secret(types.TString),
			wantMsg: "expects String, got Secret<String>", secretOnly: true},
		{name: "secret reference into plain reference", dst: types.RefTo(types.PemRef, privateKey),
			src:     types.Secret(types.RefTo(types.PemRef, privateKey)),
			wantMsg: "expects Pem<tls.PrivateKey>, got Secret<Pem<tls.PrivateKey>>", secretOnly: true},
		{name: "secret of the wrong shape is a type error", dst: types.TString, src: types.Secret(types.TInt),
			wantMsg: "expects String, got Secret<Int>"},
		{name: "secret into secret slot", dst: types.Secret(types.TString), src: types.Secret(types.EnumOf("x"))},
		{name: "plain into secret slot", dst: types.Secret(types.TString), src: types.TString},
		{name: "secret element into plain list", dst: types.ListOf(types.TString), src: types.ListOf(types.Secret(types.TString)),
			wantMsg: "[*] expects String, got Secret<String>", secretOnly: true},
		{name: "secret slot covers its elements", dst: types.Secret(types.ListOf(types.TString)), src: types.ListOf(types.Secret(types.TString))},
		{name: "secret list into list of secrets", dst: types.ListOf(types.Secret(types.TString)), src: types.Secret(types.ListOf(types.TString)),
			wantMsg: "expects [Secret<String>], got Secret<[String]>", secretOnly: true},
		{name: "secret record field", dst: types.RecordOf(req("password", types.TString)),
			src:     types.RecordOf(req("password", types.Secret(types.TString))),
			wantMsg: "password expects String, got Secret<String>", secretOnly: true},
		{name: "secret record field into map", dst: types.MapOf(types.TString),
			src:     types.RecordOf(req("password", types.Secret(types.TString))),
			wantMsg: "password expects String, got Secret<String>", secretOnly: true},

		{name: "list is covariant", dst: types.ListOf(types.TFloat), src: types.ListOf(types.TInt)},
		{name: "list element mismatch", dst: types.ListOf(types.TString), src: types.ListOf(types.TInt),
			wantMsg: "[*] expects String, got Int"},
		{name: "map is covariant", dst: types.MapOf(types.TString), src: types.MapOf(types.EnumOf("a"))},
		{name: "record into map", dst: types.MapOf(types.TString), src: types.RecordOf(req("a", types.EnumOf("x")), req("b", types.TString))},
		{name: "optional field into map of nullable", dst: types.MapOf(types.Nullable(types.TString)), src: types.RecordOf(req("a", types.EnumOf("x")), opt("b", types.TString))},
		{name: "optional field into map", dst: types.MapOf(types.TString), src: types.RecordOf(req("a", types.EnumOf("x")), opt("b", types.TString)),
			wantMsg: "b expects String, got String?"},
		{name: "record field does not fit map", dst: types.MapOf(types.TString), src: types.RecordOf(req("n", types.TInt)),
			wantMsg: "n expects String, got Int"},
		{name: "map is not a record", dst: types.RecordOf(opt("a", types.TString)), src: types.MapOf(types.TString),
			wantMsg: "expects { a?: String }, got {String: String}"},
		{name: "list is not a map", dst: types.MapOf(types.TString), src: types.ListOf(types.TString),
			wantMsg: "expects {String: String}, got [String]"},

		{name: "record with optional field omitted", dst: types.RecordOf(req("a", types.TString), opt("b", types.TInt)),
			src: types.RecordOf(req("a", types.EnumOf("x")))},
		{name: "record missing required field", dst: types.RecordOf(req("a", types.TString), req("b", types.TInt)),
			src: types.RecordOf(req("a", types.TString)), wantMsg: "b is required"},
		{name: "record with extra field", dst: types.RecordOf(req("a", types.TString)),
			src: types.RecordOf(req("a", types.TString), req("c", types.TInt)), wantMsg: "c is not an expected field"},
		{name: "null into optional field", dst: types.RecordOf(opt("b", types.TInt)), src: types.RecordOf(req("b", types.TNull))},
		{name: "nullable into optional field", dst: types.RecordOf(opt("b", types.TInt)), src: types.RecordOf(req("b", types.Nullable(types.TInt)))},
		{name: "null into required field", dst: types.RecordOf(req("b", types.TInt)), src: types.RecordOf(req("b", types.TNull)),
			wantMsg: "b expects Int, got null"},
		{name: "optional source field into required field", dst: types.RecordOf(req("b", types.TInt)), src: types.RecordOf(opt("b", types.TInt)),
			wantMsg: "b expects Int, got Int?"},
		{name: "read-only field is not required", dst: types.RecordOf(req("a", types.TString), types.Field{Name: "id", Type: types.TString, ReadOnly: true}),
			src: types.RecordOf(req("a", types.TString))},
		{name: "nested field mismatch names its path", dst: subject(types.TString), src: subject(types.TInt),
			wantMsg: "subject.common_name expects String, got Int"},
		{name: "mismatch inside a list of records", dst: types.ListOf(types.RecordOf(req("n", types.TString))),
			src: types.ListOf(types.RecordOf(req("n", types.TBool))), wantMsg: "[*].n expects String, got Bool"},

		{name: "same resource type", dst: types.ResourceOf(privateKey), src: types.ResourceOf(privateKey, req("algorithm", types.TString))},
		{name: "different resource type", dst: types.ResourceOf(privateKey), src: types.ResourceOf(certRequest),
			wantMsg: "expects tls.PrivateKey, got tls.CertRequest"},
		{name: "resource is not a record", dst: types.RecordOf(), src: types.ResourceOf(privateKey),
			wantMsg: "expects {}, got tls.PrivateKey"},

		{name: "invalid source", dst: types.TString, src: types.TInvalid},
		{name: "invalid destination", dst: types.TInvalid, src: types.Secret(types.TInt)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := types.Assign(tt.dst, tt.src)
			if tt.wantMsg == "" {
				if !ok {
					t.Fatalf("Assign(%s, %s) rejected: %s", tt.dst, tt.src, m.Message(""))
				}
				if m != (types.Mismatch{}) {
					t.Fatalf("an allowed flow must return an empty Mismatch, got %+v", m)
				}
				return
			}
			if ok {
				t.Fatalf("Assign(%s, %s) allowed, want %q", tt.dst, tt.src, tt.wantMsg)
			}
			if got := m.Message(""); got != tt.wantMsg {
				t.Fatalf("Message = %q, want %q", got, tt.wantMsg)
			}
			if m.SecretOnly != tt.secretOnly {
				t.Fatalf("SecretOnly = %v, want %v", m.SecretOnly, tt.secretOnly)
			}
		})
	}
}

func TestMismatchMessageJoinsSlotAndPath(t *testing.T) {
	tests := []struct {
		m    types.Mismatch
		slot string
		want string
	}{
		{types.Mismatch{Reason: "expects String, got Int"}, "cert_pem", "cert_pem expects String, got Int"},
		{types.Mismatch{Reason: "expects String, got Int", Path: "subject.common_name"}, "", "subject.common_name expects String, got Int"},
		{types.Mismatch{Reason: "is required", Path: "common_name"}, "subject", "subject.common_name is required"},
		{types.Mismatch{Reason: "expects String, got Int", Path: "[*].n"}, "tags", "tags[*].n expects String, got Int"},
		{types.Mismatch{Reason: "expects String, got Int"}, "", "expects String, got Int"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.m.Message(tt.slot); got != tt.want {
				t.Fatalf("Message(%q) = %q, want %q", tt.slot, got, tt.want)
			}
		})
	}
}

func TestFieldValueType(t *testing.T) {
	tests := []struct {
		name string
		f    types.Field
		want string
	}{
		{"required", req("a", types.TInt), "Int"},
		{"optional", opt("a", types.TInt), "Int?"},
		{"read-only", types.Field{Name: "a", Type: types.TInt, ReadOnly: true}, "Int?"},
		{"required and nullable", req("a", types.Nullable(types.TInt)), "Int?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.ValueType().String(); got != tt.want {
				t.Fatalf("ValueType() = %s, want %s", got, tt.want)
			}
		})
	}
}

// Never is the element type of an empty collection. It has no values, so
// it flows into every type and joins to the other side. Never? is null.
func TestNever(t *testing.T) {
	never, nullableNever := types.TNever, types.Nullable(types.TNever)
	emptyField := types.RecordOf(opt("a", never))
	t.Run("Assign", func(t *testing.T) {
		tests := []struct {
			name     string
			dst, src types.Type
			ok       bool
		}{
			{"into a scalar", types.TInt, never, true},
			{"into a nullable scalar", types.Nullable(types.TString), never, true},
			{"into a record", types.RecordOf(req("a", types.TInt)), never, true},
			{"into a resource", types.ResourceOf(privateKey), never, true},
			{"an empty list into a list", types.ListOf(types.TInt), types.ListOf(never), true},
			{"an empty map into a map", types.MapOf(types.TString), types.MapOf(never), true},
			{"a secret into a secret", types.Secret(types.TInt), types.Secret(never), true},
			{"into Never", never, never, true},
			{"Never? into a nullable type", types.Nullable(types.TInt), nullableNever, true},
			{"Never? into a non-null type", types.TInt, nullableNever, false},
			{"Never? into a non-null list element", types.ListOf(types.TInt), types.ListOf(nullableNever), false},
			{"a field of an empty element as a map value", types.MapOf(types.TInt), emptyField, false},
			{"a field of an empty element as a nullable map value", types.MapOf(types.Nullable(types.TInt)), emptyField, true},
			{"a value into Never", never, types.TInt, false},
			{"null into Never", never, types.TNull, false},
			{"a list into an empty list", types.ListOf(never), types.ListOf(types.TInt), false},
			{"a secret into a plain slot", types.TInt, types.Secret(never), false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if m, ok := types.Assign(tt.dst, tt.src); ok != tt.ok {
					t.Fatalf("Assign(%s, %s) = %v %q, want %v", tt.dst, tt.src, ok, m.Message(""), tt.ok)
				}
			})
		}
	})
	t.Run("a field of an empty element reads as null", func(t *testing.T) {
		f, _ := emptyField.Field("a")
		if got := f.ValueType(); got.Kind() != types.Null || !got.IsNullable() {
			t.Fatalf("ValueType = %s, want null", got)
		}
		if got := never.Elem(); got.Kind() != types.Never {
			t.Fatalf("Never.Elem() = %s, want Never", got)
		}
	})
}

func TestHasSecret(t *testing.T) {
	keyType := types.ResourceOf(privateKey,
		req("algorithm", types.TString),
		types.Field{Name: "private_key_pem", Type: types.Secret(types.RefTo(types.PemRef, privateKey)), ReadOnly: true},
	)
	tests := []struct {
		name string
		typ  types.Type
		want bool
	}{
		{"plain", types.TString, false},
		{"secret", types.Secret(types.TInt), true},
		{"list of secrets", types.ListOf(types.Secret(types.TString)), true},
		{"map of secrets", types.MapOf(types.Secret(types.TString)), true},
		{"record with a secret field", types.RecordOf(req("a", types.TInt), req("p", types.Secret(types.TString))), true},
		{"record without one", types.RecordOf(req("a", types.TInt)), false},
		{"resource handle is not data", keyType, false},
		{"list of resource handles", types.ListOf(keyType), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.typ.HasSecret(); got != tt.want {
				t.Fatalf("%s.HasSecret() = %v, want %v", tt.typ, got, tt.want)
			}
		})
	}
	f, _ := keyType.Field("private_key_pem")
	if !f.Type.IsSecret() {
		t.Fatal("reading a secret attribute through a resource handle must still be secret")
	}
}

func TestAccessors(t *testing.T) {
	rec := types.RecordOf(
		req("b", types.TInt),
		types.Field{Name: "id", Type: types.TString, ReadOnly: true},
		opt("a", types.TString),
	)
	var names []string
	for _, f := range rec.Fields() {
		names = append(names, f.Name)
	}
	if want := []string{"a", "b", "id"}; !slices.Equal(names, want) {
		t.Fatalf("Fields() = %v, want sorted %v", names, want)
	}
	id, ok := rec.Field("id")
	if !ok || !id.Computed || id.Required() {
		t.Fatalf("Field(id) = %+v, %v; a read-only field is computed and not required", id, ok)
	}
	if a, _ := rec.Field("a"); a.Required() {
		t.Fatal("an optional field is not required")
	}
	if b, _ := rec.Field("b"); !b.Required() {
		t.Fatal("a plain field is required")
	}
	if _, ok := rec.Field("missing"); ok {
		t.Fatal("Field must report a missing field")
	}
	if got := types.ListOf(types.TInt).Elem(); got.Kind() != types.Int {
		t.Fatalf("Elem of [Int] = %s", got)
	}
	if got := types.TString.Elem(); got.Kind() != types.Invalid {
		t.Fatalf("Elem of String = %s, want invalid", got)
	}
	if got := types.RefTo(types.PemRef, privateKey).Resource(); got != privateKey {
		t.Fatalf("Resource of a single-target ref = %s", got)
	}
	if got := pemAnyCert.Resource(); got != (types.Name{}) {
		t.Fatalf("Resource of a multi-target ref = %s, want zero", got)
	}
	if !types.TNull.IsNullable() || !types.Nullable(types.TInt).IsNullable() || types.TInt.IsNullable() {
		t.Fatal("IsNullable must hold for null and T? only")
	}
	if got := types.EnumOf("b", "a", "b").Enum(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Enum() = %v, want sorted and deduplicated", got)
	}
}

func TestConstructorsDoNotAliasInput(t *testing.T) {
	members := []string{"b", "a"}
	e := types.EnumOf(members...)
	members[0] = "z"
	if e.String() != `"a" | "b"` {
		t.Fatalf("EnumOf aliased its input: %s", e)
	}
	fields := []types.Field{req("a", types.TInt)}
	r := types.RecordOf(fields...)
	fields[0].Name = "z"
	if _, ok := r.Field("a"); !ok {
		t.Fatal("RecordOf aliased its input")
	}
}

func TestRecordOfPanicsOnDuplicateField(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a duplicate field must panic")
		}
	}()
	types.RecordOf(req("a", types.TInt), req("a", types.TString))
}

func TestNamed(t *testing.T) {
	group := types.RecordOf(req("name", types.TString), req("min", types.TInt))
	named := types.Named("Group", group)
	other := types.Named("Pool", group)

	printed := []struct {
		typ  types.Type
		want string
	}{
		{named, "Group"},
		{types.Nullable(named), "Group?"},
		{types.Secret(named), "Secret<Group>"},
		{types.ListOf(named), "[Group]"},
		{types.MapOf(named), "{String: Group}"},
		{types.RecordOf(req("g", named)), "{ g: Group }"},
	}
	for _, tt := range printed {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.typ.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("a field read through an alias prints its own type", func(t *testing.T) {
		if f, _ := named.Field("min"); f.Type.String() != "Int" {
			t.Errorf("field min prints %s, want Int", f.Type)
		}
	})
	t.Run("an alias assigns both ways with the type it names", func(t *testing.T) {
		for _, pair := range [][2]types.Type{{named, group}, {group, named}, {named, other}} {
			if m, ok := types.Assign(pair[0], pair[1]); !ok {
				t.Errorf("Assign(%s, %s): %s", pair[0], pair[1], m.Message(""))
			}
		}
	})
	t.Run("a mismatch names the alias", func(t *testing.T) {
		m, ok := types.Assign(named, types.TString)
		if ok || m.Message("g") != "g expects Group, got String" {
			t.Errorf("Assign(Group, String) = %q, %v", m.Message("g"), ok)
		}
	})
}
