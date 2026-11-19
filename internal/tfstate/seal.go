package tfstate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// KeySize is the size of an environment's state key file.
const KeySize = 32

// Key seals the states, the saved plans, and the index secrets of one
// environment: AES-256-GCM
// under keys that HKDF-SHA256 derives from the environment's state key
// file. Each has its own label, which also differs from the one the
// state of the old Writ language derived its seal key with, so no sealed
// value of one kind opens as a sealed value of another.
type Key struct {
	aead    cipher.AEAD
	plan    cipher.AEAD
	secrets cipher.AEAD
}

// NewKey derives a Key from the 32 bytes of an environment's state key.
func NewKey(secret []byte) (*Key, error) {
	if len(secret) != KeySize {
		return nil, fmt.Errorf("a state key is %d bytes, not %d", KeySize, len(secret))
	}
	state, err := derive(secret, "writ tfstate seal v1")
	if err != nil {
		return nil, err
	}
	plan, err := derive(secret, "writ tofu saved plan seal v1")
	if err != nil {
		return nil, err
	}
	secrets, err := derive(secret, "writ tfstate index secrets seal v1")
	if err != nil {
		return nil, err
	}
	return &Key{aead: state, plan: plan, secrets: secrets}, nil
}

func derive(secret []byte, label string) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, secret, nil, label, KeySize)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ReadKey reads an environment's state key file. A file of the wrong size
// is an error, never a reason to make a new key: a new key orphans every
// sealed state.
func ReadKey(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != KeySize {
		return nil, fmt.Errorf("state key %s is %d bytes, want %d: the file is damaged; restore it from a backup", path, len(b), KeySize)
	}
	return NewKey(b)
}

// OpenKey reads an environment's state key file, as ReadKey does, and
// makes the file with a new key when it does not exist. Two processes that make it at
// once share one key: each writes a temporary file and links it into
// place, and the one that loses reads the winner's.
func OpenKey(path string) (*Key, error) {
	k, err := ReadKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return k, err
	}
	secret := make([]byte, KeySize)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state.key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(secret)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := os.Link(f.Name(), path); errors.Is(err, fs.ErrExist) {
		return ReadKey(path)
	} else if err != nil {
		return nil, err
	}
	return NewKey(secret)
}

// SealPlan encrypts a saved plan with a fresh nonce. The plan's id, its
// stack, and its environment are the associated data, so a sealed plan
// copied to another plan's row does not open.
func (k *Key) SealPlan(saved []byte, id, env, stack string) []byte {
	return sealWith(k.plan, saved, planSealedFor(id, env, stack))
}

// OpenPlan decrypts a saved plan that SealPlan sealed for the same id,
// stack, and environment.
func (k *Key) OpenPlan(sealed []byte, id, env, stack string) ([]byte, error) {
	n := k.plan.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed saved plan is too short")
	}
	plain, err := k.plan.Open(nil, sealed[:n], sealed[n:], planSealedFor(id, env, stack))
	if err != nil {
		return nil, fmt.Errorf("cannot open the saved plan of plan %s of %s@%s: the key, the plan, the stack, or the environment is not the one it was sealed for", id, stack, env)
	}
	return plain, nil
}

func planSealedFor(id, env, stack string) []byte {
	return fmt.Appendf(nil, "writ tofu saved plan v1\x00%s\x00%s\x00%s", id, env, stack)
}

func sealWith(aead cipher.AEAD, plain, data []byte) []byte {
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // crypto/rand.Read never fails on supported platforms
	}
	return aead.Seal(nonce, nonce, plain, data)
}

// seal encrypts plain with a fresh nonce. The stack@env and the version
// number are the associated data, so a sealed state copied to another
// stack's row, or to another version's row, does not open.
func (k *Key) seal(plain []byte, env, stack string, version int64) []byte {
	return sealWith(k.aead, plain, sealedFor(env, stack, version))
}

// OpenState decrypts one stored version of a stack@env's state, which
// holds every value the state holds: only a run may see it, as tofu reads
// it from the backend.
func (k *Key) OpenState(sealed []byte, env, stack string, version int64) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed state is too short")
	}
	plain, err := k.aead.Open(nil, sealed[:n], sealed[n:], sealedFor(env, stack, version))
	if err != nil {
		return nil, fmt.Errorf("cannot unseal version %d of the state of %s@%s: the key, the stack, or the version is not the one it was sealed for", version, stack, env)
	}
	return plain, nil
}

func sealedFor(env, stack string, version int64) []byte {
	return fmt.Appendf(nil, "writ tfstate v1\x00%s\x00%s\x00%d", env, stack, version)
}

// SealIndexSecrets encrypts the secrets that the mask of the apply which
// wrote one version of a stack@env's state knew, which a refresh of the
// state index from that version needs. The stack@env and the version
// number are the associated data, as a sealed state's are.
func (k *Key) SealIndexSecrets(secrets []string, env, stack string, version int64) []byte {
	var plain []byte
	for _, s := range secrets {
		plain = binary.AppendUvarint(plain, uint64(len(s)))
		plain = append(plain, s...)
	}
	return sealWith(k.secrets, plain, indexSecretsFor(env, stack, version))
}

// OpenIndexSecrets decrypts what SealIndexSecrets sealed for the same
// version of the same stack@env.
func (k *Key) OpenIndexSecrets(sealed []byte, env, stack string, version int64) ([]string, error) {
	n := k.secrets.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed index secrets are too short")
	}
	plain, err := k.secrets.Open(nil, sealed[:n], sealed[n:], indexSecretsFor(env, stack, version))
	if err != nil {
		return nil, fmt.Errorf("cannot unseal the index secrets of version %d of the state of %s@%s: the key, the stack, or the version is not the one they were sealed for", version, stack, env)
	}
	var out []string
	for len(plain) > 0 {
		size, w := binary.Uvarint(plain)
		if w <= 0 || size > uint64(len(plain)-w) {
			return nil, fmt.Errorf("the index secrets of version %d of the state of %s@%s are damaged", version, stack, env)
		}
		out = append(out, string(plain[w:w+int(size)]))
		plain = plain[w+int(size):]
	}
	return out, nil
}

func indexSecretsFor(env, stack string, version int64) []byte {
	return fmt.Appendf(nil, "writ tfstate index secrets v1\x00%s\x00%s\x00%d", env, stack, version)
}
