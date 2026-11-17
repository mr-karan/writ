package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/value"
)

type settlingStandIn struct {
	protocol
	calls, failAt int
}

func (s *settlingStandIn) importState(_ context.Context, typ, _ string) (importResult, error) {
	return importResult{objects: []importedObject{{typ: typ, state: dynamic{json: []byte(`{"id":"imported"}`)}, private: []byte("imported")}}}, nil
}
func (s *settlingStandIn) read(_ context.Context, req readCall) (stateResult, error) {
	s.calls++
	if s.calls == s.failAt {
		return stateResult{diags: []wireDiag{{error: true, summary: "read failed"}}}, nil
	}
	return stateResult{state: dynamic{json: []byte(`{"id":"read"}`)}, private: []byte("read")}, nil
}

func TestImportSettlingRead(t *testing.T) {
	for _, wire := range []string{"aws_lb_listener_rule", "aws_lb_listener", "aws_alb_listener"} {
		for _, version := range []string{"6.70.0", "6.71.0"} {
			t.Run(wire+"/"+version, func(t *testing.T) {
				_, ws, err := buildSchema(awsSrc, version, awsFixture(t))
				if err != nil {
					t.Fatal(err)
				}
				stand := &settlingStandIn{}
				client := &Client{src: awsSrc, wire: ws, proto: stand}
				got, err := client.Import(context.Background(), ImportRequest{Type: wire, ID: "example"})
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if version == "6.70.0" && strings.HasSuffix(wire, "_rule") {
					want = 2
				}
				if stand.calls != want || got.State.Get("id").AsString() != "read" {
					t.Fatalf("reads %d, state %v", stand.calls, got.State)
				}
				if want == 2 {
					stand.calls, stand.failAt = 0, 2
					got, err = client.Import(context.Background(), ImportRequest{Type: wire, ID: "example"})
					if err == nil || got.State.Kind() != value.Object || got.State.Get("id").AsString() != "read" || string(got.Private) != "read" {
						t.Fatalf("failed second read lost first state: %+v, %v", got, err)
					}
				}
			})
		}
	}
}
