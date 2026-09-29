package main

import (
	"testing"
)

func TestMaxPlaceholder(t *testing.T) {
	cases := map[string]int{
		"SELECT 1":                         0,
		"SELECT $1":                        1,
		"SELECT $2, $1, $10":               10,
		"SELECT '$1'":                      0,
		"SELECT 'it''s $1'":                0,
		`SELECT E'\'$1'`:                   0,
		`SELECT "$1"`:                      0,
		"SELECT 1 -- $1\n":                 0,
		"SELECT 1 /* $1 /* $2 */ $3 */":    0,
		"SELECT $$ $1 $$":                  0,
		"SELECT $tag$ $1 $tag$ || $2":      2,
		"SELECT '$9' || $1 /* $8 */ -- $7": 1,
		"CREATE FUNCTION f() RETURNS int AS $$ SELECT $1 $$ LANGUAGE sql": 0,
	}
	for sql, want := range cases {
		if got := MaxPlaceholder(sql); got != want {
			t.Errorf("MaxPlaceholder(%q) = %d, want %d", sql, got, want)
		}
	}
}

func FuzzMaxPlaceholder(f *testing.F) {
	for _, s := range []string{"SELECT $1", "'$1'", "$$ $1 $$", "$a$ $1", "/* /* */", "E'\\'", `"$1`, "$99999999999", "--", "$"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if n := MaxPlaceholder(s); n < 0 || n > 65535 {
			t.Fatalf("MaxPlaceholder(%q) = %d", s, n)
		}
	})
}
