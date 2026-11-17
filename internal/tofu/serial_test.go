package tofu

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestPlanSerial(t *testing.T) {
	plan := func(entries map[string]string) []byte {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		for name, text := range entries {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			w.Write([]byte(text))
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	for _, tc := range []struct {
		name string
		plan []byte
		want Serial
		err  string
	}{
		{"the state the plan read", plan(map[string]string{"tfplan": "x", "tfstate": `{"version":4,"serial":7,"lineage":"0b1e"}`, "tfstate-prev": `{"serial":9,"lineage":"other"}`}), Serial{"0b1e", 7}, ""},
		{"no state", plan(map[string]string{"tfstate": `{"version":4,"serial":0,"lineage":""}`}), Serial{}, ""},
		{"a serial with no lineage", plan(map[string]string{"tfstate": `{"serial":3}`}), Serial{}, "no lineage and serial"},
		{"an encrypted state", plan(map[string]string{"tfstate": `{"encrypted_data":"AAAA"}`}), Serial{}, "no lineage and serial"},
		{"no state entry", plan(map[string]string{"tfplan": "x"}), Serial{}, "holds no state"},
		{"an encrypted saved plan", []byte(`{"encrypted_data":"AAAA"}`), Serial{}, "encrypted saved plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanSerial(tc.plan)
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) || got != tc.want {
				t.Fatalf("PlanSerial: %+v, %v; want %+v, error with %q", got, err, tc.want, tc.err)
			}
		})
	}
}
