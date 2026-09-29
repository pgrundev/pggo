package pggo

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Type OIDs pggo decodes natively. Any other type can still be read as a
// string (its text representation) or as a RawValue.
const (
	OIDBool        = 16
	OIDBytea       = 17
	OIDChar        = 18 // "char"
	OIDName        = 19
	OIDInt8        = 20
	OIDInt2        = 21
	OIDInt2Vector  = 22
	OIDInt4        = 23
	OIDText        = 25
	OIDOID         = 26
	OIDXID         = 28
	OIDCID         = 29
	OIDOIDVector   = 30
	OIDJSON        = 114
	OIDFloat4      = 700
	OIDFloat8      = 701
	OIDBoolArray   = 1000
	OIDInt2Array   = 1005
	OIDInt4Array   = 1007
	OIDTextArray   = 1009
	OIDBPChar      = 1042
	OIDVarchar     = 1043
	OIDInt8Array   = 1016
	OIDOIDArray    = 1028
	OIDVarcharArr  = 1015
	OIDNameArray   = 1003
	OIDFloat8Array = 1022
	OIDDate        = 1082
	OIDTimestamp   = 1114
	OIDTimestampTZ = 1184
	OIDInterval    = 1186
	OIDNumeric     = 1700
	OIDUUID        = 2950
	OIDJSONB       = 3802
)

// Format is a PostgreSQL wire format code.
type Format int16

const (
	TextFormat   Format = 0
	BinaryFormat Format = 1
)

// RawValue is a column value exactly as the server sent it. Scanning into
// *RawValue works for every type, including ones pggo has no decoder for.
type RawValue struct {
	OID    uint32
	Format Format
	Bytes  []byte // nil for SQL NULL
}

// IsNull reports whether the value is SQL NULL.
func (v RawValue) IsNull() bool { return v.Bytes == nil }

func (v RawValue) String() string { return string(v.Bytes) }

// Numeric is a numeric value in its exact decimal text form. It marshals to
// JSON as a number (or as the string "NaN"/"Infinity").
type Numeric string

func (n Numeric) MarshalJSON() ([]byte, error) {
	if _, err := strconv.ParseFloat(string(n), 64); err != nil || strings.ContainsAny(string(n), "nNiI") {
		return json.Marshal(string(n))
	}
	return []byte(n), nil
}

// Float64 converts the value (possibly losing precision).
func (n Numeric) Float64() (float64, error) { return parseFloatPG(string(n)) }

// ---- parameters ------------------------------------------------------------

// encodeParam converts a Go value to a text-format parameter (nil = NULL).
func encodeParam(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(x), nil
	case json.RawMessage:
		if x == nil {
			return nil, nil
		}
		return []byte(x), nil
	case []byte:
		if x == nil {
			return nil, nil
		}
		return []byte(`\x` + hex.EncodeToString(x)), nil
	case bool:
		return []byte(strconv.FormatBool(x)), nil
	case int:
		return strconv.AppendInt(nil, int64(x), 10), nil
	case int8:
		return strconv.AppendInt(nil, int64(x), 10), nil
	case int16:
		return strconv.AppendInt(nil, int64(x), 10), nil
	case int32:
		return strconv.AppendInt(nil, int64(x), 10), nil
	case int64:
		return strconv.AppendInt(nil, x, 10), nil
	case uint:
		return strconv.AppendUint(nil, uint64(x), 10), nil
	case uint8:
		return strconv.AppendUint(nil, uint64(x), 10), nil
	case uint16:
		return strconv.AppendUint(nil, uint64(x), 10), nil
	case uint32:
		return strconv.AppendUint(nil, uint64(x), 10), nil
	case uint64:
		return strconv.AppendUint(nil, x, 10), nil
	case float32:
		return formatFloat(float64(x), 32), nil
	case float64:
		return formatFloat(x, 64), nil
	case Numeric:
		return []byte(x), nil
	case time.Time:
		return []byte(x.Format("2006-01-02 15:04:05.999999999Z07:00")), nil
	case []string:
		return arrayLiteral(len(x), func(i int) (string, bool) { return x[i], true }), nil
	case []int32:
		return arrayLiteral(len(x), func(i int) (string, bool) { return strconv.Itoa(int(x[i])), false }), nil
	case []int64:
		return arrayLiteral(len(x), func(i int) (string, bool) { return strconv.FormatInt(x[i], 10), false }), nil
	case []int:
		return arrayLiteral(len(x), func(i int) (string, bool) { return strconv.Itoa(x[i]), false }), nil
	case RawValue:
		return x.Bytes, nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, nil
		}
		return encodeParam(rv.Elem().Interface())
	case reflect.String:
		return []byte(rv.String()), nil
	case reflect.Bool:
		return []byte(strconv.FormatBool(rv.Bool())), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(nil, rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.AppendUint(nil, rv.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return formatFloat(rv.Float(), 64), nil
	}
	return nil, fmt.Errorf("unsupported parameter type %T", v)
}

func formatFloat(f float64, bits int) []byte {
	switch {
	case math.IsNaN(f):
		return []byte("NaN")
	case math.IsInf(f, 1):
		return []byte("Infinity")
	case math.IsInf(f, -1):
		return []byte("-Infinity")
	}
	return strconv.AppendFloat(nil, f, 'g', -1, bits)
}

func arrayLiteral(n int, elem func(int) (string, bool)) []byte {
	b := []byte{'{'}
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		s, quote := elem(i)
		if !quote {
			b = append(b, s...)
			continue
		}
		b = append(b, '"')
		for j := 0; j < len(s); j++ {
			if s[j] == '"' || s[j] == '\\' {
				b = append(b, '\\')
			}
			b = append(b, s[j])
		}
		b = append(b, '"')
	}
	return append(b, '}')
}

// ---- scanning --------------------------------------------------------------

// Scan copies the current row into dest, one destination per column.
// Destinations are pointers (see scanValue for the supported conversions);
// pass nil to skip a column.
func (r *Rows) Scan(dest ...any) error {
	if r.vals == nil && len(r.cols) > 0 {
		return errors.New("pggo: Scan called without a current row")
	}
	if len(dest) != len(r.cols) {
		return fmt.Errorf("pggo: Scan: %d destinations for %d columns", len(dest), len(r.cols))
	}
	for i, d := range dest {
		if d == nil {
			continue
		}
		if err := scanValue(d, r.cols[i].TypeOID, r.vals[i]); err != nil {
			return fmt.Errorf("pggo: column %q (index %d): %w", r.cols[i].Name, i, err)
		}
	}
	return nil
}

// Values returns the current row as Go values. The types follow pgx, so code
// that marshals them (e.g. to JSON) sees the same shapes: bool, int16/int32/
// int64, uint32 (oid), float32/float64, Numeric, string, int32 for "char",
// time.Time, []byte (bytea), decoded JSON, []any for arrays, nil for NULL.
// Types without a decoder come back as their text representation (string).
func (r *Rows) Values() ([]any, error) {
	out := make([]any, len(r.vals))
	for i, v := range r.vals {
		var x any
		if err := scanValue(&x, r.cols[i].TypeOID, v); err != nil {
			return nil, fmt.Errorf("pggo: column %q: %w", r.cols[i].Name, err)
		}
		out[i] = x
	}
	return out, nil
}

var errNull = errors.New("cannot scan NULL into a non-pointer destination (use a pointer, e.g. *string)")

func isIntOID(oid uint32) bool {
	switch oid {
	case OIDInt2, OIDInt4, OIDInt8, OIDOID, OIDXID, OIDCID:
		return true
	}
	return false
}

func isNumberOID(oid uint32) bool {
	return isIntOID(oid) || oid == OIDFloat4 || oid == OIDFloat8 || oid == OIDNumeric
}

// scanValue decodes one text-format value into dest.
func scanValue(dest any, oid uint32, raw []byte) error {
	switch d := dest.(type) {
	case *RawValue:
		*d = RawValue{OID: oid, Format: TextFormat}
		if raw != nil {
			d.Bytes = append([]byte{}, raw...)
		}
		return nil
	case *any:
		v, err := decodeAny(oid, raw)
		*d = v
		return err
	case *string:
		if raw == nil {
			return errNull
		}
		*d = string(raw)
		return nil
	case *[]byte:
		if raw == nil {
			*d = nil
			return nil
		}
		if oid == OIDBytea {
			b, err := decodeBytea(raw)
			*d = b
			return err
		}
		*d = append([]byte{}, raw...)
		return nil
	case *json.RawMessage:
		if raw == nil {
			*d = nil
			return nil
		}
		*d = append(json.RawMessage{}, raw...)
		return nil
	case *bool:
		if raw == nil {
			return errNull
		}
		if oid != OIDBool {
			return fmt.Errorf("cannot scan type OID %d into *bool", oid)
		}
		*d = len(raw) == 1 && raw[0] == 't'
		return nil
	case *time.Time:
		if raw == nil {
			return errNull
		}
		t, err := parseTime(oid, string(raw))
		*d = t
		return err
	case *float64:
		if raw == nil {
			return errNull
		}
		if !isNumberOID(oid) {
			return fmt.Errorf("cannot scan type OID %d into *float64", oid)
		}
		f, err := parseFloatPG(string(raw))
		*d = f
		return err
	case *float32:
		if raw == nil {
			return errNull
		}
		if !isNumberOID(oid) {
			return fmt.Errorf("cannot scan type OID %d into *float32", oid)
		}
		f, err := parseFloatPG(string(raw))
		*d = float32(f)
		return err
	case *Numeric:
		if raw == nil {
			return errNull
		}
		if !isNumberOID(oid) {
			return fmt.Errorf("cannot scan type OID %d into *pggo.Numeric", oid)
		}
		*d = Numeric(raw)
		return nil
	case *[]string:
		if raw == nil {
			*d = nil
			return nil
		}
		elems, err := parseArray(oid, string(raw))
		if err != nil {
			return err
		}
		out := make([]string, len(elems))
		for i, e := range elems {
			if e == nil {
				return errors.New("cannot scan a NULL array element into string")
			}
			out[i] = *e
		}
		*d = out
		return nil
	case *[]int32:
		return scanIntSlice(d, oid, raw, 32, func(n int64) int32 { return int32(n) })
	case *[]int64:
		return scanIntSlice(d, oid, raw, 64, func(n int64) int64 { return n })
	case *[]int16:
		return scanIntSlice(d, oid, raw, 16, func(n int64) int16 { return int16(n) })
	case *[]uint32:
		if raw == nil {
			*d = nil
			return nil
		}
		elems, err := parseArray(oid, string(raw))
		if err != nil {
			return err
		}
		out := make([]uint32, len(elems))
		for i, e := range elems {
			if e == nil {
				return errors.New("cannot scan a NULL array element into uint32")
			}
			n, err := strconv.ParseUint(*e, 10, 32)
			if err != nil {
				return err
			}
			out[i] = uint32(n)
		}
		*d = out
		return nil
	}
	return scanReflect(dest, oid, raw)
}

// scanReflect handles integers of every size, pointers (nullable), and named
// types whose underlying kind is supported (type Kind string).
func scanReflect(dest any, oid uint32, raw []byte) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("destination must be a non-nil pointer, got %T", dest)
	}
	el := rv.Elem()
	if el.Kind() == reflect.Pointer {
		if raw == nil {
			el.Set(reflect.Zero(el.Type()))
			return nil
		}
		v := reflect.New(el.Type().Elem())
		if err := scanValue(v.Interface(), oid, raw); err != nil {
			return err
		}
		el.Set(v)
		return nil
	}
	if raw == nil {
		return errNull
	}
	switch el.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := parseIntPG(oid, string(raw))
		if err != nil {
			return err
		}
		if el.OverflowInt(n) {
			return fmt.Errorf("value %d out of range for %s", n, el.Type())
		}
		el.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := parseIntPG(oid, string(raw))
		if err != nil {
			return err
		}
		if n < 0 || el.OverflowUint(uint64(n)) {
			return fmt.Errorf("value %d out of range for %s", n, el.Type())
		}
		el.SetUint(uint64(n))
		return nil
	case reflect.String:
		el.SetString(string(raw))
		return nil
	case reflect.Bool:
		var b bool
		if err := scanValue(&b, oid, raw); err != nil {
			return err
		}
		el.SetBool(b)
		return nil
	case reflect.Float32, reflect.Float64:
		var f float64
		if err := scanValue(&f, oid, raw); err != nil {
			return err
		}
		el.SetFloat(f)
		return nil
	}
	return fmt.Errorf("unsupported scan destination %T for type OID %d", dest, oid)
}

// parseIntPG reads an integer from an integer, numeric, or float column. A
// numeric/float value must be a whole number: nothing is silently truncated.
func parseIntPG(oid uint32, s string) (int64, error) {
	switch {
	case isIntOID(oid):
		return strconv.ParseInt(s, 10, 64)
	case oid == OIDNumeric || oid == OIDFloat4 || oid == OIDFloat8:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
		if i := strings.IndexByte(s, '.'); i >= 0 && strings.Trim(s[i+1:], "0") == "" {
			if n, err := strconv.ParseInt(s[:i], 10, 64); err == nil {
				return n, nil
			}
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != math.Trunc(f) || f > math.MaxInt64 || f < math.MinInt64 {
			return 0, fmt.Errorf("cannot scan %q into an integer without losing information", s)
		}
		return int64(f), nil
	}
	return 0, fmt.Errorf("cannot scan type OID %d into an integer", oid)
}

func parseFloatPG(s string) (float64, error) {
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "Infinity":
		return math.Inf(1), nil
	case "-Infinity":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

func scanIntSlice[T int16 | int32 | int64](d *[]T, oid uint32, raw []byte, bits int, conv func(int64) T) error {
	if raw == nil {
		*d = nil
		return nil
	}
	elems, err := parseArray(oid, string(raw))
	if err != nil {
		return err
	}
	out := make([]T, len(elems))
	for i, e := range elems {
		if e == nil {
			return errors.New("cannot scan a NULL array element into an integer")
		}
		n, err := strconv.ParseInt(*e, 10, bits)
		if err != nil {
			return err
		}
		out[i] = conv(n)
	}
	*d = out
	return nil
}

// parseArray parses a one-dimensional array literal ({a,"b c",NULL}) or an
// int2vector/oidvector ("1 2 3"). NULL elements are nil.
func parseArray(oid uint32, s string) ([]*string, error) {
	if oid == OIDInt2Vector || oid == OIDOIDVector {
		var out []*string
		for _, f := range strings.Fields(s) {
			f := f
			out = append(out, &f)
		}
		return out, nil
	}
	if i := strings.IndexByte(s, '='); i > 0 && strings.HasPrefix(s, "[") {
		s = s[i+1:] // explicit bounds: [0:2]={...}
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil, fmt.Errorf("type OID %d is not an array (%q)", oid, truncate(s))
	}
	body := s[1 : len(s)-1]
	var out []*string
	if body == "" {
		return []*string{}, nil
	}
	for i := 0; i <= len(body); {
		if i < len(body) && body[i] == '{' {
			return nil, errors.New("multi-dimensional arrays are not supported")
		}
		var el strings.Builder
		quoted := false
		if i < len(body) && body[i] == '"' {
			quoted = true
			i++
			for ; i < len(body) && body[i] != '"'; i++ {
				if body[i] == '\\' && i+1 < len(body) {
					i++
				}
				el.WriteByte(body[i])
			}
			i++ // closing quote
		} else {
			for ; i < len(body) && body[i] != ','; i++ {
				el.WriteByte(body[i])
			}
		}
		v := el.String()
		if !quoted && v == "NULL" {
			out = append(out, nil)
		} else {
			out = append(out, &v)
		}
		if i < len(body) && body[i] != ',' {
			return nil, fmt.Errorf("malformed array literal %q", truncate(s))
		}
		i++
	}
	return out, nil
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func decodeBytea(raw []byte) ([]byte, error) {
	if len(raw) >= 2 && raw[0] == '\\' && raw[1] == 'x' {
		out := make([]byte, hex.DecodedLen(len(raw)-2))
		_, err := hex.Decode(out, raw[2:])
		return out, err
	}
	return nil, errors.New("bytea not in hex format (bytea_output = 'escape' is not supported)")
}

var tsLayouts = []string{
	"2006-01-02 15:04:05.999999999Z07",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07:00:00",
}

// parseTime decodes timestamptz (returned in the local time zone, as pgx
// does), timestamp and date (returned in UTC).
func parseTime(oid uint32, s string) (time.Time, error) {
	if s == "infinity" || s == "-infinity" {
		return time.Time{}, fmt.Errorf("cannot scan %s into time.Time", s)
	}
	if strings.HasSuffix(s, " BC") {
		return time.Time{}, errors.New("BC dates are not supported")
	}
	switch oid {
	case OIDTimestampTZ:
		for _, l := range tsLayouts {
			if t, err := time.Parse(l, s); err == nil {
				return t.Local(), nil
			}
		}
		return time.Time{}, fmt.Errorf("cannot parse timestamptz %q", s)
	case OIDTimestamp:
		return time.Parse("2006-01-02 15:04:05.999999999", s)
	case OIDDate:
		return time.Parse("2006-01-02", s)
	}
	return time.Time{}, fmt.Errorf("cannot scan type OID %d into time.Time", oid)
}

// decodeAny implements Values: pgx-compatible Go types per OID.
func decodeAny(oid uint32, raw []byte) (any, error) {
	if raw == nil {
		return nil, nil
	}
	s := string(raw)
	switch oid {
	case OIDBool:
		return s == "t", nil
	case OIDInt2:
		n, err := strconv.ParseInt(s, 10, 16)
		return int16(n), err
	case OIDInt4:
		n, err := strconv.ParseInt(s, 10, 32)
		return int32(n), err
	case OIDInt8:
		return strconv.ParseInt(s, 10, 64)
	case OIDOID, OIDXID, OIDCID:
		n, err := strconv.ParseUint(s, 10, 32)
		return uint32(n), err
	case OIDFloat4:
		f, err := parseFloatPG(s)
		return float32(f), err
	case OIDFloat8:
		return parseFloatPG(s)
	case OIDNumeric:
		return Numeric(s), nil
	case OIDChar:
		if len(raw) == 0 {
			return int32(0), nil
		}
		return int32(raw[0]), nil // pgx decodes "char" as a rune
	case OIDBytea:
		return decodeBytea(raw)
	case OIDJSON, OIDJSONB:
		var v any
		err := json.Unmarshal(raw, &v)
		return v, err
	case OIDTimestampTZ, OIDTimestamp, OIDDate:
		return parseTime(oid, s)
	case OIDTextArray, OIDVarcharArr, OIDNameArray, OIDInt2Array, OIDInt4Array, OIDInt8Array, OIDOIDArray, OIDFloat8Array, OIDBoolArray:
		elems, err := parseArray(oid, s)
		if err != nil {
			return nil, err
		}
		elemOID := map[uint32]uint32{OIDTextArray: OIDText, OIDVarcharArr: OIDVarchar, OIDNameArray: OIDName,
			OIDInt2Array: OIDInt2, OIDInt4Array: OIDInt4, OIDInt8Array: OIDInt8, OIDOIDArray: OIDOID,
			OIDFloat8Array: OIDFloat8, OIDBoolArray: OIDBool}[oid]
		out := make([]any, len(elems))
		for i, e := range elems {
			if e == nil {
				continue
			}
			v, err := decodeAny(elemOID, []byte(*e))
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	return s, nil
}
