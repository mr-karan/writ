package mask

import (
	"slices"
	"strings"
)

// automaton finds, in one pass over a text, every place where one of a set
// of patterns ends (Aho-Corasick). Its nodes are the distinct prefixes of
// the patterns, numbered breadth first and, within a depth, in sorted
// order, so the children of node u are the nodes child[u] to
// child[u+1]-1, sorted by label.
type automaton struct {
	child   []int32
	label   []byte
	fail    []int32
	longest []int32    // the length of the longest pattern that is a suffix of the node's prefix
	root    [256]int32 // the root's transitions, 0 for none, since most bytes of a text are read there
}

// fold maps the bytes the sweep treats as one: a query string writes a
// space as '+'.
func fold(c byte) byte {
	if c == '+' {
		return ' '
	}
	return c
}

func foldString(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '+' {
			return ' '
		}
		return r
	}, s)
}

func newAutomaton(patterns []string) *automaton {
	ps := make([]string, len(patterns))
	for i, p := range patterns {
		ps[i] = foldString(p)
	}
	slices.Sort(ps)
	ps = slices.Compact(ps)

	a := &automaton{label: []byte{0}, longest: []int32{0}}
	parent := []int32{0}
	cur := make([]int32, len(ps))
	active := make([]int, 0, len(ps))
	for i, p := range ps {
		if p != "" {
			active = append(active, i)
		}
	}
	// Patterns sorted, a depth's distinct prefixes come in order of their
	// parent and then their last byte, which is breadth-first order.
	for d := 0; len(active) > 0; d++ {
		next := active[:0]
		lastParent, lastLabel, node := int32(-1), -1, int32(0)
		for _, i := range active {
			p, c := ps[i], int(ps[i][d])
			if cur[i] != lastParent || c != lastLabel {
				node = int32(len(a.label))
				a.label = append(a.label, byte(c))
				a.longest = append(a.longest, 0)
				parent = append(parent, cur[i])
				lastParent, lastLabel = cur[i], c
			}
			cur[i] = node
			if len(p) == d+1 {
				a.longest[node] = int32(d + 1)
			} else {
				next = append(next, i)
			}
		}
		active = next
	}

	n := len(a.label)
	a.child = make([]int32, n+1)
	for v := 1; v < n; v++ {
		a.child[parent[v]+1]++
	}
	a.child[0] = 1
	for u := 1; u <= n; u++ {
		a.child[u] += a.child[u-1]
	}
	for v := a.child[0]; v < a.child[1]; v++ {
		a.root[a.label[v]] = v
	}
	a.fail = make([]int32, n)
	for v := 1; v < n; v++ {
		if p := parent[v]; p != 0 {
			f := a.fail[p]
			for {
				if t := a.step(f, a.label[v]); t >= 0 {
					a.fail[v] = t
					break
				}
				if f == 0 {
					break
				}
				f = a.fail[f]
			}
		}
		a.longest[v] = max(a.longest[v], a.longest[a.fail[v]])
	}
	return a
}

// step returns the child of u labelled c, or -1.
func (a *automaton) step(u int32, c byte) int32 {
	if u == 0 {
		if t := a.root[c]; t != 0 {
			return t
		}
		return -1
	}
	lo, hi := a.child[u], a.child[u+1]
	for lo < hi {
		m := int32(uint32(lo+hi) >> 1)
		switch l := a.label[m]; {
		case l == c:
			return m
		case l < c:
			lo = m + 1
		default:
			hi = m
		}
	}
	return -1
}

// find appends the span of the longest pattern that ends at each offset of
// text where one ends. A shorter pattern that ends there lies inside it.
func find[T string | []byte](a *automaton, text T, spans [][2]int) [][2]int {
	u := int32(0)
	for i := range len(text) {
		c := fold(text[i])
		for {
			if t := a.step(u, c); t >= 0 {
				u = t
				break
			}
			if u == 0 {
				break
			}
			u = a.fail[u]
		}
		if n := a.longest[u]; n > 0 {
			spans = append(spans, [2]int{i + 1 - int(n), i + 1})
		}
	}
	return spans
}
