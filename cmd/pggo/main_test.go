package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	perr "github.com/pgrundev/pggo/internal/errors"
	"github.com/pgrundev/pggo/internal/postgres"
)

const url = "postgres://u:p@h/db"

func mustParse(t *testing.T, args ...string) *options {
	t.Helper()
	o, err := parseArgs(args)
	if err != nil {
		t.Fatalf("parseArgs(%q): %v", args, err)
	}
	return o
}

func mustFail(t *testing.T, args ...string) *perr.Error {
	t.Helper()
	_, err := parseArgs(args)
	if err == nil {
		t.Fatalf("parseArgs(%q) should fail", args)
	}
	e := perr.Classify(err)
	if e.Type != perr.InvalidInput {
		t.Fatalf("parseArgs(%q) type = %s, want invalid_input", args, e.Type)
	}
	return e
}

func strs(p []*string) []any {
	out := make([]any, len(p))
	for i, v := range p {
		if v != nil {
			out[i] = *v
		}
	}
	return out
}

func TestDefaults(t *testing.T) {
	o := mustParse(t, "query", url, "SELECT 1")
	if o.timeout != 10*time.Second || o.maxRows != 100 || o.maxBytes != 64*1024 || o.iterations != 10 {
		t.Fatalf("%+v", o)
	}
}

func TestPositionals(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env/db")
	o := mustParse(t, "query", url, "SELECT 1")
	if o.url != url || o.sql != "SELECT 1" {
		t.Fatalf("%+v", o)
	}
	o = mustParse(t, "query", "SELECT 1")
	if o.url != "postgres://env/db" || o.sql != "SELECT 1" {
		t.Fatalf("env fallback: %+v", o)
	}
	o = mustParse(t, "ping")
	if o.url != "postgres://env/db" {
		t.Fatalf("ping env fallback: %+v", o)
	}
	o = mustParse(t, "ping", url)
	if o.url != url {
		t.Fatalf("%+v", o)
	}
	// Flags may appear anywhere.
	o = mustParse(t, "exec", "--param", "1", url, "--timeout=2s", "DELETE FROM t WHERE id = $1")
	if o.url != url || o.sql != "DELETE FROM t WHERE id = $1" || o.timeout != 2*time.Second || len(o.params) != 1 {
		t.Fatalf("%+v", o)
	}
}

func TestDoubleDashAllowsSQLStartingWithDashes(t *testing.T) {
	o := mustParse(t, "query", url, "--", "-- comment\nSELECT 1")
	if o.sql != "-- comment\nSELECT 1" {
		t.Fatalf("%q", o.sql)
	}
}

func TestSQLStartingWithComment(t *testing.T) {
	o := mustParse(t, "query", url, "-- find users\nSELECT 1")
	if o.sql != "-- find users\nSELECT 1" {
		t.Fatalf("%q", o.sql)
	}
	o = mustParse(t, "exec", "--timeout", "1s", "--comment\nDELETE FROM t")
	if o.sql != "--comment\nDELETE FROM t" {
		t.Fatalf("%q", o.sql)
	}
	mustFail(t, "query", url, "SELECT 1", "--bogus")
}

func TestParams(t *testing.T) {
	o := mustParse(t, "query", url, "SELECT $1, $2, $3", "--param", "a b", "--param=", "--param", "--not-a-flag")
	if got := strs(o.params); len(got) != 3 || got[0] != "a b" || got[1] != "" || got[2] != "--not-a-flag" {
		t.Fatalf("%v", got)
	}
	o = mustParse(t, "query", url, "SELECT $1, $2, $3, $4, $5, $6", "--params", `[42, "s", null, true, {"b": [1, 2]}, 1.5e3]`)
	want := []any{"42", "s", nil, "true", `{"b":[1,2]}`, "1.5e3"}
	got := strs(o.params)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("param %d = %#v, want %#v", i, got[i], want[i])
		}
	}
	o = mustParse(t, "query", url, "SELECT $1", "--params", `["quote \" and \\u00e9 and \n"]`)
	if *o.params[0] != "quote \" and \\u00e9 and \n" {
		t.Fatalf("%q", *o.params[0])
	}
	o = mustParse(t, "query", url, "SELECT 1", "--params", `[]`)
	if len(o.params) != 0 {
		t.Fatal("empty params")
	}
}

func TestPlaceholderValidation(t *testing.T) {
	e := mustFail(t, "query", url, "SELECT id FROM t WHERE id = ", "--param", "42")
	if !strings.Contains(e.Hint, "single quotes") || !strings.Contains(e.Hint, "$1") {
		t.Fatalf("hint: %q", e.Hint)
	}
	e = mustFail(t, "query", url, "SELECT $1")
	if !strings.Contains(e.Hint, "--param") {
		t.Fatalf("hint: %q", e.Hint)
	}
	mustFail(t, "query", url, "SELECT $1, $3", "--param", "1", "--param", "2")
	mustParse(t, "query", url, "SELECT $1, $3, $2", "--param", "1", "--param", "2", "--param", "3")
	mustParse(t, "query", url, "SELECT $1, $1", "--param", "1")
}

func TestInvalidArgs(t *testing.T) {
	cases := [][]string{
		{"frobnicate"},
		{"SELECT 1"},
		{"query"},
		{"query", url},
		{"query", url, "SELECT 1", "extra"},
		{"query", url, "  \n "},
		{"query", url, "SELECT 1", "--bogus", "1"},
		{"query", url, "SELECT 1", "--timeout"},
		{"query", url, "SELECT 1", "--timeout", "soon"},
		{"query", url, "SELECT 1", "--timeout", "0"},
		{"query", url, "SELECT 1", "--timeout", "-1s"},
		{"query", url, "SELECT 1", "--max-rows", "0"},
		{"query", url, "SELECT 1", "--max-rows", "-3"},
		{"query", url, "SELECT 1", "--max-bytes", "x"},
		{"query", url, "SELECT $1", "--params", "not json"},
		{"query", url, "SELECT $1", "--params", `{"a":1}`},
		{"query", url, "SELECT $1", "--param", "1", "--params", "[1]"},
		{"exec", url, "SELECT 1", "--max-rows", "5"},
		{"ping", url, "--param", "1"},
		{"ping", url, "extra"},
		{"bench", url, "--iterations", "0"},
		{"bench", url, "--iterations", "10001"},
		{"version", "--timeout", "1s"},
	}
	for _, args := range cases {
		mustFail(t, args...)
	}
}

func TestUnknownCommandTeachesUsage(t *testing.T) {
	e := mustFail(t, "SELECT * FROM users WHERE email = 'alex@example.com' AND this_is_long = true")
	if !strings.Contains(e.Hint, "--param") || !strings.Contains(e.Message, "...") {
		t.Fatalf("%+v", e)
	}
}

func TestParseTimeout(t *testing.T) {
	cases := map[string]time.Duration{"5s": 5 * time.Second, "500ms": 500 * time.Millisecond, "2m": 2 * time.Minute, "2": 2 * time.Second, "0.5": 500 * time.Millisecond}
	for in, want := range cases {
		if got, err := parseTimeout(in); err != nil || got != want {
			t.Errorf("parseTimeout(%q) = %v, %v", in, got, err)
		}
	}
}

func TestAliases(t *testing.T) {
	for _, a := range []string{"help", "-h", "--help"} {
		if mustParse(t, a).cmd != "help" {
			t.Fatal(a)
		}
	}
	if mustParse(t).cmd != "help" {
		t.Fatal("no args should be help")
	}
	for _, a := range []string{"version", "-v", "--version"} {
		if mustParse(t, a).cmd != "version" {
			t.Fatal(a)
		}
	}
}

func TestHelpIsValidJSONAndComplete(t *testing.T) {
	b := helpJSON()
	if strings.Contains(string(b), "\n") {
		t.Fatal("help must be a single line")
	}
	var h struct {
		OK       bool                       `json:"ok"`
		Version  string                     `json:"version"`
		Commands map[string]json.RawMessage `json:"commands"`
		Flags    map[string]string          `json:"flags"`
	}
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatalf("help is not valid JSON: %v", err)
	}
	if !h.OK || h.Version != version {
		t.Fatalf("%+v", h)
	}
	for cmd, flags := range commandFlags {
		if _, ok := h.Commands[cmd]; !ok {
			t.Errorf("help does not document command %q", cmd)
		}
		for _, f := range flags {
			found := false
			for k := range h.Flags {
				if strings.HasPrefix(k, "--"+f+" ") {
					found = true
				}
			}
			if !found {
				t.Errorf("help does not document flag --%s", f)
			}
		}
	}
}

func TestRunWithoutDatabase(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"version"}} {
		b, err := run(args, time.Now())
		if err != nil || !json.Valid(b) {
			t.Fatalf("%v: %v %s", args, err, b)
		}
	}
	if _, err := run([]string{"ping", "postgres://h:0/db"}, time.Now()); perr.Classify(err).Type != perr.InvalidInput {
		t.Fatalf("bad port should be invalid_input: %v", err)
	}
}

func TestColumnKeys(t *testing.T) {
	cols := []postgres.Column{{Name: "id"}, {Name: "id"}, {Name: "id_2"}, {Name: "?column?"}, {Name: "é \"q\""}, {Name: "id"}}
	var got []string
	for _, k := range columnKeys(cols) {
		var s string
		if err := json.Unmarshal(k, &s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	want := []string{"id", "id_2", "id_2_2", "?column?", "é \"q\"", "id_3"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
}
