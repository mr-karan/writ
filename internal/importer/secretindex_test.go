package importer

import (
	"encoding/base64"
	"errors"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// randomSecrets returns count secrets of size bytes of base64 text each.
func randomSecrets(t *testing.T, count, size int) []value.Value {
	t.Helper()
	rng := rand.New(rand.NewChaCha8([32]byte{7}))
	vals := make([]value.Value, count)
	for i := range vals {
		raw := make([]byte, size*3/4)
		for j := range raw {
			raw[j] = byte(rng.Uint32())
		}
		vals[i] = value.StringVal(base64.StdEncoding.EncodeToString(raw)).Secret()
	}
	return vals
}

// The index of the secrets takes a few bytes of memory for each byte of
// secret. An index of every 8-byte run of every secret took 315 MiB for
// one secret of 1 MiB, and kept 112 MiB.
func TestSecretIndexMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("measures memory and time of a 1 MiB index")
	}
	for _, tc := range []struct {
		name        string
		count, size int
	}{
		{"one secret of 1 MiB", 1, 1 << 20},
		{"1000 secrets of 1700 bytes", 1000, 1700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vals := randomSecrets(t, tc.count, tc.size)
			last := vals[len(vals)-1].AsString()
			part := last[len(last)/2 : len(last)/2+16]
			const maxAlloc, maxLive = 48 << 20, 16 << 20

			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			s := Known{Secrets: vals}.set()
			_, found := s.within(part, nil)
			elapsed := time.Since(start)
			runtime.GC()
			runtime.ReadMemStats(&after)
			alloc := after.TotalAlloc - before.TotalAlloc
			live := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			t.Logf("elapsed=%s allocated=%d live=%d", elapsed, alloc, live)
			if !found {
				t.Fatal("a 16-byte part of a secret is not found")
			}
			if alloc > maxAlloc || live > maxLive {
				t.Errorf("the index allocated %d bytes and keeps %d, want at most %d and %d", alloc, live, maxAlloc, maxLive)
			}
			// A lookup must not grow with the bytes of secret. Wall-clock
			// limits fail on a loaded machine, so the lookups run interleaved
			// against this index and against one of a 64-byte secret, and
			// the test bounds their ratio. Measured under load: the index
			// is about 10 times slower, and a scan of 1 MiB about 200.
			tiny := Known{Secrets: randomSecrets(t, 1, 64)}.set()
			tinyText := randomSecrets(t, 1, 64)[0].AsString()
			var big, small time.Duration
			const lookups = 10000
			for i := range lookups {
				x := last[i%(len(last)-24) : i%(len(last)-24)+24]
				start := time.Now()
				_, inBig := s.within(x, nil)
				_, outBig := s.within(strings.ToLower(x)+"!", nil)
				big += time.Since(start)
				if !inBig {
					t.Fatal("a 24-byte part of a secret is not found")
				}
				if outBig {
					t.Fatal("a value that is part of no secret is found")
				}
				y := tinyText[i%40 : i%40+24]
				start = time.Now()
				tiny.within(y, nil)
				tiny.within(strings.ToLower(y)+"!", nil)
				small += time.Since(start)
			}
			t.Logf("lookup big=%s small=%s", big/(2*lookups), small/(2*lookups))
			if big > 50*small {
				t.Errorf("a lookup in %d bytes of secret took %s, %0.f times one in 64 bytes; want at most 50", tc.count*tc.size, big/(2*lookups), float64(big)/float64(small))
			}
			runtime.KeepAlive(s)
			runtime.KeepAlive(tiny)
		})
	}
}

// An import that knows more secret bytes than maxSecretBytes is refused
// before it builds the index.
func TestSecretIndexBudget(t *testing.T) {
	vals := []value.Value{value.StringVal(strings.Repeat("s3cr3t-b", maxSecretBytes/8+1)).Secret()}
	res := &schema.Resource{Block: schema.Block{Attributes: []schema.Attribute{{Name: "result", Type: types.TString}}}}
	read := value.ObjectVal(map[string]value.Value{"result": value.StringVal("an unrelated public value")})

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := check(Known{Secrets: vals}, res, "unrelated-import-id", read)
	runtime.ReadMemStats(&after)
	if limit, ok := errors.AsType[*LimitError](err); !ok || limit.What != "secret bytes" || limit.Limit != maxSecretBytes {
		t.Fatalf("Check of more than %d secret bytes: %v, want a LimitError of secret bytes", maxSecretBytes, err)
	}
	if !strings.Contains(err.Error(), "more than the limit of 67108864") {
		t.Errorf("the refusal does not state the limit: %v", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
		t.Errorf("the refusal allocated %d bytes, want at most 1 MiB", alloc)
	}
}

// A value at more places in the secrets than lookupLimit is compared with
// each secret in turn, and is still found, with its owner.
func TestSecretIndexRepeats(t *testing.T) {
	s := newSecretSet()
	own, other := &node{}, &node{}
	s.keep(own, strings.Repeat("a", 4096), true)
	s.keep(other, strings.Repeat("a", 2048)+"tail-of-it", true)
	if err := s.ready(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		x               string
		wantOwn, wantOt bool
	}{
		{strings.Repeat("a", 8), true, true},
		{strings.Repeat("a", 16) + "tail", false, true},
		{strings.Repeat("a", 3000), true, false},
		{strings.Repeat("b", 8), false, false},
	} {
		if gotOwn, gotOt := s.within(tc.x, own); gotOwn != tc.wantOwn || gotOt != tc.wantOt {
			t.Errorf("within(%.12q...) = %v, %v, want %v, %v", tc.x, gotOwn, gotOt, tc.wantOwn, tc.wantOt)
		}
	}
}

// A value that holds a line break is not found across the end of one
// secret and the start of the next, which the index keeps a line break
// apart.
func TestSecretIndexSeparator(t *testing.T) {
	s := newSecretSet()
	s.keep(nil, "secret-left-ABCD", true)
	s.keep(nil, "EFGH-secret-right", true)
	if err := s.ready(); err != nil {
		t.Fatal(err)
	}
	for _, x := range []string{"ABCDEFGH", "ABCD\nEFGH", "ABCD\r\nEFGH"} {
		if _, other := s.within(x, nil); other {
			t.Errorf("within(%q) found a secret across the end of another", x)
		}
	}
	ix := newTextIndex([]string{"secret-left-ABCD", "EFGH-secret-right"})
	ix.within("ABCD\nEFGH", func(text string) bool {
		t.Errorf("the index found %q across the end of %q", "ABCD\nEFGH", text)
		return true
	})
}

// A secret of 1 MiB that has the armor of a private key is indexed twice:
// by its form and by its key text. Each index takes about 5 bytes of
// memory for each byte. A value of 1 MiB is compared by each run of
// minKeyText characters of its key text.
func TestKeyIndexMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("measures memory and time of a 1 MiB private key")
	}
	body := randomSecrets(t, 1, 1<<20)[0].AsString()
	key := testPEM("RSA PRIVATE KEY", body)
	res := &schema.Resource{Block: schema.Block{Attributes: []schema.Attribute{{Name: "result", Type: types.TString}}}}
	// The value and the key come from one stream of random bytes, so the
	// value is in lower case to differ from the key.
	unrelated := strings.ToLower(randomSecrets(t, 2, 1<<20)[1].AsString())
	for _, tc := range []struct {
		name, text string
		refused    bool
	}{
		{"a short value", "an unrelated public value", false},
		{"a value of 1 MiB that shares no run with the key", unrelated, false},
		{"a value of 1 MiB that ends with a part of the key", unrelated[:len(unrelated)-40] + body[len(body)/2:len(body)/2+40], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := value.ObjectVal(map[string]value.Value{"result": value.StringVal(tc.text)})
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			err := check(Known{Secrets: []value.Value{value.StringVal(key).Secret()}}, res, "unrelated-import-id", read)
			elapsed := time.Since(start)
			runtime.GC()
			runtime.ReadMemStats(&after)
			alloc := after.TotalAlloc - before.TotalAlloc
			t.Logf("elapsed=%s allocated=%d", elapsed, alloc)
			if (err != nil) != tc.refused {
				t.Errorf("Check: %v, want refused %v", err, tc.refused)
			}
			const maxAlloc = 64 << 20
			if alloc > maxAlloc {
				t.Errorf("the check allocated %d bytes, want at most %d", alloc, maxAlloc)
			}
			// A check that compares the 1 MiB value with the 1 MiB key run
			// by run takes hours. The bound is loose because a loaded
			// machine made the linear check take over a minute.
			if elapsed > 2*time.Minute {
				t.Errorf("the check took %s, want at most 2m", elapsed)
			}
		})
	}
}
