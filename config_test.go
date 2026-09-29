package pggo

import (
	"testing"
)

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig("postgres://u:p%40ss@db.example.com:6543/app?sslmode=require&application_name=x&lock_timeout=1s&channel_binding=require")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "db.example.com" || c.Port != 6543 || c.User != "u" || c.Password != "p@ss" || c.Database != "app" || c.SSLMode != "require" {
		t.Fatalf("%+v", c)
	}
	if c.RuntimeParams["lock_timeout"] != "1s" || c.RuntimeParams["application_name"] != "x" || c.RuntimeParams["channel_binding"] != "" {
		t.Fatalf("params %v", c.RuntimeParams)
	}
	c, err = ParseConfig("host=/tmp port=5433 user=bob password='a b\\'c' dbname=d sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "/tmp" || c.Port != 5433 || c.Password != "a b'c" || c.Database != "d" {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{"postgres://h:0/db", "postgres://h/db?sslmode=bogus", "garbage", "password='unterminated"} {
		if _, err := ParseConfig(bad); err == nil {
			t.Errorf("ParseConfig(%q) should fail", bad)
		}
	}
}

func FuzzParseConfig(f *testing.F) {
	for _, s := range []string{"postgres://u:p@h:5432/db?sslmode=require", "host=h port=1 password='a\\'b'", "postgresql://[::1]:5/x", "a=", "='x'", "postgres://%zz"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := ParseConfig(s)
		if err == nil && (c.Port <= 0 || c.Port > 65535 || c.SSLMode == "") {
			t.Fatalf("ParseConfig(%q) accepted invalid config %+v", s, c)
		}
	})
}
