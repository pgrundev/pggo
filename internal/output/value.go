package output

// Type OIDs that map to native JSON values. Everything else is a JSON string
// holding PostgreSQL's text representation.
const (
	oidBool    = 16
	oidInt8    = 20
	oidInt2    = 21
	oidInt4    = 23
	oidOID     = 26
	oidJSON    = 114
	oidFloat4  = 700
	oidFloat8  = 701
	oidNumeric = 1700
	oidJSONB   = 3802
)

// AppendValue appends the JSON encoding of a text-format column value.
// raw == nil means SQL NULL.
func AppendValue(b []byte, oid uint32, raw []byte) []byte {
	if raw == nil {
		return append(b, "null"...)
	}
	switch oid {
	case oidBool:
		if len(raw) == 1 && raw[0] == 't' {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case oidInt2, oidInt4, oidInt8, oidOID, oidFloat4, oidFloat8, oidNumeric:
		// NaN and ±Infinity have no JSON number form; keep them as strings.
		// Validating integers too guarantees valid JSON even from a misbehaving server.
		if isJSONNumber(raw) {
			return append(b, raw...)
		}
	case oidJSON, oidJSONB:
		return append(b, raw...)
	}
	return AppendString(b, string(raw))
}

func isJSONNumber(s []byte) bool {
	if len(s) == 0 {
		return false
	}
	i := 0
	if s[0] == '-' {
		i++
	}
	digits := 0
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		digits++
	}
	if digits == 0 || (digits > 1 && s[i-digits] == '0') {
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		n := i
		for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		}
		if i == n {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		n := i
		for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		}
		if i == n {
			return false
		}
	}
	return i == len(s)
}
