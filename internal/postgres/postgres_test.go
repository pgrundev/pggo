package postgres

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

func TestParseURL(t *testing.T) {
	c, err := ParseURL("postgres://u:p%40ss@db.example.com:6543/app?sslmode=require&application_name=x&lock_timeout=1s&channel_binding=require")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "db.example.com" || c.Port != 6543 || c.User != "u" || c.Password != "p@ss" || c.Database != "app" || c.SSLMode != "require" {
		t.Fatalf("%+v", c)
	}
	if c.Params["lock_timeout"] != "1s" || c.Params["application_name"] != "x" || c.Params["channel_binding"] != "" {
		t.Fatalf("params %v", c.Params)
	}
	c, err = ParseURL("host=/tmp port=5433 user=bob password='a b\\'c' dbname=d sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "/tmp" || c.Port != 5433 || c.Password != "a b'c" || c.Database != "d" {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{"postgres://h:0/db", "postgres://h/db?sslmode=bogus", "garbage", "password='unterminated"} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("ParseURL(%q) should fail", bad)
		}
	}
}

func TestParseCommandTag(t *testing.T) {
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
		cmd, n := ParseCommandTag(c.tag)
		if cmd != c.cmd || n != c.rows {
			t.Errorf("ParseCommandTag(%q) = %q, %d", c.tag, cmd, n)
		}
	}
}

func TestAppendValue(t *testing.T) {
	cases := []struct {
		oid  uint32
		raw  []byte
		want string
	}{
		{oidInt4, nil, "null"},
		{oidBool, []byte("t"), "true"},
		{oidBool, []byte("f"), "false"},
		{oidInt8, []byte("-9"), "-9"},
		{oidFloat8, []byte("1e+100"), "1e+100"},
		{oidFloat8, []byte("-Infinity"), `"-Infinity"`},
		{oidNumeric, []byte("NaN"), `"NaN"`},
		{oidNumeric, []byte("0.10"), "0.10"},
		{oidJSONB, []byte(`{"a":1}`), `{"a":1}`},
		{25, []byte("a\"b\\c\n\x01<>&"), `"a\"b\\c\n\u0001<>&"`},
		{25, []byte("\xff"), `"�"`},
	}
	for _, c := range cases {
		if got := string(AppendValue(nil, c.oid, c.raw)); got != c.want {
			t.Errorf("AppendValue(%d, %q) = %s, want %s", c.oid, c.raw, got, c.want)
		}
	}
}

func TestSCRAMVector(t *testing.T) {
	// RFC 7677 test vector (user "user", password "pencil").
	s := &scram{password: "pencil", nonce: "rOprNGfwEbeRWgbNEkqO"}
	s.clientFirst()
	s.clientBare = "n=user,r=rOprNGfwEbeRWgbNEkqO"
	final, err := s.clientFinal([]byte("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"))
	if err != nil {
		t.Fatal(err)
	}
	want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if string(final) != want {
		t.Fatalf("got %s", final)
	}
	if !s.verifyServer([]byte("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=")) {
		t.Fatal("server signature not verified")
	}
}
