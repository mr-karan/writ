package control_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
)

func indexWrite(stack string, version int64, rows ...control.IndexRow) control.IndexWrite {
	return control.IndexWrite{Env: "prod", Stack: stack, Backend: control.BackendHTTP, Version: version, At: t0, Rows: rows}
}

// The rows of a stack@env are replaced together, never by an older state
// version than the one they came from, and the stacks whose newest state
// is ahead of their index are the ones a refresh reads.
func TestStateIndex(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		sg := control.IndexRow{Address: "aws_security_group.web", Type: "aws_security_group", Provider: "registry.opentofu.org/hashicorp/aws",
			ID: "sg-0a1b2c3d4e5f60718", ARN: "arn:aws:ec2:ap-south-1:123456789012:security-group/sg-0a1b2c3d4e5f60718", Region: "ap-south-1"}
		db := control.IndexRow{Address: "aws_db_instance.orders", Type: "aws_db_instance", Provider: "registry.opentofu.org/hashicorp/aws",
			ID: "db-ABCDEFGHIJKLMNOPQRSTUVWXY1", Identifier: "orders-db"}
		pet := control.IndexRow{Address: "random_pet.name", Type: "random_pet", Provider: "registry.opentofu.org/hashicorp/random", ID: "keen-mole"}

		if _, err := s.Index(ctx, "prod", "network"); err != control.ErrNotFound {
			t.Fatalf("the index of a stack nothing indexed: %v, want ErrNotFound", err)
		}
		writeState(t, l, "network", "RUN1", 1)
		writeState(t, l, "app", "RUN2", 1)
		writeState(t, l, "app", "RUN3", 2)
		stale, err := s.StaleIndexes(ctx, 10)
		if err != nil || !slices.Equal(stale, []control.StaleIndex{{"prod", "app", 2}, {"prod", "network", 1}}) {
			t.Fatalf("stale indexes before any index: %v %v", stale, err)
		}
		if stale, err := s.StaleIndexes(ctx, 1); err != nil || len(stale) != 1 {
			t.Fatalf("stale indexes with a limit of one: %v %v", stale, err)
		}

		if ok, err := s.ReplaceIndex(ctx, indexWrite("network", 1, sg, db)); !ok || err != nil {
			t.Fatalf("the first index of network: %v %v", ok, err)
		}
		if ok, err := s.ReplaceIndex(ctx, indexWrite("app", 1, pet)); !ok || err != nil {
			t.Fatalf("an index of app at version 1: %v %v", ok, err)
		}
		if stale, err := s.StaleIndexes(ctx, 10); err != nil || !slices.Equal(stale, []control.StaleIndex{{"prod", "app", 2}}) {
			t.Fatalf("stale indexes after network's: %v %v", stale, err)
		}
		if ok, err := s.ReplaceIndex(ctx, indexWrite("app", 2)); !ok || err != nil {
			t.Fatalf("an index of app at version 2, with no rows: %v %v", ok, err)
		}
		if ok, err := s.ReplaceIndex(ctx, indexWrite("app", 1, pet)); ok || err != nil {
			t.Fatalf("an index of an older version replaced a newer one: %v %v", ok, err)
		}
		if ix, err := s.Index(ctx, "prod", "app"); err != nil || ix.Version != 2 || len(ix.Rows) != 0 {
			t.Fatalf("app's index: %+v %v", ix, err)
		}
		if stale, err := s.StaleIndexes(ctx, 10); err != nil || len(stale) != 0 {
			t.Fatalf("stale indexes when each is current: %v %v", stale, err)
		}

		ix, err := s.Index(ctx, "prod", "network")
		if err != nil || ix.Backend != control.BackendHTTP || ix.Version != 1 || !ix.At.Equal(t0) || !slices.Equal(ix.Rows, []control.IndexRow{db, sg}) {
			t.Fatalf("network's index: %+v %v", ix, err)
		}
		edited := sg
		edited.Region = "eu-west-1"
		w := indexWrite("network", 1, edited)
		w.At = t0.Add(time.Minute)
		if ok, err := s.ReplaceIndex(ctx, w); !ok || err != nil {
			t.Fatalf("an index of the same version again: %v %v", ok, err)
		}
		if ix, err := s.Index(ctx, "prod", "network"); err != nil || !slices.Equal(ix.Rows, []control.IndexRow{edited}) {
			t.Fatalf("network's index after a second read of version 1: %+v %v", ix, err)
		}
	})
}

// The index secrets of an apply are kept with the state versions its run
// wrote and with no other, go when their version goes, and a stale stack
// whose newest version has them comes before one whose has none.
func TestIndexSecrets(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		seal := func(v int64) []byte { return fmt.Appendf(nil, "sealed secrets of version %d", v) }
		writeState(t, l, "app", "RUN1", 1)
		writeState(t, l, "app", "RUN2", 2)
		writeState(t, l, "api", "RUN3", 1)
		if n, err := s.PutIndexSecrets(ctx, "prod", "app", "RUN2", seal); n != 1 || err != nil {
			t.Fatalf("sealed the secrets of RUN2 for %d versions: %v, want 1", n, err)
		}
		if got, err := s.IndexSecrets(ctx, "prod", "app", 2); err != nil || !bytes.Equal(got, seal(2)) {
			t.Fatalf("the index secrets of version 2: %q %v", got, err)
		}
		for _, v := range []int64{1, 9} {
			if got, err := s.IndexSecrets(ctx, "prod", "app", v); !errors.Is(err, control.ErrNotFound) {
				t.Errorf("the index secrets of version %d, which RUN2 did not write: %q %v, want ErrNotFound", v, got, err)
			}
		}
		if got, err := s.IndexSecrets(ctx, "prod", "api", 1); !errors.Is(err, control.ErrNotFound) {
			t.Errorf("the index secrets of another stack: %q %v, want ErrNotFound", got, err)
		}
		if stale, err := s.StaleIndexes(ctx, 1); err != nil || !slices.Equal(stale, []control.StaleIndex{{"prod", "app", 2}}) {
			t.Fatalf("one stale index: %v %v, want app, whose newest version has index secrets", stale, err)
		}
		for serial := int64(3); serial <= 7; serial++ {
			writeState(t, l, "app", fmt.Sprintf("RUN%d", serial+1), serial)
		}
		if got, err := s.IndexSecrets(ctx, "prod", "app", 2); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the index secrets of a version no longer kept: %q %v, want ErrNotFound", got, err)
		}
		var left int
		if err := controltest.Raw(t, loc).QueryRowContext(ctx, "SELECT count(*) FROM tfstate_versions WHERE index_secrets IS NOT NULL").Scan(&left); err != nil || left != 0 {
			t.Fatalf("%d rows still hold index secrets: %v", left, err)
		}
	})
}
