package oidc_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
)

func TestDeviceSignIn(t *testing.T) {
	ctx := context.Background()
	is := oidctest.New(t)
	is.SetPerson(oidctest.GitLabPerson())
	p := newProvider(t, is, true)
	if !p.Device() {
		t.Fatal("discovery names a device endpoint, and Device is false")
	}
	a, err := p.StartDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.UserCode == "" || a.VerificationURI != is.URL+"/oauth/device" || a.Interval != time.Second || a.ExpiresIn > 5*time.Minute || a.ExpiresIn < 4*time.Minute {
		t.Fatalf("device auth %+v", a)
	}
	approved := make(chan struct{})
	go func() {
		defer close(approved)
		time.Sleep(2500 * time.Millisecond)
		if !is.ApproveDevice(a.UserCode) {
			t.Error("the issuer does not know the user code")
		}
	}()
	tok, err := p.WaitDevice(ctx, a)
	<-approved
	if err != nil {
		t.Fatal(err)
	}
	if tok.Subject != "2002" || tok.Kind != oidc.People || tok.Issuer != "sso" {
		t.Fatalf("token %+v", tok)
	}
	if groups, _ := tok.Claims.Values("groups"); !slices.Equal(groups, []string{"acme", "acme/platform"}) {
		t.Errorf("groups %v, want userinfo's", groups)
	}
	// Polls a second apart while the person had not signed in, then the
	// one that got the token: never more than one an interval.
	if polls := is.DevicePolls(); polls < 2 || polls > 4 {
		t.Errorf("%d polls of the token endpoint in about 3 seconds at an interval of 1 second", polls)
	}
}

func TestDeviceSlowDown(t *testing.T) {
	is := oidctest.New(t)
	is.SetPerson(oidctest.GitLabPerson())
	p := newProvider(t, is, false)
	a, err := p.StartDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	is.SlowDownNext()
	is.ApproveDevice(a.UserCode)
	start := time.Now()
	if _, err := p.WaitDevice(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	// A poll at 1 second answered slow_down, so the next waits 1 + 5.
	if took := time.Since(start); took < 6*time.Second {
		t.Fatalf("the sign-in finished %s after a slow_down; RFC 8628 adds 5 seconds to the interval", took)
	}
}

func TestDeviceRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("denied", func(t *testing.T) {
		is := oidctest.New(t)
		is.SetPerson(oidctest.GitLabPerson())
		p := newProvider(t, is, false)
		a, err := p.StartDevice(ctx)
		if err != nil {
			t.Fatal(err)
		}
		is.DenyDevice(a.UserCode)
		if _, err := p.WaitDevice(ctx, a); err == nil || err.Reason() != oidc.ReasonDenied {
			t.Fatalf("got %v, want access_denied", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		is := oidctest.New(t)
		is.SetPerson(oidctest.GitLabPerson())
		is.SetDevice(time.Second, time.Second)
		p := newProvider(t, is, false)
		a, err := p.StartDevice(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.WaitDevice(ctx, a); err == nil || err.Reason() != oidc.ReasonCodeExpired {
			t.Fatalf("got %v, want expired_token", err)
		}
	})
	t.Run("other audience", func(t *testing.T) {
		is := oidctest.New(t)
		is.SetPerson(oidctest.GitLabPerson())
		is.EditIDToken(func(c map[string]any) { c["aud"] = "someone-else" })
		p := newProvider(t, is, false)
		a, err := p.StartDevice(ctx)
		if err != nil {
			t.Fatal(err)
		}
		is.ApproveDevice(a.UserCode)
		_, ref := p.WaitDevice(ctx, a)
		if ref == nil || ref.Check() != oidc.CheckAudience {
			t.Fatalf("got %v, want an audience failure", ref)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		is := oidctest.New(t)
		p := newProvider(t, is, false)
		a, err := p.StartDevice(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if _, err := p.WaitDevice(cctx, a); err == nil || err.Reason() != oidc.ReasonDeadline {
			t.Fatalf("got %v, want the context's end", err)
		}
	})
	t.Run("no device endpoint", func(t *testing.T) {
		is := oidctest.New(t)
		is.SetNoDevice(true)
		p := newProvider(t, is, false)
		if p.Device() {
			t.Fatal("Device is true with no endpoint")
		}
		if _, err := p.StartDevice(ctx); err == nil || err.Reason() != oidc.ReasonNoDeviceFlow {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("wrong client secret", func(t *testing.T) {
		is := oidctest.New(t)
		p := newProvider(t, is, false)
		is.SetClient("writ-client", "rotated")
		_, err := p.StartDevice(ctx)
		if err == nil || !strings.Contains(err.Message(), "invalid_client") {
			t.Fatalf("a device code with a wrong secret: %v", err)
		}
		if strings.Contains(err.Message(), "client-secret-value") {
			t.Fatal("the error holds the client secret")
		}
	})
}

// A sign-in in the browser may start while a device login polls the token
// endpoint; under -race the two must not touch the same client config.
func TestDeviceLoginBesideBrowserSignIn(t *testing.T) {
	ctx := context.Background()
	is := oidctest.New(t)
	is.SetPerson(oidctest.GitLabPerson())
	p := newProvider(t, is, true)
	a, err := p.StartDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(1500 * time.Millisecond)
		is.ApproveDevice(a.UserCode)
	}()
	stop := make(chan struct{})
	signedIn := make(chan struct{})
	go func() {
		defer close(signedIn)
		for {
			select {
			case <-stop:
				return
			default:
				if u, _ := p.AuthCodeURL("https://writ.example.com/ui/callback", "state", "nonce"); u == "" {
					t.Error("no sign-in URL")
					return
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	if _, err := p.WaitDevice(ctx, a); err != nil {
		t.Error(err)
	}
	close(stop)
	<-signedIn
	<-done
}
