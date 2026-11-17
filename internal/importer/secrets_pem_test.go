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
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"math/big"
	mrand "math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// testPEM wraps body at 64 columns between armor lines of kind.
func testPEM(kind, body string) string {
	var b strings.Builder
	b.WriteString("-----BEGIN " + kind + "-----\n")
	for i := 0; i < len(body); i += 64 {
		b.WriteString(body[i:min(i+64, len(body))] + "\n")
	}
	b.WriteString("-----END " + kind + "-----\n")
	return b.String()
}

// testBody returns the base64 body of a PEM text: its lines without armor
// lines and line breaks.
func testBody(text string) string {
	var b strings.Builder
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); !strings.HasPrefix(line, "-----") {
			b.WriteString(line)
		}
	}
	return b.String()
}

// fakeKey returns the body and the PEM of a text that has the armor of a
// private key and a body of random bytes, which parses as no key.
func fakeKey() (body, key string) {
	rng := mrand.New(mrand.NewChaCha8([32]byte{42}))
	raw := make([]byte, 600)
	for i := range raw {
		raw[i] = byte(rng.Uint32())
	}
	body = base64.StdEncoding.EncodeToString(raw)
	return body, testPEM("RSA PRIVATE KEY", body)
}

// wireWriter writes the OpenSSH wire form, independently of the code under
// test.
type wireWriter struct{ bytes.Buffer }

func (w *wireWriter) str(b []byte) {
	_ = binary.Write(w, binary.BigEndian, uint32(len(b)))
	w.Write(b)
}

func (w *wireWriter) mpint(n *big.Int) {
	b := n.Bytes()
	if len(b) > 0 && b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	w.str(b)
}

// sshPublic returns the OpenSSH wire form of the public key of k.
func sshPublic(k crypto.Signer) []byte {
	var w wireWriter
	switch p := k.Public().(type) {
	case *rsa.PublicKey:
		w.str([]byte("ssh-rsa"))
		w.mpint(big.NewInt(int64(p.E)))
		w.mpint(p.N)
	case ed25519.PublicKey:
		w.str([]byte("ssh-ed25519"))
		w.str(p)
	case *ecdsa.PublicKey:
		point, _ := p.Bytes()
		w.str([]byte("ecdsa-sha2-nistp256"))
		w.str([]byte("nistp256"))
		w.str(point)
	}
	return w.Bytes()
}

// openSSH returns k as an unencrypted OpenSSH private key, as
// "openssh-key-v1" in OpenSSH's PROTOCOL.key defines it.
func openSSH(k crypto.Signer) string {
	var priv wireWriter
	priv.Write([]byte{1, 2, 3, 4, 1, 2, 3, 4})
	switch p := k.(type) {
	case *rsa.PrivateKey:
		priv.str([]byte("ssh-rsa"))
		priv.mpint(p.N)
		priv.mpint(big.NewInt(int64(p.E)))
		priv.mpint(p.D)
		priv.mpint(p.Precomputed.Qinv)
		priv.mpint(p.Primes[0])
		priv.mpint(p.Primes[1])
	case ed25519.PrivateKey:
		priv.str([]byte("ssh-ed25519"))
		priv.str(p.Public().(ed25519.PublicKey))
		priv.str(p)
	case *ecdsa.PrivateKey:
		point, _ := p.PublicKey.Bytes()
		d, _ := p.Bytes()
		priv.str([]byte("ecdsa-sha2-nistp256"))
		priv.str([]byte("nistp256"))
		priv.str(point)
		priv.mpint(new(big.Int).SetBytes(d))
	}
	priv.str(nil) // comment
	for i := byte(1); priv.Len()%8 != 0; i++ {
		priv.WriteByte(i)
	}
	var w wireWriter
	w.WriteString("openssh-key-v1\x00")
	w.str([]byte("none"))
	w.str([]byte("none"))
	w.str(nil)
	_ = binary.Write(&w, binary.BigEndian, uint32(1))
	w.str(sshPublic(k))
	w.str(priv.Bytes())
	return string(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: w.Bytes()}))
}

// testKey is a real private key in one PEM format, with the encodings of
// its public key that a stack can hold in the clear.
type testKey struct {
	name string
	priv string
	// public are the public key as PKIX PEM, as PKCS #1 PEM for RSA, as
	// an authorized_keys line for a key OpenSSH reads, and a self-signed
	// certificate for the key.
	public []string
	// publicDER are the DER and wire forms of the public key and of the
	// certificate.
	publicDER [][]byte
}

var testKeys = sync.OnceValue(func() []testKey {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	ec256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ec384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	encode := func(kind string, der []byte) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
	}
	pkcs8 := func(k any) []byte {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			panic(err)
		}
		return der
	}
	sec1, _ := x509.MarshalECPrivateKey(ec256)
	newKey := func(name, priv string, k crypto.Signer, ssh bool) testKey {
		spki, _ := x509.MarshalPKIXPublicKey(k.Public())
		tk := testKey{name: name, priv: priv, public: []string{encode("PUBLIC KEY", spki)}, publicDER: [][]byte{spki}}
		if r, ok := k.Public().(*rsa.PublicKey); ok {
			der := x509.MarshalPKCS1PublicKey(r)
			tk.public = append(tk.public, encode("RSA PUBLIC KEY", der))
			tk.publicDER = append(tk.publicDER, der)
		}
		if ssh {
			wire := sshPublic(k)
			name := string(wire[4 : 4+binary.BigEndian.Uint32(wire)])
			tk.public = append(tk.public, name+" "+base64.StdEncoding.EncodeToString(wire)+" test@example")
			tk.publicDER = append(tk.publicDER, wire)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "u20.example"},
			NotBefore: time.Unix(1_800_000_000, 0), NotAfter: time.Unix(1_800_086_400, 0),
		}
		cert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
		if err != nil {
			panic(err)
		}
		tk.public = append(tk.public, encode("CERTIFICATE", cert))
		tk.publicDER = append(tk.publicDER, cert)
		return tk
	}
	return []testKey{
		newKey("RSA PKCS #1", encode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey)), rsaKey, true),
		newKey("RSA PKCS #8", encode("PRIVATE KEY", pkcs8(rsaKey)), rsaKey, true),
		newKey("P-256 SEC 1", encode("EC PRIVATE KEY", sec1), ec256, true),
		newKey("P-384 PKCS #8", encode("PRIVATE KEY", pkcs8(ec384)), ec384, false),
		newKey("Ed25519 PKCS #8", encode("PRIVATE KEY", pkcs8(ed)), ed, true),
		newKey("RSA OpenSSH", openSSH(rsaKey), rsaKey, true),
		newKey("P-256 OpenSSH", openSSH(ec256), ec256, true),
		newKey("Ed25519 OpenSSH", openSSH(ed), ed, true),
	}
})

// resultSchema is a resource with one public string attribute.
var resultSchema = &schema.Resource{Block: schema.Block{Attributes: []schema.Attribute{
	{Name: "result", Type: types.TString, Optional: true},
	{Name: "secret", Type: types.TString, Optional: true, Sensitive: true},
	{Name: "note", Type: types.TString, Optional: true},
}}}

// refused reports whether k refuses an object whose result is x.
func refused(k Known, x string) bool {
	return check(k, resultSchema, "unrelated-import-id", value.ObjectVal(map[string]value.Value{"result": value.StringVal(x)})) != nil
}

func secretOnly(s string) Known {
	return Known{Secrets: []value.Value{value.StringVal(s).Secret()}}
}

// windows returns each part of n bytes of s, at each offset that is a
// multiple of step.
func windows(s string, n, step int) []string {
	var parts []string
	for i := 0; i+n <= len(s); i += step {
		parts = append(parts, s[i:i+n])
	}
	return parts
}

// A private key holds parts of its public key. The check derives the
// public key from the private key, so the public key, each of its parts,
// a certificate for it, each part of 16 or more bytes of the certificate,
// and public armor are accepted when the private key is the only thing
// the stack knows. A part of 8 bytes of a certificate can match the
// private key by coincidence, as an object identifier both hold, so it is
// accepted when a value of state holds the certificate.
func TestKeyPublicMaterialAccepted(t *testing.T) {
	for _, tk := range testKeys() {
		t.Run(tk.name, func(t *testing.T) {
			keyOnly := secretOnly(tk.priv)
			cert := tk.public[len(tk.public)-1]
			withCert := withPublic(keyOnly, cert)
			var cases, certParts []string
			for _, p := range tk.public {
				body := testBody(p)
				if !strings.HasPrefix(p, "-----") {
					body = strings.Fields(p)[1]
				}
				cases = append(cases, p, form(p), strings.ReplaceAll(p, "\n", "\r\n"), body)
				cases = append(cases, windows(body, 16, 1)...)
				cases = append(cases, windows(p, 24, 5)...)
				if p == cert {
					certParts = windows(form(p), 8, 3)
				} else {
					cases = append(cases, windows(form(p), 8, 3)...)
				}
			}
			cases = append(cases, "-----BEGIN", "-----END", "-----BEGIN PUBLIC KEY-----", "-----END PUBLIC KEY-----", "-----BEGIN CERTIFICATE-----")
			for _, c := range []struct {
				name  string
				known Known
				cases []string
			}{
				{"the private key alone", keyOnly, cases},
				{"the certificate in the run", withCert, append(certParts, cases...)},
			} {
				n := 0
				for _, x := range c.cases {
					if refused(c.known, x) {
						n++
						if n <= 3 {
							t.Errorf("with %s, public material %q is refused", c.name, x)
						}
					}
				}
				if n > 0 {
					t.Errorf("with %s, %d of %d values of public material are refused, want 0", c.name, n, len(c.cases))
				}
			}
		})
	}
}

// privateOnly returns the parts of 16 characters of the body of a private
// key, at offsets that are multiples of 4, whose 12 bytes are in none of
// the DER or wire forms of its public key and certificate.
func privateOnly(tk testKey) []string {
	body := testBody(tk.priv)
	var parts []string
	for i := 0; i+16 <= len(body); i += 4 {
		raw, err := base64.StdEncoding.DecodeString(body[i : i+16])
		if err != nil {
			continue
		}
		public := false
		for _, der := range tk.publicDER {
			public = public || bytes.Contains(der, raw)
		}
		if !public {
			parts = append(parts, body[i:i+16])
		}
	}
	return parts
}

// A part of a private key that is not in its public key is refused in
// every encoding: as it is, with other text around it, and inside the
// whole key with its line breaks escaped, indented, or replaced.
func TestKeyPrivateMaterialRefused(t *testing.T) {
	_, fake := fakeKey()
	keys := append([]testKey{{name: "text with the armor of a key that does not parse", priv: fake}}, testKeys()...)
	for _, tk := range keys {
		t.Run(tk.name, func(t *testing.T) {
			k := secretOnly(tk.priv)
			parts := privateOnly(tk)
			if len(parts) < 4 {
				t.Fatalf("only %d parts of the private key are outside its public key", len(parts))
			}
			body := testBody(tk.priv)
			lines := strings.Split(strings.TrimSuffix(tk.priv, "\n"), "\n")
			frag := parts[len(parts)/2]
			end := strings.Index(tk.priv, "-----END")
			cases := map[string]string{
				"armor line":                       lines[0],
				"end armor line":                   lines[len(lines)-1],
				"armor and the first bytes":        lines[0] + "\n" + body[:12],
				"whole key":                        tk.priv,
				"whole key with CRLF":              strings.ReplaceAll(tk.priv, "\n", "\r\n"),
				"whole key, JSON-escaped":          `{"k":"` + strings.ReplaceAll(tk.priv, "\n", `\n`) + `"}`,
				"whole key, escaped twice":         strings.ReplaceAll(tk.priv, "\n", `\\n`),
				"whole key, YAML-indented":         "content: |\n      " + strings.ReplaceAll(strings.TrimSuffix(tk.priv, "\n"), "\n", "\n      ") + "\n",
				"whole key, spaces for newlines":   strings.ReplaceAll(tk.priv, "\n", " "),
				"whole key, tab-indented":          strings.ReplaceAll(tk.priv, "\n", "\n\t"),
				"body in JSON":                     `{"key":"` + body + `"}`,
				"body":                             body,
				"body lines after ssl_key=":        "ssl_key=" + strings.Join(lines[1:], "\n"),
				"part padded":                      "key=" + frag + ";",
				"two parts padded":                 "id:" + parts[0] + parts[1] + ":end",
				"part with a line break":           frag[:8] + "\n" + frag[8:],
				"part with an escaped line break":  frag[:8] + `\n` + frag[8:],
				"part in a sentence":               "the key starts " + frag + " and goes on",
				"part of 16 characters, with tabs": "\t" + frag + "\t",
			}
			for _, p := range parts {
				cases["part "+p[:4]] = p
			}
			// The body of a SEC 1 or Ed25519 key, or of an OpenSSH key, ends
			// with public bytes or with padding.
			if tail := body[len(body)-16:]; slices.Contains(parts, tail) {
				cases["last bytes and -----END"] = tk.priv[end-16 : end+8]
			}
			for name, x := range cases {
				if !refused(k, x) {
					t.Errorf("%s is accepted, want it refused", name)
				}
			}
		})
	}
}

// A private key inside a JSON document, with its line breaks escaped, as
// in a cloud service account key, is found by its key text, and its public
// key is derived from it.
func TestKeyInJSONSecret(t *testing.T) {
	tk := testKeys()[1]
	doc := `{"type":"service_account","private_key":"` + strings.ReplaceAll(tk.priv, "\n", `\n`) + `","client_email":"u20@example.com"}`
	k := secretOnly(doc)
	if refused(k, tk.public[0]) || refused(k, testBody(tk.public[0])[40:72]) {
		t.Error("public material of a key in a JSON document is refused")
	}
	parts := privateOnly(tk)
	for name, x := range map[string]string{
		"the key with raw line breaks": tk.priv,
		"a part of the key":            parts[len(parts)/2],
		"a part of the document":       `"client_email":"u20@example.com"`,
	} {
		if !refused(k, x) {
			t.Errorf("%s is accepted, want it refused", name)
		}
	}
}

// Armor-like text inside a secret that is not a PEM is part of the secret.
func TestCheckArmorInPlainSecret(t *testing.T) {
	if !refused(secretOnly("prefix\n-----BEGIN SYNTHETIC SECRET-----\nsuffix"), "-----BEGIN SYNTHETIC SECRET-----") {
		t.Error("an armor-like line of a plain secret is accepted, want it refused")
	}
}

// No public value makes a part of a secret public: not a public value of
// another object of the run, and not a public value of an object of the
// run that keeps the secret. A review found that a padded
// public value made a short secret, the inner password of a JSON
// document, and a part of a private key public.
func TestPublicValueDoesNotVouch(t *testing.T) {
	keyBody, key := fakeKey()
	frag := keyBody[300:332]
	doc := `{"user":"admin","password":"Xq7-inner-pw-93"}`
	withRow := withPublic
	keyState := secretOnly(key)
	for _, tc := range []struct {
		name  string
		known Known
		x     string
	}{
		{"short secret, a public note holds it padded", withRow(secretOnly("hunter2"), "hunter2024-backups"), "hunter2"},
		{"short secret, a public note is other text", withRow(secretOnly("k9Zq"), "bucket-k9Zq7-logs"), "k9Zq"},
		{"8-byte secret, a public note holds it padded", withRow(secretOnly("hunter22"), "hunter22-backups"), "hunter22"},
		{"inner password, a public URL holds it", withRow(secretOnly(doc), "postgres://admin:Xq7-inner-pw-93@db:5432/app"), "Xq7-inner-pw-93"},
		{"private part, a public note is the part", withRow(keyState, frag), frag},
		{"private part, a public note holds it padded", withRow(keyState, "key="+frag+";"), frag},
		{"private part, a public note holds the whole key", withRow(keyState, key), frag},
		{"private part, a public note holds the body", withRow(keyState, keyBody), frag},
	} {
		if !refused(tc.known, tc.x) {
			t.Errorf("%s: accepted, want refused", tc.name)
		}
	}
}

// The same, with objects of the run: the object o keeps the secret, or a
// copy of it, and holds it with padding in a public attribute.
func TestRunObjectDoesNotVouch(t *testing.T) {
	keyBody, key := fakeKey()
	frag := keyBody[300:332]
	obj := func(m map[string]string) value.Value {
		o := map[string]value.Value{}
		for k, v := range m {
			o[k] = value.StringVal(v)
			if k == "secret" {
				o[k] = o[k].Secret()
			}
		}
		return value.ObjectVal(o)
	}
	blk := resultSchema.Block
	for _, tc := range []struct {
		name               string
		keeper, other, src map[string]string
	}{
		{"short secret, o keeps it, public note padded", map[string]string{"secret": "hunter2"}, map[string]string{"secret": "hunter2", "note": "hunter2-prod-db"}, map[string]string{"result": "hunter2"}},
		{"private part, o keeps the key, note padded", map[string]string{"secret": key}, map[string]string{"secret": key, "note": "id:" + frag + ":end"}, map[string]string{"result": frag}},
		{"private part, o keeps the key, note is the part", map[string]string{"secret": key}, map[string]string{"secret": key, "note": frag}, map[string]string{"result": frag}},
		{"private part, o without the key, note padded", map[string]string{"secret": key}, map[string]string{"note": "id:" + frag + ":end"}, map[string]string{"result": frag}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSecretSet()
			keeper, o, src := &node{}, &node{}, &node{}
			s.addState(keeper, blk, obj(tc.keeper))
			s.addState(o, blk, obj(tc.other))
			s.addState(src, blk, obj(tc.src))
			if err := s.ready(); err != nil {
				t.Fatal(err)
			}
			if !inClearOf(s, blk, obj(tc.src), src, src) {
				t.Error("the state of src is accepted, want it refused")
			}
			if !inClearOf(s, blk, obj(tc.src), nil, src) {
				t.Error("the code of src is accepted, want it refused")
			}
		})
	}
}

// A secret with CRLF line breaks that has fewer than 8 bytes without them
// is still found inside a value that holds it as it is.
func TestShortFormSecretInsideValue(t *testing.T) {
	k := secretOnly("abc\r\ndef")
	if !refused(k, "x=abc\r\ndef;") {
		t.Error("a value that holds the secret abc\\r\\ndef is accepted, want it refused")
	}
	if refused(k, "x=abcdef;") {
		t.Error("a value that holds abcdef is refused; the secret is 6 bytes without its line break")
	}
}

// A public value that only repeats a secret, as a random.Password whose
// password is "none" repeats it in its id, makes no part of it public. The
// object may repeat its own secret, and no other object may.
func TestRepeatedSecretDoesNotVouch(t *testing.T) {
	res := &schema.Resource{Block: schema.Block{Attributes: []schema.Attribute{
		{Name: "result", Type: types.TString, Sensitive: true},
		{Name: "id", Type: types.TString},
	}}}
	for _, tc := range []struct {
		secret, id string
		refused    bool
	}{
		{"none", "none", true},
		{"u20-repeated-password", "u20-repeated-password", true},
		{"u20-repeated-password", "repeated", true},
		{"u20-repeated-password", "u20-other-value", false},
	} {
		known := Known{Secrets: []value.Value{value.StringVal(tc.secret).Secret(), value.StringVal(tc.secret)}}
		read := value.ObjectVal(map[string]value.Value{"result": value.StringVal("own").Secret(), "id": value.StringVal(tc.id)})
		if err := check(known, res, "unrelated-import-id", read); (err != nil) != tc.refused {
			t.Errorf("Check of id %q beside the password %q: %v, want refused %v", tc.id, tc.secret, err, tc.refused)
		}
	}
}
