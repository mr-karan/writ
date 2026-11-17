package provider

import (
	"context"
	"testing"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

type defaultsReader struct {
	protocol
	state dynamic
}

func (p defaultsReader) read(context.Context, readCall) (stateResult, error) {
	return stateResult{state: p.state, private: []byte("private")}, nil
}

func TestClientReadFillsDefaults(t *testing.T) {
	p, ws := mustSchema(t, schema.Source{Namespace: "test", Type: "kinds"}, kindsSchema)
	r, err := p.ResourceByWire("kinds_thing")
	if err != nil {
		t.Fatal(err)
	}
	r.ReadDefaults = map[string]string{"b": "false", "s": `"10m"`}
	w, err := ws.resource("kinds_thing")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		in, want value.Value
	}{
		{"null defaults", value.ObjectVal(nil), value.BoolVal(false)},
		{"explicit", value.ObjectVal(map[string]value.Value{"b": value.BoolVal(true)}), value.BoolVal(true)},
		{"gone", value.NullVal(), value.NullVal()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := w.encode(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			c := &Client{wire: ws, proto: defaultsReader{state: dynamic{msgpack: raw}}}
			got, err := c.Read(context.Background(), ReadRequest{Type: "kinds_thing", Current: tc.in})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "gone" {
				if got.New.Kind() != value.Null {
					t.Fatal("gone object filled")
				}
				return
			}
			if !value.Equal(got.New.Get("b"), tc.want) || !value.Equal(got.New.Get("s"), value.StringVal("10m")) || string(got.Private) != "private" {
				t.Fatalf("read = %v", got)
			}
		})
	}
}
