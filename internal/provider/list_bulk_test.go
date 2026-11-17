package provider_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// clientLister lists through a provider Client, as the engine does.
type clientLister struct{ c *provider.Client }

func (l clientLister) List(ctx context.Context, _ types.Name, _ addr.Addr, query value.Value, limit int) ([]string, diag.List, error) {
	ids, _, err := l.c.List(ctx, "fake_thing", query, limit)
	return ids, nil, err
}

// noReader fails the test on any read: the limit must stop the run first.
type noReader struct{ t *testing.T }

func (r noReader) Read(context.Context, types.Name, addr.Addr, string) (importer.Found, diag.List, error) {
	r.t.Error("a read ran, want the object limit to stop the run before any read")
	return importer.Found{}, nil, nil
}

func (noReader) Validator(types.Name, addr.Addr) importer.Validator { return nil }

func (noReader) Planner(types.Name, addr.Addr, importer.Found) importer.Planner { return nil }

// A provider that lists A, A, B passes a limit of 1 object, though its
// first two events name one object.
func TestBulkListRepeatOverGRPC(t *testing.T) {
	c := provider.ServeListStandIn(t, []string{"thing-0001", "thing-0001", "thing-0002"})
	_, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{{Type: types.Name{Provider: "fake", Type: "Thing"}, List: provider.ListStandInQuery}},
		Reader:     noReader{t}, Lister: clientLister{c}, MaxObjects: 1, File: "imported.writ",
	})
	var limit *importer.LimitError
	if !errors.As(err, &limit) || limit.What != "objects" {
		t.Fatalf("err = %v, want an object limit", err)
	}
}
