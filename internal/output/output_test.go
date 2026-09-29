package output

import (
	"encoding/json"
	"reflect"
	"testing"
	"unicode/utf8"

	perr "github.com/pgrundev/pggo/internal/errors"
)

func TestAppendString(t *testing.T) {
	cases := map[string]string{
		"":             `""`,
		"plain":        `"plain"`,
		`q"b\`:         `"q\"b\\"`,
		"\n\r\t":       `"\n\r\t"`,
		"\x00\x1f\x7f": `"\u0000\u001f` + "\x7f" + `"`,
		"<a&b>":        `"<a&b>"`,
		"é日本🐘":         `"é日本🐘"`,
		"\xff\xfe":     "\"\uFFFD\uFFFD\"",
		"a" + string(rune(0x2028)) + "b" + string(rune(0x2029)) + "c": `"a` + `\u2028` + "b" + `\u2029` + `c"`,
	}
	for in, want := range cases {
		if got := string(AppendString(nil, in)); got != want {
			t.Errorf("AppendString(%q) = %s, want %s", in, got, want)
		}
	}
}

func FuzzAppendString(f *testing.F) {
	for _, s := range []string{"", "a", "\"\\", "\x00", "\xff", "日本", string(rune(0x2028)), "\xed\xa0\x80"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b := AppendString(nil, s)
		var got string
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("invalid JSON %s for %q: %v", b, s, err)
		}
		if utf8.ValidString(s) && got != s {
			t.Fatalf("round trip: got %q want %q", got, s)
		}
	})
}

func TestObjectOrderAndTypes(t *testing.T) {
	b := NewObject().Bool("ok", true).String("z", "last?").Int("a", -5).Ms("ms", 1.23456).Raw("raw", []byte(`[1,{"x":null}]`)).Bytes()
	want := `{"ok":true,"z":"last?","a":-5,"ms":1.235,"raw":[1,{"x":null}]}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
	if string(NewObject().Bytes()) != "{}" {
		t.Fatal("empty object")
	}
}

func TestAppendMs(t *testing.T) {
	cases := map[float64]string{0: "0", 1: "1", 2.5: "2.5", 0.0004: "0", 0.0005: "0.001", 1234.56789: "1234.568"}
	for in, want := range cases {
		if got := string(AppendMs(nil, in)); got != want {
			t.Errorf("AppendMs(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestErrorEnvelope(t *testing.T) {
	full := Error(&perr.Error{Type: perr.Postgres, Code: "23505", Message: "dup", Detail: "d", Hint: "h", Position: 3})
	if string(full) != `{"ok":false,"error":{"type":"postgres_error","code":"23505","message":"dup","detail":"d","hint":"h","position":3,"retryable":false}}` {
		t.Fatalf("got %s", full)
	}
	min := Error(perr.New(perr.Timeout, "slow"))
	if string(min) != `{"ok":false,"error":{"type":"timeout","code":null,"message":"slow","retryable":true}}` {
		t.Fatalf("got %s", min)
	}
	var m map[string]any
	if err := json.Unmarshal(full, &m); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range m["error"].(map[string]any) {
		keys = append(keys, k)
	}
	if len(keys) != 7 {
		t.Fatalf("keys %v", keys)
	}
	_ = reflect.DeepEqual
}
