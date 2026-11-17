package importer_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/value"
)

// TestBulkRefusesAShortOwnSecretID imports passwords whose import ids, their
// own results, are shorter than the length a comparison with another
// object's secret needs. An object's own secret is tracked at any length,
// so the password is refused without its id in the problem. A string with
// the same short id beside it is another object, and that comparison keeps
// the short-value exception, so the string is not refused.
func TestBulkRefusesAShortOwnSecretID(t *testing.T) {
	ss := schemas(t)
	for _, id := range []string{"abc", "true", "false"} {
		t.Run(id, func(t *testing.T) {
			r := &fakeReader{objects: map[string]importer.Found{
				"random.Password " + id: passwordObj(t, ss, id),
				"random.String " + id:   stringObj(t, ss, id),
			}}
			res, err := importer.Bulk(context.Background(), importer.BulkOptions{
				Selections: []importer.Selection{{Type: passwordType, IDs: []string{id}}, {Type: stringType, IDs: []string{id}}},
				Reader:     r, File: "imported.tf",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Problems) != 1 || res.Problems[0].From != "objects[0].ids[0]" || !strings.Contains(res.Problems[0].Reason, "OpenTofu refuses a sensitive import id") {
				t.Fatalf("problems %+v; want only the password refused", res.Problems)
			}
			if strings.Contains(res.Problems[0].Reason, id) {
				t.Errorf("the problem names the secret id %q", id)
			}
			for _, o := range res.Objects {
				if o.Type == passwordType {
					t.Errorf("password %s was imported, want it refused", o.Addr)
				}
			}
		})
	}
}

// TestBulkRefusesAContainedSecret imports objects that hold a secret of
// another owner, of eight bytes or more, inside a longer value: an import
// id, an attribute that is not sensitive, and the literal the code would
// write for it. Each is refused, and no problem repeats the secret.
func TestBulkRefusesAContainedSecret(t *testing.T) {
	ss := schemas(t)
	const secret = "u20-substring-secret"
	r := &fakeReader{objects: map[string]importer.Found{
		"random.Password " + secret:               passwordObj(t, ss, secret),
		"random.String prefix-" + secret + "-end": stringObj(t, ss, "prefix-"+secret+"-end"),
		"random.Integer 5":                        integerObj(t, ss, 5, "seed-"+secret),
	}}
	password := importer.Selection{Type: passwordType, IDs: []string{secret}}
	for _, tc := range []struct {
		name    string
		opts    importer.BulkOptions
		refused string
	}{
		{name: "an id holds a password of the run", refused: "objects[1].ids[0]", opts: importer.BulkOptions{Selections: []importer.Selection{
			password, {Type: stringType, IDs: []string{"prefix-" + secret + "-end"}},
		}}},
		{name: "an attribute holds a password of the run", refused: "objects[1].ids[0]", opts: importer.BulkOptions{Selections: []importer.Selection{
			password, {Type: integerType, IDs: []string{"5"}},
		}}},
		{name: "an id holds a Secret param", refused: "objects[0].ids[0]", opts: importer.BulkOptions{
			Selections: []importer.Selection{{Type: stringType, IDs: []string{"prefix-" + secret + "-end"}}},
			Secrets:    []value.Value{value.StringVal(secret).Secret()},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Reader, tc.opts.File = r, "imported.tf"
			res, err := importer.Bulk(context.Background(), tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Failed() || len(res.Code) != 0 {
				t.Fatalf("problems %+v, objects %+v, code:\n%s\nwant the run refused", res.Problems, res.Objects, res.Code)
			}
			if slices.ContainsFunc(res.Objects, func(o importer.BulkObject) bool { return strings.Contains(o.ID, secret) }) {
				t.Errorf("objects %+v hold the secret", res.Objects)
			}
			refused := false
			for _, p := range res.Problems {
				if strings.Contains(p.Reason, secret) || strings.Contains(p.From, secret) {
					t.Errorf("problem %+v repeats the secret", p)
				}
				refused = refused || p.Kind == importer.NotImported && p.From == tc.refused && strings.Contains(p.Reason, "equals, contains, or is part of a secret")
			}
			if !refused {
				t.Errorf("problems %+v, want %s refused", res.Problems, tc.refused)
			}
		})
	}
}
