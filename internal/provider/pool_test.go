package provider

import (
	"context"
	"errors"
	"testing"
)

func TestPoolStartsNothingUntilUsed(t *testing.T) {
	p := NewPool(PoolOptions{Dirs: Dirs{Cache: t.TempDir()}})
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.Started() != 0 {
		t.Errorf("Started = %d, want 0", p.Started())
	}
	if _, err := p.Client(context.Background(), keyOf(tlsSrc)); !errors.Is(err, errPoolClosed) {
		t.Errorf("Client after Close = %v, want errPoolClosed", err)
	}
}

func TestPoolReportsUninstalledProviders(t *testing.T) {
	p := NewPool(PoolOptions{Dirs: Dirs{Cache: t.TempDir()}})
	p.Add(&Lock{}, []Config{{Source: tlsSrc, Version: "4.4.2"}})
	defer p.Close(context.Background())
	_, err := p.Client(context.Background(), keyOf(tlsSrc))
	var missing *MissingError
	if !errors.As(err, &missing) {
		t.Errorf("Client = %v, want a *MissingError", err)
	}
	if p.Started() != 0 {
		t.Errorf("Started = %d, want 0", p.Started())
	}
}
