package policy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeOutputs are evaluation outputs for one set: the one valid output,
// the outputs the security review of P1 turned into a pass, and others
// that break the protocol.
var fakeOutputs = map[string]string{
	"valid":             "{\"set\":0,\"decision\":{\"messages\":[{\"rule\":\"deny\",\"message\":\"fake\"}]}}\n{\"done\":1}\n",
	"null":              "null\n{\"done\":1}\n",
	"empty object":      "{}\n{\"done\":1}\n",
	"partial json":      "{\"set\":0}\n{\"set\":0,\"deci",
	"set only":          "{\"set\":0}\n{\"done\":1}\n",
	"duplicate":         "{\"set\":0,\"decision\":{\"messages\":[]}}\n{\"set\":0,\"decision\":{\"messages\":[]}}\n{\"done\":1}\n",
	"unknown field":     "{\"set\":0,\"decision\":{\"messages\":[]},\"pass\":true}\n{\"done\":1}\n",
	"out of range":      "{\"set\":1,\"decision\":{\"messages\":[]}}\n{\"done\":1}\n",
	"missing messages":  "{\"set\":0,\"decision\":{}}\n{\"done\":1}\n",
	"null messages":     "{\"set\":0,\"decision\":{\"messages\":null}}\n{\"done\":1}\n",
	"decision and err":  "{\"set\":0,\"decision\":{\"messages\":[]},\"error\":\"x\"}\n{\"done\":1}\n",
	"no done":           "{\"set\":0,\"decision\":{\"messages\":[]}}\n",
	"wrong count":       "{\"set\":0,\"decision\":{\"messages\":[]}}\n{\"done\":2}\n",
	"trailing bytes":    "{\"set\":0,\"decision\":{\"messages\":[]}}\n{\"done\":1}\n{}",
	"two objects":       "{\"set\":0,\"decision\":{\"messages\":[]}} {}\n{\"done\":1}\n",
	"nothing":           "",
	"key case":          "{\"Set\":0,\"Decision\":{\"Messages\":[]}}\n{\"Done\":1}\n",
	"duplicate key":     "{\"set\":0,\"decision\":{\"messages\":[{\"rule\":\"deny\",\"message\":\"no\"}],\"messages\":[]}}\n{\"done\":1}\n",
	"duplicate set":     "{\"set\":5,\"set\":0,\"decision\":{\"messages\":[]}}\n{\"done\":1}\n",
	"null error":        "{\"set\":0,\"decision\":{\"messages\":[]},\"error\":null}\n{\"done\":1}\n",
	"null done":         "{\"set\":0,\"decision\":{\"messages\":[]}}\n{\"done\":1,\"set\":null}\n",
	"null decision":     "{\"set\":0,\"error\":\"x\",\"decision\":null}\n{\"done\":1}\n",
	"null message":      "{\"set\":0,\"decision\":{\"messages\":[null]}}\n{\"done\":1}\n",
	"null text":         "{\"set\":0,\"decision\":{\"messages\":[{\"rule\":\"deny\",\"message\":null}]}}\n{\"done\":1}\n",
	"no rule":           "{\"set\":0,\"decision\":{\"messages\":[{\"message\":\"x\"}]}}\n{\"done\":1}\n",
	"a rule not read":   "{\"set\":0,\"decision\":{\"messages\":[{\"rule\":\"allow\",\"message\":\"x\"}]}}\n{\"done\":1}\n",
	"message key twice": "{\"set\":0,\"decision\":{\"messages\":[{\"rule\":\"warn\",\"rule\":\"deny\",\"message\":\"x\"}]}}\n{\"done\":1}\n",
	"duplicate done":    "{\"set\":0,\"decision\":{\"messages\":[]}}\n{\"done\":7,\"done\":1}\n",
	"empty error":       "{\"set\":0,\"error\":\"\"}\n{\"done\":1}\n",
}

func TestPolicyReadOutput(t *testing.T) {
	for mode, out := range fakeOutputs {
		t.Run(mode, func(t *testing.T) {
			got, err := readOutput([]byte(out), 1)
			if mode == "valid" {
				if err != nil || len(got) != 1 || len(got[0].messages) != 1 || got[0].messages[0] != (message{"deny", "fake"}) {
					t.Fatalf("the valid output gave %+v, %v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("readOutput accepted %q as %+v", out, got)
			}
		})
	}
}

// TestPolicyEvaluateFails gives evaluate processes that exit with an
// error or write past the output limit. Each set is an error, also one
// whose result arrived.
func TestPolicyEvaluateFails(t *testing.T) {
	valid := "printf '%s' '" + strings.TrimSuffix(fakeOutputs["valid"], "\n") + "'; echo"
	cases := map[string]struct{ script, want string }{
		"exit 1":           {valid + "; exit 1", "the evaluation process failed"},
		"over the limit":   {"i=0; while [ $i -lt 5000 ]; do printf '%1000s' ''; i=$((i+1)); done; " + valid, "more than 4 MiB"},
		"no done":          {"printf '%s\\n' '{\"set\":0,\"decision\":{\"messages\":[]}}'", "broke its protocol"},
		"a missing binary": {"", "start the evaluation process"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := &Engine{command: []string{"/bin/sh", "sh", "-c", c.script}, timeout: time.Minute, memory: DefaultMemory,
				sets: []compiled{{name: "a", level: Mandatory}, {name: "b", level: Advisory}}}
			if c.script == "" {
				e.command = []string{"/nonexistent/writ", childName}
			}
			in, err := NewInput([]byte(`{"format_version":"1.2","resource_changes":[]}`), Writ{Decision: PlanPoint, Now: time.Unix(0, 0), Environment: "e", Stack: "s"})
			if err != nil {
				t.Fatal(err)
			}
			d := e.Decide(context.Background(), in)
			if d.Outcome != Deny || len(d.Results) != 2 || d.Results[0].Rule != "error" || d.Results[1].Outcome != Warn || !strings.Contains(d.Results[0].Message, c.want) {
				t.Fatalf("got %s %+v, want two errors with %q", d.Outcome, d.Results, c.want)
			}
		})
	}
}
