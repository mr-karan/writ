package diag

import "testing"

func TestListSortsByPositionThenClass(t *testing.T) {
	at := func(file string, line, col int) Span {
		return Span{File: file, Start: Pos{Line: line, Col: col}}
	}
	var l List
	l.Errorf(Type, at("b.writ", 1, 1), "b")
	l.Warnf(Name, at("a.writ", 3, 2), "a3")
	l.Errorf(Type, at("a.writ", 3, 2), "a3-type")
	l.Errorf(Syntax, at("a.writ", 1, 9), "a1")
	l.Sort()

	var got []string
	for _, d := range l {
		got = append(got, d.Message)
	}
	want := []string{"a1", "a3", "a3-type", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestHasErrorsIgnoresWarnings(t *testing.T) {
	var l List
	l.Warnf(Goal, Span{}, "vacuous goal")
	if l.HasErrors() {
		t.Fatal("a warning alone must not count as an error")
	}
	l.Errorf(Type, Span{}, "mismatch %d", 1)
	if !l.HasErrors() {
		t.Fatal("an error must count")
	}
	if l[1].Message != "mismatch 1" {
		t.Fatalf("Errorf formats its message, got %q", l[1].Message)
	}
}
