package sqldb

import "testing"

func TestRebind(t *testing.T) {
	tests := []struct {
		name, in, want string
		pg             bool
	}{
		{"sqlite keeps the statement", "SELECT a FROM t WHERE b = ? AND c = '?'", "SELECT a FROM t WHERE b = ? AND c = '?'", false},
		{"numbers in order", "UPDATE t SET a = ?, b = ? WHERE c = ?", "UPDATE t SET a = $1, b = $2 WHERE c = $3", true},
		{"a ? in a string literal", "SELECT a FROM t WHERE b = '?' AND c = ?", "SELECT a FROM t WHERE b = '?' AND c = $1", true},
		{"a doubled quote in a literal", "SELECT 'it''s ?' , ?", "SELECT 'it''s ?' , $1", true},
		{"a ? in a quoted identifier", `SELECT "a?b" FROM t WHERE c = ?`, `SELECT "a?b" FROM t WHERE c = $1`, true},
		{"a doubled quote in an identifier", `SELECT "a""?" , ?`, `SELECT "a""?" , $1`, true},
		{"a line comment", "SELECT ? -- why?\n, ?", "SELECT $1 -- why?\n, $2", true},
		{"a block comment", "SELECT ? /* why? */ , ?", "SELECT $1 /* why? */ , $2", true},
		{"an unclosed literal keeps its text", "SELECT ?, 'a?", "SELECT $1, 'a?", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Rebind(tt.in, tt.pg); got != tt.want {
				t.Errorf("Rebind(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
