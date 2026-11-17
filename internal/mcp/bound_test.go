package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBound(t *testing.T) {
	bigDiag := func(i int) string {
		return fmt.Sprintf(`{"severity":"error","class":"type","message":"%d %s"}`, i, strings.Repeat("m", 9000))
	}
	var big []string
	for i := range 20 {
		big = append(big, bigDiag(i))
	}
	nested := `{"format":"writ.remote-plan/v0","ok":false,"document":{"format":"writ.plan.json/v2","diagnostics":[` + strings.Repeat(`{"message":"d"},`, MaxDiagnostics+2) + `{"message":"d"}]},"diagnostics":[]}`
	inner := `{"diagnostics":[` + strings.Repeat(`{},`, MaxDiagnostics) + `{}]}`
	nestedDiags := `{"diagnostics":[` + strings.Repeat(inner+",", MaxDiagnostics) + inner + `]}`
	var blocks strings.Builder
	blocks.WriteString("--- main.writ\n+++ main.writ\n")
	for i := range MaxDiagnostics + 3 {
		fmt.Fprintf(&blocks, "error[syntax] main.writ:%d:1\n  unexpected character\n  fix: delete it\n", i+1)
	}
	blocks.WriteString("formatted: 1 file\n")
	tests := []struct {
		name  string
		in    string
		check func(t *testing.T, out string)
	}{
		{
			name: "a document too long after the cap keeps its head and the diagnostics that fit",
			in:   `{"format":"writ.check/v0","ok":false,"resources":0,"diagnostics":[` + strings.Join(big, ",") + `],"goals":[]}`,
			check: func(t *testing.T, out string) {
				var doc struct {
					Format      string            `json:"format"`
					OK          *bool             `json:"ok"`
					Truncated   bool              `json:"truncated"`
					Message     string            `json:"message"`
					Diagnostics []json.RawMessage `json:"diagnostics"`
					Omitted     int               `json:"omitted_diagnostics"`
				}
				if err := json.Unmarshal([]byte(out), &doc); err != nil {
					t.Fatalf("%v: %.200s", err, out)
				}
				if len(out) > MaxOutput || doc.Format != "writ.check/v0" || doc.OK == nil || *doc.OK || !doc.Truncated || len(doc.Diagnostics) == 0 || len(doc.Diagnostics)+doc.Omitted != 20 || !strings.Contains(doc.Message, "narrow the request") {
					t.Fatalf("%d bytes: %.300s", len(out), out)
				}
			},
		},
		{
			name: "diagnostics nested in a document are capped, in key order",
			in:   nested,
			check: func(t *testing.T, out string) {
				want := `{"format":"writ.remote-plan/v0","ok":false,"document":{"format":"writ.plan.json/v2","diagnostics":[` + strings.TrimSuffix(strings.Repeat(`{"message":"d"},`, MaxDiagnostics), ",") + `],"omitted_diagnostics":3},"diagnostics":[]}`
				if out != want {
					t.Fatalf("got\n%s\nwant\n%s", out, want)
				}
			},
		},
		{
			name: "diagnostics nested in kept diagnostics are capped too",
			in:   nestedDiags,
			check: func(t *testing.T, out string) {
				var doc struct {
					Diagnostics []struct {
						Diagnostics []json.RawMessage `json:"diagnostics"`
						Omitted     int               `json:"omitted_diagnostics"`
					} `json:"diagnostics"`
					Omitted int `json:"omitted_diagnostics"`
				}
				if err := json.Unmarshal([]byte(out), &doc); err != nil {
					t.Fatalf("%v: %.200s", err, out)
				}
				if len(doc.Diagnostics) != MaxDiagnostics || doc.Omitted != 1 {
					t.Fatalf("outer %d, omitted %d", len(doc.Diagnostics), doc.Omitted)
				}
				for i, d := range doc.Diagnostics {
					if len(d.Diagnostics) != MaxDiagnostics || d.Omitted != 1 {
						t.Fatalf("diagnostic %d holds %d, omitted %d", i, len(d.Diagnostics), d.Omitted)
					}
				}
			},
		},
		{
			name: "a truncated document drops a format or ok it cannot bound",
			in:   `{"format":"` + strings.Repeat("x", 100000) + `","ok":"` + strings.Repeat("y", 100000) + `","diagnostics":[]}`,
			check: func(t *testing.T, out string) {
				var doc map[string]json.RawMessage
				if err := json.Unmarshal([]byte(out), &doc); err != nil {
					t.Fatalf("%v: %.200s", err, out)
				}
				_, format := doc["format"]
				_, ok := doc["ok"]
				if len(out) > 1024 || format || ok || string(doc["truncated"]) != "true" {
					t.Fatalf("%d bytes: %.300s", len(out), out)
				}
			},
		},
		{
			name: "a truncated document keeps a format of the known shape",
			in:   `{"format":"writ.error/v0","ok":false,"message":"` + strings.Repeat("x", 100000) + `","diagnostics":[]}`,
			check: func(t *testing.T, out string) {
				if !strings.HasPrefix(out, `{"format":"writ.error/v0","ok":false,"truncated":true,`) || len(out) > 1024 {
					t.Fatalf("%d bytes: %.300s", len(out), out)
				}
			},
		},
		{
			name: "a truncated document keeps the policy results that fit, and counts the rest",
			in: `{"format":"writ.remote-run/v0","ok":true,"document":"` + strings.Repeat("x", MaxOutput) + `","policy":{"format":"writ.policy-decision/v1","point":"apply","outcome":"warn","results":[` +
				strings.TrimSuffix(strings.Repeat(`{"outcome":"warn","message":"`+strings.Repeat("w", 1000)+`"},`, 100), ",") + `]},"diagnostics":[]}`,
			check: func(t *testing.T, out string) {
				var doc struct {
					Policy struct {
						Outcome string            `json:"outcome"`
						Results []json.RawMessage `json:"results"`
						Omitted int               `json:"omitted_results"`
					} `json:"policy"`
				}
				if err := json.Unmarshal([]byte(out), &doc); err != nil {
					t.Fatalf("%v: %.200s", err, out)
				}
				p := doc.Policy
				if len(out) > MaxOutput || p.Outcome != "warn" || len(p.Results) == 0 || len(p.Results)+p.Omitted != 100 || p.Omitted == 0 {
					t.Fatalf("%d bytes, %d results, %d omitted", len(out), len(p.Results), p.Omitted)
				}
			},
		},
		{
			name: "a truncated document drops a policy that is not a decision",
			in:   `{"format":"writ.remote-plan/v0","ok":true,"document":"` + strings.Repeat("x", MaxOutput) + `","policy":{"format":"writ.policy-decision/v1","outcome":"` + strings.Repeat("d", 100) + `"},"diagnostics":[]}`,
			check: func(t *testing.T, out string) {
				if strings.Contains(out, `"policy"`) || len(out) > 1024 {
					t.Fatalf("%d bytes: %.300s", len(out), out)
				}
			},
		},
		{
			name: "a type keeps its angle brackets",
			in:   `{"format":"writ.check/v0","diagnostics":[],"message":"Secret<String> & more"}`,
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, `"Secret<String> & more"`) {
					t.Fatal(out)
				}
			},
		},
		{
			name: "text keeps its first diagnostics and every other line",
			in:   blocks.String(),
			check: func(t *testing.T, out string) {
				if n := strings.Count(out, "error[syntax]"); n != MaxDiagnostics || strings.Count(out, "  fix: delete it\n") != MaxDiagnostics || !strings.HasPrefix(out, "--- main.writ\n+++ main.writ\n") || !strings.HasSuffix(out, "formatted: 1 file\n[writ mcp left out 3 diagnostics after the first 50; fix these, then run the tool again]\n") {
					t.Fatalf("%d diagnostics:\n%s", n, out)
				}
			},
		},
		{
			name: "a line longer than the bound is cut at a rune",
			in:   strings.Repeat("é", MaxOutput),
			check: func(t *testing.T, out string) {
				if len(out) > MaxOutput || !utf8.ValidString(out) || !strings.Contains(out, "[writ mcp cut this result at ") {
					t.Fatalf("%d bytes, valid %v", len(out), utf8.ValidString(out))
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.check(t, Bound(tt.in)) })
	}
}

// TestBoundPolicyKeepsDeniesFirst requires that a policy decision cut to
// fit keeps every deny before any warning, whatever the config order.
func TestBoundPolicyKeepsDeniesFirst(t *testing.T) {
	var results []string
	for i := range 40 {
		results = append(results, fmt.Sprintf(`{"set":"a","level":"advisory","rule":"warn","outcome":"warn","message":"warning %d %s"}`, i, strings.Repeat("w", 900)))
	}
	results = append(results, `{"set":"b","level":"mandatory","rule":"deny","outcome":"deny","message":"the only deny"}`)
	raw := json.RawMessage(`{"format":"writ.policy-decision/v1","point":"plan","outcome":"deny","results":[` + strings.Join(results, ",") + `]}`)
	out, ok := boundPolicy(raw, 32<<10)
	if !ok {
		t.Fatal("boundPolicy refused the decision")
	}
	var got struct {
		Results []struct {
			Outcome, Message string
		}
		Omitted int `json:"omitted_results"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) == 0 || got.Results[0].Outcome != "deny" || got.Results[0].Message != "the only deny" {
		t.Fatalf("the first kept result is not the deny: %+v", got.Results[:min(len(got.Results), 1)])
	}
	if got.Omitted == 0 || len(got.Results)+got.Omitted != 41 {
		t.Fatalf("kept %d and omitted %d of 41", len(got.Results), got.Omitted)
	}
}
