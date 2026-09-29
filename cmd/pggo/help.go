package main

// helpText is printed by `pggo`, `pggo help`, and `pggo --help`. It is JSON so
// an agent can read it with the same parser it uses for every other response.
const helpText = `{"ok":true,"name":"pggo","version":"` + version + `",
"description":"Tiny PostgreSQL adapter for LLMs and coding agents. Every command prints exactly one JSON object to stdout. Exit code 0 means ok:true, 1 means ok:false.",
"connection":"Pass a postgres:// URL as the first argument, or omit it to use $DATABASE_URL (then libpq PG* variables).",
"commands":{
 "ping":{"usage":"pggo ping [URL]","does":"Check that PostgreSQL is reachable and accepts the credentials.","returns":{"ok":true,"latency_ms":3.4}},
 "info":{"usage":"pggo info [URL]","does":"Server version, current database, current user, and whether the session is read-only.","returns":{"ok":true,"postgres_version":"18.0","database":"app","user":"agent","read_only":false}},
 "query":{"usage":"pggo query [URL] 'SELECT ... WHERE id = $1' --param 42","does":"Run one read-only statement and return rows as JSON objects. Writes are rejected; use exec.","returns":{"ok":true,"columns":["id","email"],"rows":[{"id":42,"email":"a@example.com"}],"row_count":1,"truncated":false,"duration_ms":2.8}},
 "exec":{"usage":"pggo exec [URL] 'UPDATE users SET active = $1 WHERE id = $2' --param true --param 42","does":"Run one statement that changes data or schema (INSERT, UPDATE, DELETE, DDL). Autocommits.","returns":{"ok":true,"command":"UPDATE","rows_affected":1,"duration_ms":2.1}},
 "bench":{"usage":"pggo bench [URL] [--iterations 10]","does":"Measure connection setup and warm SELECT 1 latency (min/p50/p95/p99/max ms)."},
 "version":{"usage":"pggo version"},
 "help":{"usage":"pggo help"}
},
"flags":{
 "--param VALUE":"Value for the next $N placeholder, in order. Repeatable. Sent separately from the SQL, never interpolated.",
 "--params JSON":"All parameters as a JSON array, e.g. '[42, \"a@b.com\", null]'. Use this to pass NULL.",
 "--timeout DURATION":"Limit for the whole command, e.g. 5s or 500ms. Default 10s. Server-side work is canceled on expiry.",
 "--max-rows N":"query: maximum rows returned. Default 100.",
 "--max-bytes N":"query: maximum bytes of row JSON returned. Default 65536.",
 "--iterations N":"bench: samples per measurement. Default 10."
},
"rules":[
 "Always pass values with --param and $1, $2, ... placeholders; never paste values into the SQL string.",
 "Wrap SQL in single quotes so the shell does not expand $1.",
 "One statement per call.",
 "If truncated is true, more rows exist than were returned: narrow the query (WHERE, LIMIT, fewer columns) rather than raising limits.",
 "Timestamps are returned in ISO format; json/jsonb are embedded as JSON; bigint and numeric are JSON numbers; NaN/Infinity and other types are strings."
],
"errors":{
 "shape":{"ok":false,"error":{"type":"postgres_error","code":"42P01","message":"relation \"foo\" does not exist","retryable":false}},
 "types":["connection_error","authentication_error","timeout","postgres_error","invalid_input","protocol_error"],
 "code":"PostgreSQL SQLSTATE when the server reported one, else null. Optional fields: detail, hint, position (1-based character offset into the SQL).",
 "retryable":"true when retrying the same command unchanged may succeed (network, timeout, deadlock, serialization failure)."
}}`

func helpJSON() []byte {
	// Collapse to one line: every pggo response is a single JSON line.
	b := make([]byte, 0, len(helpText))
	for i := 0; i < len(helpText); i++ {
		if helpText[i] != '\n' {
			b = append(b, helpText[i])
		}
	}
	return b
}
