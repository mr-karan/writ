package control_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
)

// BenchmarkPlansKeepNone pages GET /v1/plans?env=prod for a caller who may
// read none of 10,000 plans, each with a 32 KB document and an 8 KB text,
// so every row is read and dropped.
func BenchmarkPlansKeepNone(b *testing.B) {
	for _, e := range controltest.Engines() {
		b.Run(e.Name, func(b *testing.B) {
			s, loc := controltest.Open(b, e)
			ctx := context.Background()
			cfg, err := s.RecordConfig(ctx, "{}", "u", "test", policy(), t0)
			if err != nil {
				b.Fatal(err)
			}
			use := control.TokenUse{Issuer: "gitlab", JTI: "seed", Op: "plan", Expires: t0.Add(time.Hour)}
			if err := s.CreatePlan(ctx, newPlan("SEED", cfg, "prod", "app", t0), use, control.Entry{At: t0, Request: "seed", Kind: "request", Decision: control.Allowed}); err != nil {
				b.Fatal(err)
			}
			doc, _ := json.Marshal(map[string]string{"format": "writ.plan.json/v2", "pad": strings.Repeat("x", 32<<10)})
			res := control.PlanResult{ID: "seed-id", Status: "pending", MadeAt: t0, ExpiresAt: t0.Add(24 * time.Hour), Changes: 1,
				Document: doc, Text: strings.Repeat("y", 8<<10), Saved: []byte("saved")}
			if err := s.FinishPlan(ctx, "SEED", res, control.Entry{At: t0, Request: "seed", Kind: "plan-finished", Decision: control.NoDecision}); err != nil {
				b.Fatal(err)
			}
			raw := controltest.Raw(b, loc)
			tx, err := raw.BeginTx(ctx, nil)
			if err != nil {
				b.Fatal(err)
			}
			for i := range 10000 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO plans (ref, id, status, env, stack, author_issuer, author_subject, author_person, author_display,
					author, teams, origin, requested_at, made_at, expires_at, risks, changes, source_digest, config_id, document, text)
					SELECT ?, ?, status, env, ?, author_issuer, author_subject, author_person, author_display, author, teams, origin,
					requested_at + ?, made_at, expires_at, risks, changes, source_digest, config_id, document, text FROM plans WHERE ref = 'SEED'`,
					fmt.Sprintf("R%05d", i), fmt.Sprintf("id-%05d", i), fmt.Sprintf("s%03d", i%500), int64(i+1)); err != nil {
					b.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				got, next, err := s.Plans(ctx, control.PlanFilter{Env: "prod", Limit: 50, Keep: keepNone}, t0)
				if err != nil || len(got) != 0 || next != "" {
					b.Fatalf("%d plans, next %q, %v", len(got), next, err)
				}
			}
		})
	}
}

func keepNone(string, string) bool { return false }
