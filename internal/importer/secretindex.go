package importer

import (
	"bytes"
	"index/suffixarray"
	"sort"
)

// lookupLimit is the most places of one value in the texts that a lookup
// reads from the suffix array. A value at more places, such as a run of one
// byte inside a text that repeats it, is compared with each text in turn
// instead, so a lookup never allocates more than this many places.
const lookupLimit = 256

// textIndex finds a value inside a set of texts. It keeps the texts in one
// buffer, each followed by a line break, and a suffix array of the buffer:
// about 5 bytes for each byte of text. It compares bytes as they are: it
// removes no line break, PEM armor, or other part of a text or a value.
type textIndex struct {
	sa *suffixarray.Index
	// starts holds, in increasing order, where each text starts in the
	// buffer, and ends where it ends.
	starts, ends []int
	// texts are the texts, in the order of starts.
	texts []string
}

func newTextIndex(texts []string) *textIndex {
	size := 0
	for _, s := range texts {
		size += len(s) + 1
	}
	ix := &textIndex{texts: texts}
	buf := make([]byte, 0, size)
	for _, s := range texts {
		ix.starts = append(ix.starts, len(buf))
		buf = append(buf, s...)
		ix.ends = append(ix.ends, len(buf))
		buf = append(buf, '\n')
	}
	ix.sa = suffixarray.New(buf)
	return ix
}

// within calls yield with each text that x is part of, until yield returns
// true. It can call yield more than once with one text.
func (ix *textIndex) within(x string, yield func(text string) bool) {
	if x == "" {
		return
	}
	q := []byte(x)
	places := ix.sa.Lookup(q, lookupLimit+1)
	if len(places) > lookupLimit {
		buf := ix.sa.Bytes()
		for i := range ix.starts {
			if bytes.Contains(buf[ix.starts[i]:ix.ends[i]], q) && yield(ix.texts[i]) {
				return
			}
		}
		return
	}
	for _, p := range places {
		i := sort.Search(len(ix.starts), func(i int) bool { return ix.starts[i] > p }) - 1
		// x can hold a line break, so a place can cross the end of a
		// text into the next one.
		if p+len(q) > ix.ends[i] {
			continue
		}
		if yield(ix.texts[i]) {
			return
		}
	}
}
