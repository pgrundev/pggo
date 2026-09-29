// Package output writes pgGo's JSON responses. Keys are emitted in a fixed,
// documented order so output is byte-for-byte deterministic for a given result.
package output

import (
	"math"
	"os"
	"strconv"
	"unicode/utf8"

	perr "github.com/pgrundev/pggo/internal/errors"
)

// Object builds a single-line JSON object with keys in insertion order.
type Object struct{ b []byte }

func NewObject() *Object { return &Object{b: []byte{'{'}} }

func (o *Object) key(k string) *Object {
	if len(o.b) > 1 {
		o.b = append(o.b, ',')
	}
	o.b = AppendString(o.b, k)
	o.b = append(o.b, ':')
	return o
}

func (o *Object) Bool(k string, v bool) *Object {
	o.key(k).b = strconv.AppendBool(o.b, v)
	return o
}

func (o *Object) Int(k string, v int64) *Object {
	o.key(k).b = strconv.AppendInt(o.b, v, 10)
	return o
}

func (o *Object) String(k, v string) *Object {
	o.key(k).b = AppendString(o.b, v)
	return o
}

// Ms writes a duration in milliseconds rounded to 3 decimals.
func (o *Object) Ms(k string, ms float64) *Object {
	o.key(k).b = AppendMs(o.b, ms)
	return o
}

// Raw writes v, which must already be valid JSON.
func (o *Object) Raw(k string, v []byte) *Object {
	o.key(k).b = append(o.b, v...)
	return o
}

func (o *Object) Bytes() []byte { return append(o.b, '}') }

// AppendMs appends a millisecond value rounded to 3 decimals.
func AppendMs(b []byte, ms float64) []byte {
	return strconv.AppendFloat(b, math.Round(ms*1000)/1000, 'f', -1, 64)
}

const hexDigits = "0123456789abcdef"

// AppendString appends s as a JSON string. Unlike encoding/json it does not
// HTML-escape, keeping output readable; invalid UTF-8 becomes U+FFFD.
func AppendString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 || r == ' ' || r == ' ' {
			b = append(b, s[start:i]...)
			if r == utf8.RuneError {
				b = append(b, `�`...)
			} else {
				b = append(b, `\u202`...)
				b = append(b, hexDigits[r&0xF])
			}
			i += size
			start = i
			continue
		}
		i += size
	}
	return append(append(b, s[start:]...), '"')
}

// Write prints one JSON document followed by a newline to stdout.
func Write(b []byte) {
	os.Stdout.Write(append(b, '\n'))
}

// Error renders the structured error envelope.
func Error(e *perr.Error) []byte {
	eo := NewObject().String("type", e.Type)
	if e.Code != "" {
		eo.String("code", e.Code)
	} else {
		eo.Raw("code", []byte("null"))
	}
	eo.String("message", e.Message)
	if e.Detail != "" {
		eo.String("detail", e.Detail)
	}
	if e.Hint != "" {
		eo.String("hint", e.Hint)
	}
	if e.Position > 0 {
		eo.Int("position", int64(e.Position))
	}
	eo.Bool("retryable", e.Retryable)
	return NewObject().Bool("ok", false).Raw("error", eo.Bytes()).Bytes()
}
