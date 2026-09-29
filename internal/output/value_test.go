package output

import (
	"encoding/json"
	"testing"
)

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
		{25, []byte("\xff"), "\"\uFFFD\""},
	}
	for _, c := range cases {
		if got := string(AppendValue(nil, c.oid, c.raw)); got != c.want {
			t.Errorf("AppendValue(%d, %q) = %s, want %s", c.oid, c.raw, got, c.want)
		}
	}
}

func FuzzAppendValue(f *testing.F) {
	for _, oid := range []uint32{oidBool, oidInt4, oidInt8, oidFloat8, oidNumeric, 25} {
		for _, v := range []string{"1", "-1.5e10", "NaN", "t", "01", "1.", ".5", "-", "1e", "x\"y", "\xff"} {
			f.Add(oid, []byte(v))
		}
	}
	f.Fuzz(func(t *testing.T, oid uint32, raw []byte) {
		if oid == oidJSON || oid == oidJSONB {
			return // embedded verbatim: the server guarantees validity
		}
		if b := AppendValue(nil, oid, raw); !json.Valid(b) {
			t.Fatalf("AppendValue(%d, %q) = %s is not valid JSON", oid, raw, b)
		}
	})
}
