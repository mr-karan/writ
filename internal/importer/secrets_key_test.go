package importer

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"math/big"
	mrand "math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/value"
)

// withPublic returns k with a public value note of another object of the
// run; see check.
func withPublic(k Known, note string) Known {
	k.Secrets = append(append([]value.Value{}, k.Secrets...), value.StringVal(note))
	return k
}

// Every key that the check reads has its private numbers found, so a run
// is never public by the 2 to 6 bits it encodes of a private number.
func TestKeySecretRangesFound(t *testing.T) {
	keys := testKeys()
	for _, tk := range keys {
		s := newSecretSet()
		s.keep(nil, tk.priv, true)
		k := s.keys[testBody(tk.priv)]
		k.derive(nil)
		if k.pub == nil || k.secret == nil {
			t.Errorf("%s: parsed %v, private numbers found %v, want both", tk.name, k.pub != nil, k.secret != nil)
		}
	}
}

// A review found 47 of 287 real CA certificates refused beside a PKCS #8
// RSA private key, and the bundle that holds them. A CA certificate whose
// last extension is a path length of 0 holds the bytes 02 01 00 30 0d 06
// 09 2a 86 48 86 f7 0d 01 01: the integer 0 and the start of the
// signature algorithm. A PKCS #8 RSA key holds the same 15 bytes: its
// version 0 and its algorithm identifier. Other certificates share 2 bits
// of a byte that is not a private number, as the version byte, with the
// algorithm identifier that the public key holds too. The certificates
// here are for an unrelated key, with subjects of six lengths, so the
// shared bytes are at each offset in the base64.
func TestCACertificatesBesidePKCS8Key(t *testing.T) {
	tk := testKeys()[1]
	if tk.name != "RSA PKCS #8" {
		t.Fatalf("testKeys()[1] is %s, want RSA PKCS #8", tk.name)
	}
	ca, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pathLenZero := []byte{0x30, 0x06, 0x01, 0x01, 0xff, 0x02, 0x01, 0x00}
	k := secretOnly(tk.priv)
	var bundle strings.Builder
	for n := range 6 {
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(int64(n + 1)), Subject: pkix.Name{CommonName: strings.Repeat("c", 10+n)},
			NotBefore: time.Unix(1_800_000_000, 0), NotAfter: time.Unix(1_900_000_000, 0),
			ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: pathLenZero}},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &ca.PublicKey, ca)
		if err != nil {
			t.Fatal(err)
		}
		cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		if refused(k, cert) {
			t.Errorf("a CA certificate with a subject of %d bytes is refused, want it accepted", 10+n)
		}
		bundle.WriteString(cert)
	}
	if refused(k, bundle.String()) {
		t.Error("a bundle of CA certificates is refused, want it accepted")
	}
}

// A review found that a certificate made parts of any length of a private
// key public, the certificate itself among them, and that its signature
// was never checked. A certificate for the victim's public key, signed by
// an unrelated CA, with 60 bytes of D in an extension, made a part of 64
// characters of D public. A certificate now makes only parts of 8 to 15
// bytes public, and not when it carries a byte of a private number, at
// any offset. The three offsets of the extension put D at each offset in
// the base64.
func TestCertificateCarryingPrivateBytes(t *testing.T) {
	victim, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(victim)
	body := base64.StdEncoding.EncodeToString(der)
	dpos := bytes.Index(der, victim.D.Bytes())
	start := (dpos + 33) * 4 / 3
	start -= start % 4
	long, short := body[start:start+64], body[start:start+12]
	key := secretOnly(testPEM("RSA PRIVATE KEY", body))
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "unrelated ca"},
		NotBefore: time.Unix(1_800_000_000, 0), NotAfter: time.Unix(1_900_000_000, 0), IsCA: true, BasicConstraintsValid: true,
	}
	for pad := range 3 {
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(8), Subject: pkix.Name{CommonName: "leaf"}, NotBefore: caTmpl.NotBefore, NotAfter: caTmpl.NotAfter,
			ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1}, Value: append(make([]byte, pad), der[dpos+30:dpos+90]...)}},
		}
		cder, err := x509.CreateCertificate(rand.Reader, leaf, caTmpl, &victim.PublicKey, ca)
		if err != nil {
			t.Fatal(err)
		}
		cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cder}))
		withCert := withPublic(key, cert)
		for name, c := range map[string]struct {
			k Known
			x string
		}{
			"a part of 64 characters of D, the certificate in the run":        {withCert, long},
			"a part of 64 characters of D padded, the certificate in the run": {withCert, "x=" + long + ";"},
			"a part of 12 characters of D, the certificate in the run":        {withCert, short},
			"the certificate": {key, cert},
			"the certificate, the certificate in the run": {withCert, cert},
		} {
			if !refused(c.k, c.x) {
				t.Errorf("extension at offset %d: %s is accepted, want it refused", pad, name)
			}
		}
	}
}

// encryptedOpenSSH returns k as an OpenSSH private key with a passphrase:
// its public key in the clear, and a private section of random bytes in
// place of the encrypted one. It also returns the offset of the private
// section's bytes.
func encryptedOpenSSH(k crypto.Signer, seed byte) (string, int) {
	rng := mrand.New(mrand.NewChaCha8([32]byte{seed}))
	random := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		return b
	}
	var opts wireWriter
	opts.str(random(16))
	_ = binary.Write(&opts, binary.BigEndian, uint32(16))
	var w wireWriter
	w.WriteString("openssh-key-v1\x00")
	w.str([]byte("aes256-ctr"))
	w.str([]byte("bcrypt"))
	w.str(opts.Bytes())
	_ = binary.Write(&w, binary.BigEndian, uint32(1))
	w.str(sshPublic(k))
	section := w.Len() + 4
	w.str(random(160))
	return string(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: w.Bytes()})), section
}

func authorizedKey(k crypto.Signer) string {
	wire := sshPublic(k)
	return string(wire[4:4+binary.BigEndian.Uint32(wire)]) + " " + base64.StdEncoding.EncodeToString(wire) + " u20@example"
}

// A review found that an unrelated OpenSSH public key of the same type was
// refused beside an OpenSSH private key with a passphrase: the private key
// holds its public key in the clear, and both public keys start with the
// same key type. The check reads that public key, so its encodings, and
// another public key, are accepted. The encrypted private section is
// refused.
func TestEncryptedOpenSSHPublicKey(t *testing.T) {
	signers := func() []crypto.Signer {
		r, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		_, ed, _ := ed25519.GenerateKey(rand.Reader)
		ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		return []crypto.Signer{r, ed, ec}
	}
	own, other := signers(), signers()
	for i, k := range own {
		priv, section := encryptedOpenSSH(k, byte(i))
		name := strings.Fields(authorizedKey(k))[0]
		body := testBody(priv)
		part := body[(section+2)/3*4+8:][:16]
		known := secretOnly(priv)
		for what, x := range map[string]string{
			"its public key":                    authorizedKey(k),
			"an unrelated key of the same type": authorizedKey(other[i]),
		} {
			if refused(known, x) {
				t.Errorf("%s: %s is refused, want it accepted", name, what)
			}
		}
		for what, x := range map[string]string{
			"the whole key":                           priv,
			"the body":                                body,
			"a part of the encrypted section":         part,
			"a part of the encrypted section, padded": "k=" + part + ";",
		} {
			if !refused(known, x) {
				t.Errorf("%s: %s is accepted, want it refused", name, what)
			}
		}
	}
}

// hugeRSA returns the numbers of an RSA key whose modulus has 2^20 bits,
// with p odd and q smaller than p, so a check of the key computes q^(p-2)
// mod p.
func hugeRSA() (n, p, q *big.Int) {
	rng := mrand.New(mrand.NewChaCha8([32]byte{20}))
	half := func() *big.Int {
		b := make([]byte, 1<<16)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		b[0] |= 0x80
		b[len(b)-1] |= 1
		return new(big.Int).SetBytes(b)
	}
	p, q = half(), half()
	if p.Cmp(q) < 0 {
		p, q = q, p
	}
	return new(big.Int).Mul(p, q), p, q
}

// allocated returns the bytes that f allocates.
func allocated(f func()) uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	before := ms.TotalAlloc
	f()
	runtime.ReadMemStats(&ms)
	return ms.TotalAlloc - before
}

// A review found that the OpenSSH reader checked an RSA key of any size:
// a modulus of 2^20 bits took 4 to 13 s and 1.6 GB. The reader refuses a
// number over 16384 bits before it checks the key, and the check reads no
// private key over 16 KiB, so a PKCS #1 key of the same size is not
// checked either.
func TestHugeRSAKeyNotChecked(t *testing.T) {
	n, p, q := hugeRSA()
	var priv wireWriter
	priv.Write([]byte{0, 0, 0, 7, 0, 0, 0, 7})
	priv.str([]byte("ssh-rsa"))
	for _, x := range []*big.Int{n, big.NewInt(65537), n, big.NewInt(3), p, q} {
		priv.str(x.Bytes())
	}
	var w wireWriter
	w.WriteString("openssh-key-v1\x00")
	w.str([]byte("none"))
	w.str([]byte("none"))
	w.str(nil)
	_ = binary.Write(&w, binary.BigEndian, uint32(1))
	w.str([]byte("x"))
	w.str(priv.Bytes())
	var err error
	if got := allocated(func() { _, err = parseOpenSSH(w.Bytes()) }); err == nil || got > 64<<20 {
		t.Errorf("OpenSSH key with a modulus of 2^20 bits: error %v, %d bytes allocated, want an error and at most 64 MiB", err, got)
	}
	type pkcs1 struct {
		Version       int
		N, E, D, P, Q *big.Int
		Dp, Dq, Qinv  *big.Int
	}
	der, err := asn1.Marshal(pkcs1{0, n, big.NewInt(65537), n, p, q, p, q, p})
	if err != nil {
		t.Fatal(err)
	}
	body := base64.StdEncoding.EncodeToString(der)
	k := secretOnly(testPEM("RSA PRIVATE KEY", body))
	var leak bool
	if got := allocated(func() { leak = refused(k, body[800:816]) }); !leak || got > 64<<20 {
		t.Errorf("a part of a PKCS #1 key with a modulus of 2^20 bits: refused %v, %d bytes allocated, want refused and at most 64 MiB", leak, got)
	}
}

// An OpenSSH Ed25519 key holds its public key a second time right after
// its seed. A run of the authorized_keys line that matches the key text
// only there encodes 2 to 6 bits of the seed's last byte by its place,
// but each of its bits comes from the public key, so it is accepted. A
// gate run found 3 of 100 parts of a public_key_openssh refused.
func TestOpenSSHPublicKeyAfterSeed(t *testing.T) {
	refusedParts := 0
	for range 40 {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		known := secretOnly(openSSH(k))
		for _, part := range windows(base64.StdEncoding.EncodeToString(sshPublic(k)), 16, 1) {
			if refused(known, part) {
				refusedParts++
			}
		}
	}
	if refusedParts != 0 {
		t.Errorf("%d parts of 16 characters of the public keys of 40 Ed25519 keys are refused, want 0", refusedParts)
	}
}
