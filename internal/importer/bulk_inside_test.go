package importer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/value"
)

// TestBulkRefusesAValueInsideASecret imports objects whose import id, an
// attribute that is not sensitive, a map key, or a literal of the code is
// part of a secret, such as the password inside a secret JSON document.
// Each is refused, and no problem repeats the value.
func TestBulkRefusesAValueInsideASecret(t *testing.T) {
	ss := schemas(t)
	const inner = "u20-json-inner-secret"
	const doc = `{"password":"` + inner + `"}`
	keyed := stringObj(t, ss, "keyed-public")
	keyed.State = withAttr(keyed.State, "keepers", value.ObjectVal(map[string]value.Value{inner: value.StringVal("x")}))
	r, src := resourceOf(t, ss, sensitive)
	// The file's name is part of its own secret content. State may repeat
	// an object's own secret, but its code writes the name as a literal.
	file := importer.Found{Schema: r, Source: src, State: value.ObjectVal(map[string]value.Value{
		"filename": value.StringVal("/srv/" + inner), "content": value.StringVal(`{"path":"/srv/` + inner + `"}`).Secret(),
		"file_permission": value.StringVal("0700"), "directory_permission": value.StringVal("0700"),
		"id": value.StringVal("e80d3b319a31ae9640c8740fa9a837d876a0eb0b"),
	})}
	reader := &fakeReader{objects: map[string]importer.Found{
		"random.String " + inner:     stringObj(t, ss, inner),
		"random.Integer 5":           integerObj(t, ss, 5, inner),
		"random.String keyed-public": keyed,
		"local.SensitiveFile e80d3b319a31ae9640c8740fa9a837d876a0eb0b": file,
	}}
	docSecret := []value.Value{value.StringVal(doc).Secret()}
	for _, tc := range []struct {
		name    string
		opts    importer.BulkOptions
		refused string
	}{
		{name: "an id", refused: "objects[0].ids[0]", opts: importer.BulkOptions{
			Selections: []importer.Selection{{Type: stringType, IDs: []string{inner}}}, Secrets: docSecret,
		}},
		{name: "an attribute", refused: "objects[0].ids[0]", opts: importer.BulkOptions{
			Selections: []importer.Selection{{Type: integerType, IDs: []string{"5"}}}, Secrets: docSecret,
		}},
		{name: "a map key", refused: "objects[0].ids[0]", opts: importer.BulkOptions{
			Selections: []importer.Selection{{Type: stringType, IDs: []string{"keyed-public"}}}, Secrets: docSecret,
		}},
		{name: "a literal of the code", refused: "objects[0].ids[0]", opts: importer.BulkOptions{
			Selections: []importer.Selection{{Type: sensitive, IDs: []string{"e80d3b319a31ae9640c8740fa9a837d876a0eb0b"}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Reader, tc.opts.File = reader, "imported.tf"
			res, err := importer.Bulk(context.Background(), tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Failed() || len(res.Code) != 0 || len(res.Objects) != 0 {
				t.Fatalf("problems %+v, objects %+v, code:\n%s\nwant the run refused", res.Problems, res.Objects, res.Code)
			}
			refused := false
			for _, p := range res.Problems {
				if strings.Contains(p.Reason, inner) || strings.Contains(p.From, inner) {
					t.Errorf("problem %+v repeats the secret", p)
				}
				refused = refused || p.Kind == importer.NotImported && p.From == tc.refused && strings.Contains(p.Reason, "is part of a secret")
			}
			if !refused {
				t.Errorf("problems %+v, want %s refused", res.Problems, tc.refused)
			}
		})
	}
}

// TestBulkSecretLengthsAreBytes imports a string beside a Secret param at
// the edge of each length the check uses. The lengths count bytes, so four
// two-byte characters reach the 8 that containment needs and three do not.
// docs/import-bulk.md says so.
func TestBulkSecretLengthsAreBytes(t *testing.T) {
	ss := schemas(t)
	for _, tc := range []struct {
		name, secret, id string
		refused          bool
	}{
		{name: "an id holds an 8-byte secret", secret: "abcdefgh", id: "pre-abcdefgh-post", refused: true},
		{name: "an id holds a 7-byte secret", secret: "abcdefg", id: "pre-abcdefg-post"},
		{name: "an id holds 4 characters in 8 bytes", secret: "éééé", id: "pre-éééé-post", refused: true},
		{name: "an id holds 3 characters in 6 bytes", secret: "ééé", id: "pre-ééé-post"},
		{name: "an 8-byte id is in a secret", secret: "pre-abcdefgh-post", id: "abcdefgh", refused: true},
		{name: "a 7-byte id is in a secret", secret: "pre-abcdefg-post", id: "abcdefg"},
		{name: "an id of 4 characters in 8 bytes is in a secret", secret: "pre-éééé-post", id: "éééé", refused: true},
		{name: "an id of 3 characters and 7 bytes is in a secret", secret: "pre-éééa-post", id: "éééa"},
		{name: "an id equals a 4-byte secret of 2 characters", secret: "éé", id: "éé", refused: true},
		{name: "an id equals a 3-byte secret", secret: "abc", id: "abc"},
		{name: "an id is in a secret in another case", secret: "PRE-ABCDEFGH-POST", id: "abcdefgh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := importer.Bulk(context.Background(), importer.BulkOptions{
				Selections: []importer.Selection{{Type: stringType, IDs: []string{tc.id}}},
				Reader:     &fakeReader{objects: map[string]importer.Found{"random.String " + tc.id: stringObj(t, ss, tc.id)}},
				Secrets:    []value.Value{value.StringVal(tc.secret).Secret()},
				File:       "imported.tf",
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Failed() != tc.refused {
				t.Fatalf("refused = %v, want %v; problems %+v", res.Failed(), tc.refused, res.Problems)
			}
		})
	}
}
