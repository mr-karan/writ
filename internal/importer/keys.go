package importer

import (
	"bytes"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
)

// minKeyText is the length, in base64 characters, of the shortest run of
// a value's key text that the check compares with the key text of a
// private key; see keyText. 16 characters encode 12 bytes, which a private
// key and an unrelated value share by coincidence about once in 2^96.
const minKeyText = 16

// maxKeyDER is the most bytes of DER, or of OpenSSH wire form, that the
// check parses as a private key. A 16384-bit RSA key in PKCS #8 has about
// 9.3 KiB. The check does not read a larger key, so no part of it is
// public.
const maxKeyDER = 16 << 10

// maxRSABits is the largest RSA modulus, and the largest other RSA
// number, in bits, that the check reads. Checking a key with a modulus of
// 2^20 bits takes seconds and more than 1 GiB of memory.
const maxRSABits = 16384

// certPrivateBytes is the length of the shortest run of bytes of a private
// key, with a byte of a private number, that stops a certificate from
// vouching for the key. A part of 8 base64 characters covers at least 5
// whole bytes.
const certPrivateBytes = 5

// privateKey is a PEM private key that a secret holds.
type privateKey struct {
	// label is the PEM type, such as "RSA PRIVATE KEY".
	label string
	// body is the key text of the PEM body; see keyText.
	body string
	// secrets are the secrets that hold the key.
	secrets []string
	// parsed is set once der, pub, publicDER, and public hold what the
	// key derives.
	parsed bool
	// der is the bytes that body encodes, nil when body is not base64.
	der []byte
	// pub is the key's public key, nil when the key does not parse, and
	// secret are the ranges of der that hold its private numbers; see
	// parsedKey.
	pub    crypto.PublicKey
	secret [][2]int
	// secretRuns are the runs of secretRunBytes bytes of der that hold a
	// byte of a range of secret, built on first use.
	secretRuns map[string]bool
	// publicDER are the DER and wire forms of the key's public key, and
	// public are its encodings as text, empty when the key does not parse;
	// see publicForms.
	publicDER [][]byte
	public    []string
	// certsSeen is the number of the run's certificates that the key was
	// compared with, and certTexts are the texts of those that vouch for
	// it; see vouches.
	certsSeen int
	certTexts []string
}

// derive parses the key the first time, and compares it with each
// certificate in certs that it was not compared with.
func (k *privateKey) derive(certs []*certificate) {
	if !k.parsed {
		k.parsed = true
		if der, err := base64.StdEncoding.DecodeString(k.body); err == nil {
			k.der = der
			if pk, ok := parsePrivateKey(k.label, der); ok {
				k.pub, k.secret = pk.pub, pk.secret
				k.public, k.publicDER = publicForms(pk.pub)
				if h, ok := pkcs8Header(der, pk.pub); ok {
					k.publicDER = append(k.publicDER, h)
				}
			}
		}
	}
	if len(k.publicDER) == 0 {
		k.certsSeen = len(certs)
		return
	}
	for _, c := range certs[k.certsSeen:] {
		if k.vouches(c) {
			k.certTexts = append(k.certTexts, c.texts()...)
		}
	}
	k.certsSeen = len(certs)
}

// vouches reports whether the certificate c holds the public key of k and
// no run of certPrivateBytes bytes of the key that holds a byte of a
// private number of the key; see parsedKey. When the parser did not find
// the private numbers, the certificate must hold no such run that is in no
// encoding of the public key. So a certificate that carries bytes of the
// private key, in an extension or elsewhere, vouches for no part of it.
func (k *privateKey) vouches(c *certificate) bool {
	der, pub := c.parse()
	eq, ok := k.pub.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || pub == nil || !eq.Equal(pub) {
		return false
	}
	for i := 0; i+certPrivateBytes <= len(k.der); i++ {
		w := k.der[i : i+certPrivateBytes]
		private := k.secretAt(i, i+certPrivateBytes)
		if k.secret == nil {
			private = !inAny(k.publicDER, w)
		}
		if private && bytes.Contains(der, w) {
			return false
		}
	}
	return true
}

// runPublic reports whether run, a run of the key text of k, is public. A
// run that is part of a base64 text of an encoding of the public key of k
// is public, whatever the bytes of the key at its place, because the
// public key gives each of its bits; see publicForms. Another run is
// public when it encodes only public bytes at a place where the key text
// holds it. A run is a text, so
// one such place makes it public, also when the key holds it at another
// place too, as an OpenSSH key holds its public key twice. The bytes of the
// key that its characters encode whole must be part of the DER or wire
// form of the public key of k, or of its PKCS #8 header; see pkcs8Header.
// The first and the last character can also encode 2 to 6 bits of a byte
// that the run does not cover whole. Those bits are not compared, because
// they match a public value by coincidence once in 4 to 64 times, but the
// byte must hold no private number of the key; see parsedKey. When the
// parser did not find the private numbers, the byte must be public too.
// The check compares bytes, so a run that encodes the same bytes as an
// encoding of the public key at another bit offset is public too. A key
// that does not parse has no public run.
func (k *privateKey) runPublic(run string) bool {
	if len(k.publicDER) == 0 {
		return false
	}
	if containsIn(k.public, run) {
		return true
	}
	for off := 0; ; {
		i := strings.Index(k.body[off:], run)
		if i < 0 {
			return false
		}
		j := off + i
		lo, hi := (6*j+7)/8, min(6*(j+len(run))/8, len(k.der))
		first, last := 6*j/8, min((6*(j+len(run))+7)/8, len(k.der))
		if k.secret == nil {
			lo, hi = first, last
		}
		if lo < hi && inAny(k.publicDER, k.der[lo:hi]) && !k.secretAt(first, lo) && !k.secretAt(hi, last) {
			return true
		}
		off = j + 1
	}
}

// secretAt reports whether a byte of der in [from, to) holds a private
// number of k.
func (k *privateKey) secretAt(from, to int) bool {
	for _, r := range k.secret {
		if from < r[1] && r[0] < to {
			return true
		}
	}
	return false
}

// secretRunBytes is the length of the runs of bytes of a private key that
// certLeaks looks for in a certificate. A run of 8 bytes of a private
// number matches other bytes by coincidence about once in 2^64.
const secretRunBytes = 8

// holdsSecret reports whether b holds a run of secretRunBytes bytes of the
// key that holds a byte of a private number of the key. It reports false
// when the parser did not find the private numbers.
func (k *privateKey) holdsSecret(b []byte) bool {
	if k.secretRuns == nil {
		k.secretRuns = map[string]bool{}
		for _, r := range k.secret {
			for i := max(0, r[0]-secretRunBytes+1); i < r[1] && i+secretRunBytes <= len(k.der); i++ {
				k.secretRuns[string(k.der[i:i+secretRunBytes])] = true
			}
		}
	}
	for i := 0; i+secretRunBytes <= len(b); i++ {
		if k.secretRuns[string(b[i:i+secretRunBytes])] {
			return true
		}
	}
	return false
}

// pkcs8Header returns the PKCS #8 version and algorithm identifier that
// der, a PKCS #8 private key of pub, starts its body with. Every PKCS #8
// key of the type of pub holds the same bytes, and the PKIX public key
// holds the algorithm identifier. A certificate holds them too when its
// last extension ends with the integer 0, as a path length of 0 does,
// before the signature algorithm.
func pkcs8Header(der []byte, pub crypto.PublicKey) ([]byte, bool) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, false
	}
	var info struct {
		Algorithm asn1.RawValue
		Key       asn1.BitString
	}
	if _, err := asn1.Unmarshal(spki, &info); err != nil {
		return nil, false
	}
	for _, version := range []byte{0, 1} {
		h := append([]byte{0x02, 0x01, version}, info.Algorithm.FullBytes...)
		if i := bytes.Index(der, h); i >= 0 && i <= 4 {
			return h, true
		}
	}
	return nil, false
}

func inAny(ders [][]byte, b []byte) bool {
	for _, d := range ders {
		if bytes.Contains(d, b) {
			return true
		}
	}
	return false
}

// pemBlock is a PEM block inside a text.
type pemBlock struct {
	label string
	// text is the block as the text holds it, from its BEGIN line to the
	// end of its END line, or to the end of the text.
	text string
	// body is the key text of the block between its armor lines.
	body string
}

// pemBlocks returns each PEM block in s whose label passes want: each block
// whose armor line is "-----BEGIN <label>-----". The block runs to the
// matching END line, or to the end of s. The armor is found in s as it
// is, so a block whose line breaks are escaped, as in a JSON document, is
// found too.
func pemBlocks(s string, want func(label string) bool) []pemBlock {
	var blocks []pemBlock
	for {
		i := strings.Index(s, "-----BEGIN ")
		if i < 0 {
			return blocks
		}
		s = s[i:]
		begin := s
		s = s[len("-----BEGIN "):]
		j := strings.Index(s, "-----")
		if j < 0 {
			return blocks
		}
		label := s[:j]
		if !armorLabel(label) || !want(label) {
			continue
		}
		s = s[j+len("-----"):]
		body := s
		endLine := "-----END " + label + "-----"
		if end := strings.Index(s, endLine); end >= 0 {
			body, s = s[:end], s[end+len(endLine):]
		} else {
			s = ""
		}
		blocks = append(blocks, pemBlock{label: label, text: begin[:len(begin)-len(s)], body: keyText(body)})
	}
}

func privateLabel(label string) bool { return strings.HasSuffix(label, "PRIVATE KEY") }

func certificateLabel(label string) bool { return label == "CERTIFICATE" }

// armorLabel reports whether label can be the type of a PEM armor line:
// 1 to 64 bytes of capital letters, digits, and spaces.
func armorLabel(label string) bool {
	if label == "" || len(label) > 64 {
		return false
	}
	for i := range len(label) {
		c := label[i]
		if !('A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == ' ') {
			return false
		}
	}
	return true
}

// keyText returns the base64 characters of s, in order, without PEM armor
// lines and escaped line breaks and tabs. It removes each "-----BEGIN
// <label>-----" and "-----END <label>-----", each backslash, and the
// letter after a run of backslashes when it is n, r, or t, so "\n" and
// "\\n" are removed. Every byte that is not a letter, a digit, "+", "/",
// or "=" is removed too: line breaks, spaces, tabs, quotes, commas, and
// colons. So a private key has the same key text whether its line breaks
// are raw, CRLF, escaped for JSON, or replaced with spaces, and whether its
// lines are indented, as in YAML.
func keyText(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '-':
			i += armorAt(s[i:])
			continue
		case c == '\\':
			for i < len(s) && s[i] == '\\' {
				i++
			}
			if i < len(s) && (s[i] == 'n' || s[i] == 'r' || s[i] == 't') {
				i++
			}
			continue
		case base64Char(c):
			b = append(b, c)
		}
		i++
	}
	return string(b)
}

// armorAt returns the length of the PEM armor line at the start of s,
// without its line break, or 1 when s does not start with one.
func armorAt(s string) int {
	var rest string
	switch {
	case strings.HasPrefix(s, "-----BEGIN "):
		rest = s[len("-----BEGIN "):]
	case strings.HasPrefix(s, "-----END "):
		rest = s[len("-----END "):]
	default:
		return 1
	}
	j := strings.Index(rest, "-----")
	if j < 0 || !armorLabel(rest[:j]) {
		return 1
	}
	return len(s) - len(rest) + j + len("-----")
}

func base64Char(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/' || c == '='
}

// certificate is a PEM certificate that a value of the run holds.
type certificate struct {
	block pemBlock
	// parsed is set once der and pub hold the certificate's DER and public
	// key, pub nil when the certificate does not parse.
	parsed bool
	der    []byte
	pub    crypto.PublicKey
}

// parse returns the DER and the public key of the certificate, and parses
// it the first time.
func (c *certificate) parse() ([]byte, crypto.PublicKey) {
	if !c.parsed {
		c.parsed = true
		if der, err := base64.StdEncoding.DecodeString(c.block.body); err == nil {
			if cert, err := x509.ParseCertificate(der); err == nil {
				c.der, c.pub = der, cert.PublicKey
			}
		}
	}
	return c.der, c.pub
}

// texts returns the certificate as the value holds it, without line
// breaks, and the base64 of its DER from its first, second, and third
// byte; see publicForms.
func (c *certificate) texts() []string {
	texts := []string{form(c.block.text)}
	der, _ := base64.StdEncoding.DecodeString(c.block.body)
	for k := range min(3, len(der)) {
		texts = append(texts, base64.StdEncoding.EncodeToString(der[k:]))
	}
	return texts
}

// parsedKey is what parsePrivateKey reads from a private key.
type parsedKey struct {
	pub crypto.PublicKey
	// secret are the ranges of the key's bytes that hold its private
	// numbers, or its encrypted private section, nil when the parser
	// could not find each of them.
	secret [][2]int
}

// parsePrivateKey parses der, the bytes of a PEM private key of type
// label, and returns its public key. It reads PKCS #1 RSA, PKCS #8 (RSA,
// ECDSA, Ed25519, X25519), SEC 1 EC, and OpenSSH keys (RSA, ECDSA on P-256,
// P-384 and P-521, Ed25519), of at most maxKeyDER bytes. Each parser
// computes or checks the public key from the private values, so the public
// key it returns is the key's own and not text that the PEM only claims.
// An OpenSSH key with a passphrase is the one exception: its private
// values are encrypted, and the public key it returns is the one that the
// key holds in the clear before them.
func parsePrivateKey(label string, der []byte) (parsedKey, bool) {
	if len(der) > maxKeyDER {
		return parsedKey{}, false
	}
	var key any
	var err error
	switch label {
	case "OPENSSH PRIVATE KEY":
		if pub, private, ok := openSSHEncryptedPublic(der); ok {
			return parsedKey{pub: pub, secret: [][2]int{{private, len(der)}}}, true
		}
		key, err = parseOpenSSH(der)
	default:
		if key, err = x509.ParsePKCS8PrivateKey(der); err != nil {
			if key, err = x509.ParsePKCS1PrivateKey(der); err != nil {
				key, err = x509.ParseECPrivateKey(der)
			}
		}
	}
	if err != nil {
		return parsedKey{}, false
	}
	signer, ok := key.(interface{ Public() crypto.PublicKey })
	if !ok {
		return parsedKey{}, false
	}
	return parsedKey{pub: signer.Public(), secret: secretRanges(key, der)}, true
}

// secretRanges returns the ranges of der that hold the private numbers of
// key: D, the primes, and the CRT values of an RSA key, the scalar of an
// ECDSA or X25519 key, and the seed of an Ed25519 key. It returns nil when
// der does not hold D and both primes, or the scalar or seed.
func secretRanges(key any, der []byte) [][2]int {
	var required, optional [][]byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		if len(k.Primes) != 2 {
			return nil
		}
		required = [][]byte{k.D.Bytes(), k.Primes[0].Bytes(), k.Primes[1].Bytes()}
		for _, x := range []*big.Int{k.Precomputed.Dp, k.Precomputed.Dq, k.Precomputed.Qinv} {
			if x != nil {
				optional = append(optional, x.Bytes())
			}
		}
	case *ecdsa.PrivateKey:
		d, err := k.Bytes()
		if err != nil {
			return nil
		}
		// SEC 1 holds the scalar at the size of the curve, and OpenSSH
		// without its leading zero bytes.
		if !bytes.Contains(der, d) {
			d = new(big.Int).SetBytes(d).Bytes()
		}
		required = [][]byte{d}
	case ed25519.PrivateKey:
		required = [][]byte{k.Seed()}
	case *ecdh.PrivateKey:
		required = [][]byte{k.Bytes()}
	default:
		return nil
	}
	var ranges [][2]int
	for i, x := range append(required, optional...) {
		at := bytes.Index(der, x)
		if len(x) == 0 || at < 0 {
			if i < len(required) {
				return nil
			}
			continue
		}
		ranges = append(ranges, [2]int{at, at + len(x)})
	}
	return ranges
}

// publicForms returns the encodings of pub that a stack can hold in the
// clear, as texts and as the bytes they encode. The texts are the PKIX
// PEM, the PKCS #1 PEM of an RSA key, and the OpenSSH authorized_keys line,
// each without line breaks; and the base64 of the DER of each, and of the
// OpenSSH wire form, from its first, second, and third byte. A certificate
// holds the PKIX DER, and a private key holds parts of it, at any offset,
// so the three starts give each base64 text that encodes only bytes of the
// public key. The bytes are the DER of each and the wire form.
func publicForms(pub crypto.PublicKey) ([]string, [][]byte) {
	var ders [][]byte
	var texts []string
	if spki, err := x509.MarshalPKIXPublicKey(pub); err == nil {
		ders = append(ders, spki)
		texts = append(texts, form(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki}))))
	}
	if r, ok := pub.(*rsa.PublicKey); ok {
		der := x509.MarshalPKCS1PublicKey(r)
		ders = append(ders, der)
		texts = append(texts, form(string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: der}))))
	}
	if name, wire, ok := sshWire(pub); ok {
		ders = append(ders, wire)
		texts = append(texts, name+" "+base64.StdEncoding.EncodeToString(wire))
	}
	for _, der := range ders {
		for k := range min(3, len(der)) {
			texts = append(texts, base64.StdEncoding.EncodeToString(der[k:]))
		}
	}
	return texts, ders
}

// sshWire returns the OpenSSH key type name and wire form of pub, as an
// authorized_keys line holds it.
func sshWire(pub crypto.PublicKey) (string, []byte, bool) {
	var w sshWriter
	switch p := pub.(type) {
	case *rsa.PublicKey:
		w.str([]byte("ssh-rsa"))
		w.mpint(big.NewInt(int64(p.E)))
		w.mpint(p.N)
		return "ssh-rsa", w.b, true
	case ed25519.PublicKey:
		w.str([]byte("ssh-ed25519"))
		w.str(p)
		return "ssh-ed25519", w.b, true
	case *ecdsa.PublicKey:
		curve, ok := sshCurve(p.Curve)
		if !ok {
			return "", nil, false
		}
		point, err := p.Bytes()
		if err != nil {
			return "", nil, false
		}
		name := "ecdsa-sha2-" + curve
		w.str([]byte(name))
		w.str([]byte(curve))
		w.str(point)
		return name, w.b, true
	}
	return "", nil, false
}

func sshCurve(c elliptic.Curve) (string, bool) {
	switch c {
	case elliptic.P256():
		return "nistp256", true
	case elliptic.P384():
		return "nistp384", true
	case elliptic.P521():
		return "nistp521", true
	}
	return "", false
}

type sshWriter struct{ b []byte }

func (w *sshWriter) str(s []byte) {
	w.b = binary.BigEndian.AppendUint32(w.b, uint32(len(s)))
	w.b = append(w.b, s...)
}

func (w *sshWriter) mpint(n *big.Int) {
	b := n.Bytes()
	if len(b) > 0 && b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	w.str(b)
}

type sshReader struct {
	b   []byte
	err error
}

var errOpenSSH = errors.New("not an unencrypted OpenSSH private key")

func (r *sshReader) str() []byte {
	if r.err != nil || len(r.b) < 4 {
		r.err = errOpenSSH
		return nil
	}
	n := binary.BigEndian.Uint32(r.b)
	if uint64(n) > uint64(len(r.b)-4) {
		r.err = errOpenSSH
		return nil
	}
	s := r.b[4 : 4+n]
	r.b = r.b[4+n:]
	return s
}

func (r *sshReader) uint32() uint32 {
	if r.err != nil || len(r.b) < 4 {
		r.err = errOpenSSH
		return 0
	}
	n := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return n
}

func (r *sshReader) mpint() *big.Int {
	return new(big.Int).SetBytes(r.str())
}

// openSSHHeader reads the part of an OpenSSH private key, as
// "openssh-key-v1" defines it, that comes before its private section and
// that the key holds in the clear. It returns the cipher, the key
// derivation function, and the public key in wire form of a key that holds
// one key, and a reader at the private section.
func openSSHHeader(der []byte) (cipher, kdf string, pub []byte, r *sshReader, err error) {
	const magic = "openssh-key-v1\x00"
	if !bytes.HasPrefix(der, []byte(magic)) {
		return "", "", nil, nil, errOpenSSH
	}
	r = &sshReader{b: der[len(magic):]}
	c, f := r.str(), r.str()
	r.str() // kdf options
	if n := r.uint32(); n != 1 {
		return "", "", nil, nil, errOpenSSH
	}
	pub = r.str()
	if r.err != nil {
		return "", "", nil, nil, r.err
	}
	return string(c), string(f), pub, r, nil
}

// openSSHEncryptedPublic returns the public key of an OpenSSH private key
// with a passphrase, from the wire form that the key holds in the clear,
// and the offset of the encrypted private section, after its length. It
// returns false for a key without a passphrase, whose public key
// parseOpenSSH computes from the private values.
func openSSHEncryptedPublic(der []byte) (crypto.PublicKey, int, bool) {
	cipher, _, pub, r, err := openSSHHeader(der)
	if err != nil || cipher == "none" || len(r.b) < 4 {
		return nil, 0, false
	}
	k, ok := parseSSHPublic(pub)
	return k, len(der) - len(r.b) + 4, ok
}

// parseSSHPublic parses the OpenSSH wire form of an RSA, ECDSA, or Ed25519
// public key. It refuses an RSA modulus over maxRSABits bits, and a form
// with bytes after the key.
func parseSSHPublic(wire []byte) (crypto.PublicKey, bool) {
	r := &sshReader{b: wire}
	var pub crypto.PublicKey
	switch typ := string(r.str()); typ {
	case "ssh-ed25519":
		k := r.str()
		if len(k) != ed25519.PublicKeySize {
			return nil, false
		}
		pub = ed25519.PublicKey(bytes.Clone(k))
	case "ssh-rsa":
		e, n := r.mpint(), r.mpint()
		if n.Sign() <= 0 || n.BitLen() > maxRSABits || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return nil, false
		}
		pub = &rsa.PublicKey{N: n, E: int(e.Int64())}
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		curve := sshCurves[typ]
		if name := r.str(); "ecdsa-sha2-"+string(name) != typ {
			return nil, false
		}
		k, err := ecdsa.ParseUncompressedPublicKey(curve, r.str())
		if err != nil {
			return nil, false
		}
		pub = k
	default:
		return nil, false
	}
	if r.err != nil || len(r.b) != 0 {
		return nil, false
	}
	return pub, true
}

var sshCurves = map[string]elliptic.Curve{
	"ecdsa-sha2-nistp256": elliptic.P256(),
	"ecdsa-sha2-nistp384": elliptic.P384(),
	"ecdsa-sha2-nistp521": elliptic.P521(),
}

// parseOpenSSH parses an unencrypted OpenSSH private key, as
// "openssh-key-v1" defines it, and returns its private key. It refuses an
// RSA key with a number over maxRSABits bits before it checks the key.
func parseOpenSSH(der []byte) (any, error) {
	cipher, kdf, _, r, err := openSSHHeader(der)
	if err != nil {
		return nil, err
	}
	if cipher != "none" || kdf != "none" {
		return nil, errOpenSSH
	}
	p := &sshReader{b: r.str()}
	if r.err != nil {
		return nil, r.err
	}
	if check := p.uint32(); p.uint32() != check {
		return nil, errOpenSSH
	}
	var key any
	switch typ := string(p.str()); typ {
	case "ssh-ed25519":
		p.str()
		priv := p.str()
		if p.err != nil || len(priv) != ed25519.PrivateKeySize {
			return nil, errOpenSSH
		}
		key = ed25519.NewKeyFromSeed(priv[:ed25519.SeedSize])
	case "ssh-rsa":
		n, e, d := p.mpint(), p.mpint(), p.mpint()
		iqmp := p.mpint()
		pp, q := p.mpint(), p.mpint()
		if p.err != nil || !e.IsInt64() || e.Int64() > 1<<31-1 {
			return nil, errOpenSSH
		}
		for _, x := range []*big.Int{n, d, iqmp, pp, q} {
			if x.BitLen() > maxRSABits {
				return nil, errOpenSSH
			}
		}
		k := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: n, E: int(e.Int64())}, D: d, Primes: []*big.Int{pp, q}}
		if err := k.Validate(); err != nil {
			return nil, err
		}
		k.Precompute()
		key = k
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		curve := sshCurves[typ]
		p.str() // curve name
		p.str() // public point
		d := p.mpint()
		size := (curve.Params().BitSize + 7) / 8
		if p.err != nil || d.BitLen() > size*8 {
			return nil, errOpenSSH
		}
		k, err := ecdsa.ParseRawPrivateKey(curve, d.FillBytes(make([]byte, size)))
		if err != nil {
			return nil, err
		}
		key = k
	default:
		return nil, errOpenSSH
	}
	if p.err != nil {
		return nil, p.err
	}
	return key, nil
}
