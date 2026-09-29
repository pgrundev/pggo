package pggo

import (
	"testing"
)

func TestCommandTag(t *testing.T) {
	cases := []struct {
		tag  string
		cmd  string
		rows int64
	}{
		{"INSERT 0 3", "INSERT", 3},
		{"UPDATE 1", "UPDATE", 1},
		{"DELETE 0", "DELETE", 0},
		{"SELECT 5", "SELECT", 5},
		{"CREATE TABLE", "CREATE TABLE", 0},
		{"MERGE 2", "MERGE", 2},
	}
	for _, c := range cases {
		tag := CommandTag(c.tag)
		if cmd, n := tag.Command(), tag.RowsAffected(); cmd != c.cmd || n != c.rows {
			t.Errorf("CommandTag(%q) = %q, %d", c.tag, cmd, n)
		}
	}
}

func FuzzCommandTag(f *testing.F) {
	for _, s := range []string{"INSERT 0 1", "", " ", "1 2 3", "SELECT 99999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { CommandTag(s).Command(); CommandTag(s).RowsAffected() })
}

func FuzzParseRowDescription(f *testing.F) {
	f.Add(rowDescription(Column{"id", 23}, Column{"x", 25}))
	f.Add([]byte{0, 5, 'a'})
	f.Fuzz(func(t *testing.T, b []byte) { parseRowDescription(b) })
}

func FuzzParseError(f *testing.F) {
	f.Add([]byte("SERROR\x00C42P01\x00Mmissing\x00P15\x00\x00"))
	f.Add([]byte("C"))
	f.Fuzz(func(t *testing.T, b []byte) { parseError(b) })
}
