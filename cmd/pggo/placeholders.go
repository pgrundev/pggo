package main

// MaxPlaceholder returns the highest $N placeholder in sql, skipping string
// literals, quoted identifiers, dollar-quoted strings, and comments.
// It is used to validate parameter counts before contacting the server.
func MaxPlaceholder(sql string) int {
	max := 0
	s := sql
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			escapes := i > 0 && (s[i-1] == 'E' || s[i-1] == 'e')
			for i++; i < len(s); i++ {
				if escapes && s[i] == '\\' {
					i++
				} else if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i++
						continue
					}
					break
				}
			}
		case c == '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
			}
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for ; i < len(s) && s[i] != '\n'; i++ {
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			depth := 0
			for ; i+1 < len(s); i++ {
				if s[i] == '/' && s[i+1] == '*' {
					depth++
					i++
				} else if s[i] == '*' && s[i+1] == '/' {
					depth--
					i++
					if depth == 0 {
						break
					}
				}
			}
		case c == '$':
			j := i + 1
			n := 0
			for ; j < len(s) && s[j] >= '0' && s[j] <= '9'; j++ {
				n = n*10 + int(s[j]-'0')
				if n > 65535 {
					n = 65535
				}
			}
			if j > i+1 {
				if n > max {
					max = n
				}
				i = j - 1
				continue
			}
			if i > 0 && isIdent(s[i-1]) {
				continue // $ inside an identifier, e.g. foo$bar
			}
			// Dollar-quoted string: $tag$ ... $tag$
			for j < len(s) && isIdent(s[j]) {
				j++
			}
			if j < len(s) && s[j] == '$' {
				tag := s[i : j+1]
				end := indexFrom(s, tag, j+1)
				if end < 0 {
					return max
				}
				i = end + len(tag) - 1
			}
		}
	}
	return max
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

func indexFrom(s, sub string, from int) int {
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
