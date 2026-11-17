package provider

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/mr-karan/writ/internal/tfplugin5"
	"github.com/mr-karan/writ/internal/value"
)

// applyCreate creates an object with ApplyResourceChange, which Writ itself
// never calls: OpenTofu applies. It decodes the new state the way every
// Client call does, so a test can check how Writ marks state that only a
// real provider computes. The test providers speak protocol 5.
func applyCreate(t *testing.T, c *Client, typ string, planned, config value.Value) value.Value {
	t.Helper()
	if c.proc.protocol != 5 {
		t.Fatalf("applyCreate speaks protocol 5; %s negotiated %d", c.src, c.proc.protocol)
	}
	w, err := c.wire.resource(typ)
	if err != nil {
		t.Fatal(err)
	}
	var raw [3][]byte
	for i, v := range []value.Value{value.NullVal(), planned, config} {
		if raw[i], err = w.encode(v); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := tfplugin5.NewProviderClient(c.proc.conn).ApplyResourceChange(context.Background(), &tfplugin5.ApplyResourceChange_Request{
		TypeName:     typ,
		PriorState:   &tfplugin5.DynamicValue{Msgpack: raw[0]},
		PlannedState: &tfplugin5.DynamicValue{Msgpack: raw[1]},
		Config:       &tfplugin5.DynamicValue{Msgpack: raw[2]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if diags := c.diags(w.wireBlock, v5Diags(resp.GetDiagnostics())); hasError(diags) {
		t.Fatalf("ApplyResourceChange %s: %+v", typ, diags)
	}
	state, err := w.decode(v5Dynamic(resp.GetNewState()))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSensitiveFileDigestIsSecret(t *testing.T) {
	pool := newTestPool(t)
	defer closePool(t, pool)
	ctx := context.Background()
	c, err := pool.Client(ctx, keyOf(localSrc))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "review-low-entropy-pin-1234"
	cfg := obj(map[string]value.Value{"filename": str(filepath.Join(t.TempDir(), "secret.txt")), "content": str(secret).Secret()})
	plan, err := c.Plan(ctx, PlanRequest{Type: "local_sensitive_file", Prior: value.NullVal(), Proposed: cfg, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	result := applyCreate(t, c, "local_sensitive_file", plan.Planned, cfg)
	digest := result.Get("content_sha256")
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(secret)))
	if digest.AsString() != want {
		t.Fatal("provider did not return content hash")
	}
	if !digest.HasSecret() {
		t.Fatalf("secret-derived SHA256 printable and dictionary-checkable: %s", digest)
	}
}

func TestSensitiveFileAllDigestsAreSecret(t *testing.T) {
	pool := newTestPool(t)
	defer closePool(t, pool)
	c, err := pool.Client(context.Background(), keyOf(localSrc))
	if err != nil {
		t.Fatal(err)
	}
	cfg := obj(map[string]value.Value{"filename": str(filepath.Join(t.TempDir(), "secret.txt")), "content": str("synthetic-pin").Secret()})
	plan, err := c.Plan(context.Background(), PlanRequest{Type: "local_sensitive_file", Prior: value.NullVal(), Proposed: cfg, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	result := applyCreate(t, c, "local_sensitive_file", plan.Planned, cfg)
	for _, name := range []string{"content_md5", "content_sha1", "content_sha256", "content_sha512", "content_base64sha256", "content_base64sha512"} {
		t.Run(name, func(t *testing.T) {
			if !plan.Planned.Get(name).HasSecret() || !result.Get(name).HasSecret() {
				t.Fatal("digest lost secret mark")
			}
		})
	}
}
