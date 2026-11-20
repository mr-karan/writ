//go:build tofu

package tofu

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/egress"
)

const fillNetChildEnv = "WRIT_TEST_FILL_NET_CHILD"

// TestFillOwnNetwork runs real tofu's Fill of a lock file whose provider
// comes from a hostile registry. The lock file is the stack's, so it
// chooses the registry, and the registry the URLs Fill fetches next. In a
// user and network namespace that stands in for writ serve's host, the
// registry listens on the host's localhost:443 and points its downloads
// at an internal service on the host's 127.0.0.1:8080, at the path of the
// instance metadata service's credentials. On writ's own network Fill
// reaches both, so the test can tell. In a network namespace of its own,
// behind a provider install's egress proxy, as writ serve runs it under
// egress_mode report or enforce, it reaches neither.
func TestFillOwnNetwork(t *testing.T) {
	if os.Getenv(fillNetChildEnv) != "" {
		fillNetChild(t)
		return
	}
	bin, err := exec.LookPath("tofu")
	if err != nil {
		t.Skip("needs tofu")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", append(slices.Clone(namespaceArgs), self, "-test.run=^TestFillOwnNetwork$", "-test.v", "-test.count=1", "-test.timeout=5m")...)
	cmd.Env = append(os.Environ(), fillNetChildEnv+"=1", gateBinEnv+"="+bin, gateRootEnv+"="+t.TempDir(), gateCacheEnv+"="+t.TempDir())
	out, err := cmd.CombinedOutput()
	t.Logf("in the namespaces:\n%s", out)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
}

func fillNetChild(t *testing.T) {
	if err := loopbackUp(); err != nil {
		t.Fatal(err)
	}
	cache := os.Getenv(gateCacheEnv)
	enterOwnRoot(t, os.Getenv(gateRootEnv), cache)
	caPEM, leaf := registryCerts(t)
	// Trust the test CA where the view finds the system's bundle.
	bundle, err := filepath.EvalSymlinks(caPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount(ca, bundle, "", syscall.MS_BIND, ""); err != nil {
		t.Fatalf("bind the test CA: %v", err)
	}

	var mu sync.Mutex
	var seen []string
	note := func(who string, r *http.Request) {
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s: %s %s host=%s", who, r.Method, r.URL.Path, r.Host))
		mu.Unlock()
	}
	internal, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(internal, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		note("internal 127.0.0.1:8080", r)
		fmt.Fprint(w, "internal answer")
	}))
	reg, err := net.Listen("tcp", "127.0.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/terraform.json", func(w http.ResponseWriter, r *http.Request) {
		note("registry localhost:443", r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"providers.v1":"/v1/providers/"}`)
	})
	mux.HandleFunc("/v1/providers/acme/probe/versions", func(w http.ResponseWriter, r *http.Request) {
		note("registry localhost:443", r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"versions":[{"version":"1.0.0","protocols":["5.0"],"platforms":[{"os":"linux","arch":"amd64"},{"os":"linux","arch":"arm64"}]}]}`)
	})
	mux.HandleFunc("/v1/providers/acme/probe/1.0.0/download/", func(w http.ResponseWriter, r *http.Request) {
		note("registry localhost:443", r)
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(r.URL.Path, "/")
		fmt.Fprintf(w, `{"protocols":["5.0"],"os":"linux","arch":"%s","filename":"terraform-provider-probe_1.0.0_linux_%s.zip","download_url":"http://127.0.0.1:8080/latest/meta-data/iam/security-credentials/role","shasums_url":"http://127.0.0.1:8080/shasums","shasums_signature_url":"http://127.0.0.1:8080/shasums.sig","shasum":"%s","signing_keys":{"gpg_public_keys":[]}}`, parts[len(parts)-1], parts[len(parts)-1], strings.Repeat("ab", 32))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		note("registry localhost:443 (other)", r)
		http.NotFound(w, r)
	})
	srv := &http.Server{Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{leaf}}}
	go srv.ServeTLS(reg, "", "")

	r := newRunner(t, os.Getenv(gateBinEnv), cache, sharedDir(t, os.ModeSticky|0o777), 2*time.Minute, 1<<20)
	lock := []byte("provider \"localhost/acme/probe\" {\n  version = \"1.0.0\"\n}\n")
	fill := func(n *Net, env []string) []string {
		t.Helper()
		mu.Lock()
		seen = nil
		mu.Unlock()
		if _, err := r.Fill(context.Background(), lock, n, env); err == nil {
			t.Fatal("Fill installed a provider of the hostile registry")
		}
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}

	got := fill(nil, nil)
	const metadata = "internal 127.0.0.1:8080: GET /latest/meta-data/iam/security-credentials/role host=127.0.0.1:8080"
	if !slices.Contains(got, metadata) {
		t.Fatalf("on writ's own network Fill reached %q, not the internal service, so the test cannot tell", got)
	}

	install, err := egress.New(egress.Options{Mode: egress.Enforce, Install: true})
	if err != nil {
		t.Fatal(err)
	}
	const proxyURL = "http://127.0.0.1:3128"
	got = fill(&Net{Ports: map[int]http.Handler{3128: install}}, []string{
		"HTTP_PROXY=" + proxyURL, "HTTPS_PROXY=" + proxyURL, "NO_PROXY=127.0.0.1,localhost,::1",
	})
	if len(got) > 0 {
		t.Errorf("in a network namespace of its own Fill reached the host's loopback: %q", got)
	}
}

// registryCerts are a test CA, and a certificate it signed for localhost.
func registryCerts(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "writ test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
