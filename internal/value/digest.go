package value

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
)

// Digest is an HMAC-SHA256 of a value's canonical encoding. The state package
// owns the key, so a digest of a config that holds a low-entropy secret cannot
// be brute-forced from state.db or a plan file alone.
type Digest [32]byte

// Hasher computes digests under one key. state.Store hands one out.
type Hasher interface {
	Sum(parts ...[]byte) Digest
}

// NewHasher returns the Hasher that computes HMAC-SHA256 under key. Each part
// is prefixed with its length, so ("ab", "c") and ("a", "bc") never collide.
// It panics on an empty key: an unkeyed digest of a secret can be reversed by
// hashing guesses.
func NewHasher(key []byte) Hasher {
	if len(key) == 0 {
		panic("value: NewHasher needs a key")
	}
	return hmacHasher{key: slices.Clone(key)}
}

type hmacHasher struct{ key []byte }

func (h hmacHasher) Sum(parts ...[]byte) Digest {
	mac := hmac.New(sha256.New, h.key)
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		mac.Write(n[:])
		mac.Write(p)
	}
	var d Digest
	mac.Sum(d[:0])
	return d
}

// Tags of the canonical encoding. They are fixed numbers, not Kind values,
// so reordering Kind cannot change every digest in every state file.
const (
	tagNull   byte = 1
	tagBool   byte = 2
	tagInt    byte = 3
	tagFloat  byte = 4
	tagString byte = 5
	tagList   byte = 6
	tagObject byte = 7
	tagSecret byte = 0x80
)

// Canonical encodes a wholly known value deterministically: a kind tag, then
// the payload; objects in sorted key order; floats in their shortest exact
// form; the secret mark included. It fails on Unknown, so a digest always
// describes a concrete config.
//
// A mark on a container covers its contents, so the encoding records the
// mark on each leaf and empty container it covers, never on a non-empty
// container. Two values encode the same exactly when Equal holds.
func Canonical(v Value) ([]byte, error) {
	if b, ok := appendCanonical(nil, v, false); ok {
		return b, nil
	}
	var err error
	Walk(v, func(p Path, leaf Value) {
		if err == nil && leaf.kind == Unknown {
			err = fmt.Errorf("cannot encode an unknown value at %s", describePath(p))
		}
	})
	return nil, err
}

func describePath(p Path) string {
	if len(p) == 0 {
		return "the top level"
	}
	return p.String()
}

func appendCanonical(b []byte, v Value, secret bool) ([]byte, bool) {
	secret = secret || v.secret
	mark := func(tag byte) byte {
		if secret {
			return tag | tagSecret
		}
		return tag
	}
	switch v.kind {
	case Null:
		return append(b, mark(tagNull)), true
	case Bool:
		var x byte
		if v.b {
			x = 1
		}
		return append(b, mark(tagBool), x), true
	case Int:
		return binary.BigEndian.AppendUint64(append(b, mark(tagInt)), uint64(v.i)), true
	case Float:
		return appendBytes(append(b, mark(tagFloat)), strconv.FormatFloat(v.f, 'g', -1, 64)), true
	case String:
		return appendBytes(append(b, mark(tagString)), v.s), true
	case List, Object:
		tag := tagList
		if v.kind == Object {
			tag = tagObject
		}
		if len(v.elems) == 0 {
			tag = mark(tag)
		}
		b = binary.AppendUvarint(append(b, tag), uint64(len(v.elems)))
		for i, e := range v.elems {
			if v.kind == Object {
				b = appendBytes(b, v.keys[i])
			}
			var ok bool
			if b, ok = appendCanonical(b, e, secret); !ok {
				return nil, false
			}
		}
		return b, true
	}
	return nil, false
}

func appendBytes(b []byte, s string) []byte {
	return append(binary.AppendUvarint(b, uint64(len(s))), s...)
}
