package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBackendTerminatedMidQuery(t *testing.T) {
	victim := withParam(baseURL, "application_name", "pggo_victim")
	done := make(chan response, 1)
	go func() { done <- pggo(t, "query", victim, "SELECT pg_sleep(20)", "--timeout", "30s") }()
	c := connect(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		err := c.QueryRow(context.Background(),
			"SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE application_name = 'pggo_victim' AND state = 'active'").Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("victim query never appeared")
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case r := <-done:
		wantErr(t, r, "connection_error", "57P01", true)
	case <-time.After(10 * time.Second):
		t.Fatal("pggo did not return after its backend was terminated")
	}
}

func TestReadOnlyHints(t *testing.T) {
	setup(t, "DROP TABLE IF EXISTS it_rohint", "CREATE TABLE it_rohint (id int)")
	e := wantErr(t, pggo(t, "query", baseURL, "INSERT INTO it_rohint VALUES (1)"), "postgres_error", "25006", false)
	if !strings.Contains(e["hint"].(string), "pggo exec") {
		t.Fatalf("query should point to exec: %v", e)
	}
	ro := withParam(baseURL, "default_transaction_read_only", "on")
	for _, cmd := range []string{"query", "exec"} {
		e := wantErr(t, pggo(t, cmd, ro, "INSERT INTO it_rohint VALUES (1)"), "postgres_error", "25006", false)
		if h := e["hint"].(string); strings.Contains(h, "pggo exec") || !strings.Contains(h, "read-only") {
			t.Fatalf("%s on a read-only connection must not suggest exec: %v", cmd, e)
		}
	}
	// Data-modifying CTEs are writes too.
	wantErr(t, pggo(t, "query", baseURL, "WITH d AS (DELETE FROM it_rohint RETURNING *) SELECT * FROM d"), "postgres_error", "25006", false)
}

func TestReadOnlyStatementsViaQuery(t *testing.T) {
	for _, sql := range []string{"EXPLAIN SELECT 1", "SHOW server_version", "SELECT * FROM pg_catalog.pg_class LIMIT 1", "VALUES (1, 'a'), (2, 'b')", "TABLE pg_catalog.pg_am"} {
		t.Run(sql, func(t *testing.T) {
			if rs := rows(t, pggo(t, "query", baseURL, sql)); len(rs) == 0 {
				t.Fatal("no rows")
			}
		})
	}
}

func TestExecSpecialStatements(t *testing.T) {
	setup(t, "DROP TABLE IF EXISTS it_special", "CREATE TABLE it_special (id serial PRIMARY KEY, v text)")
	cases := []struct {
		sql, cmd string
		n        int
	}{
		{"INSERT INTO it_special (v) VALUES ('a'), ('b') RETURNING id", "INSERT", 2},
		{"SELECT * FROM it_special", "SELECT", 2},
		{"VACUUM ANALYZE it_special", "VACUUM", 0}, // cannot run inside a transaction block
		{"CREATE INDEX CONCURRENTLY IF NOT EXISTS it_special_v ON it_special (v)", "CREATE INDEX", 0},
		{"DO $$ BEGIN RAISE NOTICE 'hello'; RAISE WARNING 'careful'; END $$", "DO", 0},
		{"MERGE INTO it_special t USING (VALUES (1, 'z')) s(id, v) ON t.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v", "MERGE", 1},
		{"COMMENT ON TABLE it_special IS 'x'", "COMMENT", 0},
		{"TRUNCATE it_special", "TRUNCATE TABLE", 0},
		{"SET work_mem = '8MB'", "SET", 0},
		{"-- leading comment\nSELECT 1", "SELECT", 1},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			r := pggo(t, "exec", baseURL, c.sql)
			wantOK(t, r, "command", "rows_affected", "duration_ms")
			if r.m["command"] != c.cmd || r.m["rows_affected"] != json.Number(strconv.Itoa(c.n)) {
				t.Fatalf("got %s", r.raw)
			}
		})
	}
	// Notices do not leak into output from query either.
	setup(t, `CREATE OR REPLACE FUNCTION it_noisy() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RAISE NOTICE 'noise'; RETURN 7; END $$`)
	if got := one(t, "SELECT it_noisy() AS v"); got != json.Number("7") {
		t.Fatalf("got %v", got)
	}
	// Explicit transaction control is rejected by exec? No: it simply autocommits each call.
	r := pggo(t, "exec", baseURL, "BEGIN")
	wantOK(t, r, "command", "rows_affected", "duration_ms")
}

func TestMoreTypes(t *testing.T) {
	setup(t, "DROP DOMAIN IF EXISTS it_posint CASCADE", "CREATE DOMAIN it_posint AS int CHECK (VALUE > 0)",
		"DROP TYPE IF EXISTS it_mood CASCADE", "CREATE TYPE it_mood AS ENUM ('happy', 'sad')")
	cases := []struct {
		sql  string
		want any
	}{
		{"SELECT 5::it_posint AS v", json.Number("5")}, // domains report their base type
		{"SELECT 'happy'::it_mood AS v", "happy"},
		{"SELECT '1 day 2 hours'::interval AS v", "1 day 02:00:00"},
		{"SELECT '10.0.0.1/8'::inet AS v", "10.0.0.1/8"},
		{"SELECT 'infinity'::date AS v", "infinity"},
		{"SELECT '-infinity'::timestamptz AS v", "-infinity"},
		{"SELECT (-9223372036854775808)::int8 AS v", json.Number("-9223372036854775808")},
		{"SELECT (-32768)::int2 AS v", json.Number("-32768")},
		{"SELECT 1e-7::float8 AS v", json.Number("1e-07")},
		{"SELECT '-0'::float8 AS v", json.Number("-0")},
		{"SELECT '-Infinity'::float4 AS v", "-Infinity"},
		{"SELECT 0.000000000000000000001::numeric AS v", json.Number("0.000000000000000000001")},
		{"SELECT 'ab'::char(4) AS v", "ab  "},
		{"SELECT ROW(1, 'a') AS v", "(1,a)"},
		{"SELECT int4range(1, 5) AS v", "[1,5)"},
		{"SELECT '12:34:56.789'::time AS v", "12:34:56.789"},
		{"SELECT 'pg_class'::regclass AS v", "pg_class"},
		{"SELECT 1259::oid AS v", json.Number("1259")},
		{"SELECT ''::text AS v", ""},
		{"SELECT ARRAY['a', NULL, 'b c'] AS v", `{a,NULL,"b c"}`},
		{"SELECT '[]'::jsonb AS v", []any{}},
		{`SELECT '"just a string"'::json AS v`, "just a string"},
		{"SELECT 'null'::jsonb AS v", nil},
		{"SELECT '<a>1</a>'::xml AS v", "<a>1</a>"},
		{"SELECT 'Emoji 🐘 and ✓'::text AS v", "Emoji 🐘 and ✓"},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			if got := one(t, c.sql); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestZeroColumnsAndUnicodeNames(t *testing.T) {
	r := pggo(t, "query", baseURL, "SELECT FROM generate_series(1, 3)")
	if rs := rows(t, r); len(rs) != 3 || !strings.Contains(r.raw, `"columns":[],"rows":[{},{},{}]`) {
		t.Fatalf("got %s", r.raw)
	}
	r = pggo(t, "query", baseURL, `SELECT 1 AS "é 🐘", 2 AS "with ""quote""", 3 AS "a\b"`)
	if !reflect.DeepEqual(r.m["columns"], []any{"é 🐘", `with "quote"`, `a\b`}) || rows(t, r)[0][`with "quote"`] != json.Number("2") {
		t.Fatalf("got %s", r.raw)
	}
}

// With client_encoding=UTF8 the server refuses to send invalid UTF-8, so a
// SQL_ASCII database holding stray bytes yields a structured 22021 error rather
// than broken JSON. (pggo's own replacement of invalid bytes is covered by unit/fuzz tests.)
func TestInvalidUTF8FromSQLASCIIDatabase(t *testing.T) {
	pggo(t, "exec", baseURL, "DROP DATABASE IF EXISTS it_ascii")
	setup(t, "CREATE DATABASE it_ascii ENCODING 'SQL_ASCII' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0")
	t.Cleanup(func() { pggo(t, "exec", baseURL, "DROP DATABASE IF EXISTS it_ascii") })
	u, _ := url.Parse(baseURL)
	u.Path = "/it_ascii"
	wantErr(t, pggo(t, "query", u.String(), "SELECT chr(255) AS v"), "postgres_error", "22021", false)
	if got := one2(t, u.String(), "SELECT 'plain ascii' AS v"); got != "plain ascii" {
		t.Fatalf("got %v", got)
	}
}

func one2(t *testing.T, u, sql string) any {
	t.Helper()
	rs := rows(t, pggo(t, "query", u, sql))
	return rs[0]["v"]
}

func TestOutputLimitsEdgeCases(t *testing.T) {
	// A single row larger than --max-bytes: zero rows, explicitly truncated.
	r := pggo(t, "query", baseURL, "SELECT repeat('x', 100000) AS big")
	if rs := rows(t, r); len(rs) != 0 || r.m["truncated"] != true || r.m["truncated_reason"] != "max_bytes" {
		t.Fatalf("got row_count=%v truncated=%v", r.m["row_count"], r.m["truncated"])
	}
	if len(r.raw) > 1024 {
		t.Fatalf("output %d bytes", len(r.raw))
	}
	// Output stays bounded for any mix of limits.
	for _, lim := range []struct{ rows, bytes int }{{1, 1 << 20}, {1000, 1000}, {5, 200}, {100, 65536}} {
		r := pggo(t, "query", baseURL, "SELECT g, md5(g::text) AS h FROM generate_series(1, 5000) g",
			"--max-rows", strconv.Itoa(lim.rows), "--max-bytes", strconv.Itoa(lim.bytes))
		rs := rows(t, r)
		if len(rs) > lim.rows || r.m["truncated"] != true {
			t.Fatalf("%+v: row_count=%d truncated=%v", lim, len(rs), r.m["truncated"])
		}
		body, _ := json.Marshal(r.m["rows"])
		if len(body) > lim.bytes+2 {
			t.Fatalf("%+v: rows JSON is %d bytes", lim, len(body))
		}
	}
	// max-rows 1 on exactly one row is not truncated.
	if r := pggo(t, "query", baseURL, "SELECT 1 AS x", "--max-rows", "1"); r.m["truncated"] != false {
		t.Fatalf("got %s", r.raw)
	}
}

func TestManyParamsAndColumns(t *testing.T) {
	var ph, cols []string
	args := []string{"query", baseURL}
	var params []string
	for i := 1; i <= 100; i++ {
		ph = append(ph, fmt.Sprintf("$%d::int", i))
		params = append(params, "--param", strconv.Itoa(i))
	}
	args = append(args, "SELECT "+strings.Join(ph, " + ")+" AS total")
	if rs := rows(t, pggo(t, append(args, params...)...)); rs[0]["total"] != json.Number("5050") {
		t.Fatalf("got %v", rs)
	}
	for i := 1; i <= 300; i++ {
		cols = append(cols, fmt.Sprintf("%d AS c%d", i, i))
	}
	r := pggo(t, "query", baseURL, "SELECT "+strings.Join(cols, ", "))
	if rs := rows(t, r); len(r.m["columns"].([]any)) != 300 || rs[0]["c300"] != json.Number("300") {
		t.Fatalf("columns=%d", len(r.m["columns"].([]any)))
	}
}

func TestParamRoundTrip(t *testing.T) {
	values := []string{"", " ", "multi\nline\ttab", `quote ' double " backslash \`, "unicode é 日本 🐘", "$1", "-- not a comment", "'; DROP TABLE x; --", strings.Repeat("long", 10000)}
	for _, v := range values {
		if got := one(t, "SELECT $1::text AS v", v); got != v {
			t.Fatalf("round trip of %.40q: got %.40q", v, got)
		}
	}
	// JSON params: numbers, bools, null, nested JSON.
	r := pggo(t, "query", baseURL, "SELECT $1::numeric AS n, $2::bool AS b, $3::int IS NULL AS isnull, $4::jsonb AS j, $5::text AS s",
		"--params", `[1.25, false, null, {"a": [1, {"b": null}]}, "x\"y"]`)
	rs := rows(t, r)
	want := map[string]any{"n": json.Number("1.25"), "b": false, "isnull": true, "j": map[string]any{"a": []any{json.Number("1"), map[string]any{"b": nil}}}, "s": `x"y`}
	if !reflect.DeepEqual(rs[0], want) {
		t.Fatalf("got %#v", rs[0])
	}
}

func TestLargeSQLFromStdin(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("SELECT count(*) AS n FROM (VALUES ")
	for i := 0; i < 50000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d)", i)
	}
	sb.WriteString(") v(x) WHERE x >= $1")
	cmd := exec.Command(bin, "query", baseURL, "-", "--param", "49990")
	cmd.Stdin = strings.NewReader(sb.String())
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), `"rows":[{"n":10}]`) {
		t.Fatalf("%v %s", err, out)
	}
}

func TestConnectionStringForms(t *testing.T) {
	u, _ := url.Parse(baseURL)
	pw, _ := u.User.Password()
	host, port := u.Hostname(), u.Port()
	db := strings.TrimPrefix(u.Path, "/")

	kv := fmt.Sprintf("host=%s port=%s user=%s password='%s' dbname=%s sslmode=disable", host, port, u.User.Username(), pw, db)
	wantOK(t, pggo(t, "ping", kv), "latency_ms")

	// libpq environment variables with no URL at all.
	cmd := exec.Command(bin, "info")
	cmd.Env = append(os.Environ(), "DATABASE_URL=", "PGHOST="+host, "PGPORT="+port, "PGUSER="+u.User.Username(),
		"PGPASSWORD="+pw, "PGDATABASE="+db, "PGSSLMODE=disable")
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), `"database":"`+db+`"`) {
		t.Fatalf("PG* env: %v %s", err, out)
	}

	// Passwords with URL-special characters.
	weird := `p@ss:w/rd%?#&=é`
	pggo(t, "exec", baseURL, "DROP ROLE IF EXISTS it_weird")
	setup(t, "CREATE ROLE it_weird LOGIN PASSWORD '"+strings.ReplaceAll(weird, "'", "''")+"'")
	wu := *u
	wu.User = url.UserPassword("it_weird", weird)
	wantOK(t, pggo(t, "ping", wu.String()), "latency_ms")

	// Unknown URL parameters become session settings.
	if got := one(t, "SELECT current_setting('application_name') AS v"); got != "pggo" {
		t.Fatalf("default application_name = %v", got)
	}
	r := pggo(t, "query", withParam(withParam(baseURL, "application_name", "agent-x"), "search_path", "pg_catalog"), "SELECT current_setting('application_name') AS a, current_setting('search_path') AS s")
	if rs := rows(t, r); rs[0]["a"] != "agent-x" || rs[0]["s"] != "pg_catalog" {
		t.Fatalf("got %s", r.raw)
	}
	// An unknown server setting is a clean postgres_error, not a crash.
	wantErr(t, pggo(t, "ping", withParam(baseURL, "no_such_setting", "1")), "postgres_error", "42704", false)
	// Nonexistent database.
	u2 := *u
	u2.Path = "/no_such_db"
	wantErr(t, pggo(t, "ping", u2.String()), "postgres_error", "3D000", false)
}

func TestTLSVerifyCA(t *testing.T) {
	root := os.Getenv("PGGO_TEST_SSLROOTCERT")
	if root == "" {
		t.Skip("PGGO_TEST_SSLROOTCERT not set (scripts/test-matrix.sh sets it)")
	}
	wantOK(t, pggo(t, "ping", withParam(withParam(baseURL, "sslmode", "verify-ca"), "sslrootcert", root)), "latency_ms")
	r := pggo(t, "query", withParam(baseURL, "sslmode", "require"), "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()")
	if rs := rows(t, r); rs[0]["ssl"] != true {
		t.Fatalf("sslmode=require did not use TLS: %s", r.raw)
	}
	if rs := rows(t, pggo(t, "query", baseURL, "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()")); rs[0]["ssl"] != false {
		t.Fatal("sslmode=disable used TLS")
	}

	// A CA that did not sign the server certificate must be rejected.
	other := filepath.Join(t.TempDir(), "other.pem")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "not the server"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	os.WriteFile(other, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	e := wantErr(t, pggo(t, "ping", withParam(withParam(baseURL, "sslmode", "verify-ca"), "sslrootcert", other)), "connection_error", "", false)
	if !strings.Contains(e["message"].(string), "TLS") {
		t.Fatalf("got %v", e)
	}
	wantErr(t, pggo(t, "ping", withParam(withParam(baseURL, "sslmode", "verify-ca"), "sslrootcert", "/no/such/file")), "invalid_input", "", false)
}

func TestConcurrentInvocations(t *testing.T) {
	setup(t, "DROP TABLE IF EXISTS it_conc", "CREATE TABLE it_conc (id int PRIMARY KEY)")
	var wg sync.WaitGroup
	errs := make(chan string, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var r response
			if i%2 == 0 {
				r = pggo(t, "exec", baseURL, "INSERT INTO it_conc VALUES ($1)", "--param", strconv.Itoa(i))
			} else {
				r = pggo(t, "query", baseURL, "SELECT $1::int AS i, pg_backend_pid() AS pid", "--param", strconv.Itoa(i))
			}
			if r.m["ok"] != true {
				errs <- r.raw
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if n := one(t, "SELECT count(*) AS n FROM it_conc"); n != json.Number("20") {
		t.Fatalf("inserted %v rows, want 20", n)
	}
}

func TestGoldenOutput(t *testing.T) {
	// Exact bytes (up to duration_ms). Any change here is a breaking change for agents.
	r := pggo(t, "query", baseURL, `SELECT 1 AS i, 1.5::float8 AS f, 12.50::numeric AS n, 'a"b'::text AS t, NULL::int AS z, true AS b, '{"k": [1, null]}'::jsonb AS j, DATE '2026-01-02' AS d`)
	want := `{"ok":true,"columns":["i","f","n","t","z","b","j","d"],"rows":[{"i":1,"f":1.5,"n":12.50,"t":"a\"b","z":null,"b":true,"j":{"k": [1, null]},"d":"2026-01-02"}],"row_count":1,"truncated":false,"duration_ms":`
	if !strings.HasPrefix(r.raw, want) {
		t.Fatalf("golden mismatch:\n got %s\nwant %s…", r.raw, want)
	}
	r = pggo(t, "exec", baseURL, "SELECT 1")
	if !strings.HasPrefix(r.raw, `{"ok":true,"command":"SELECT","rows_affected":1,"duration_ms":`) {
		t.Fatalf("exec golden: %s", r.raw)
	}
	r = pggo(t, "query", baseURL, "SELECT * FROM golden_missing")
	if r.raw != `{"ok":false,"error":{"type":"postgres_error","code":"42P01","message":"relation \"golden_missing\" does not exist","position":15,"retryable":false}}`+"\n" {
		t.Fatalf("error golden: %s", r.raw)
	}
	r = pggo(t, "ping", baseURL)
	if !strings.HasPrefix(r.raw, `{"ok":true,"latency_ms":`) {
		t.Fatalf("ping golden: %s", r.raw)
	}
	r = pggo(t, "info", baseURL)
	if !strings.HasPrefix(r.raw, `{"ok":true,"postgres_version":"`) || !strings.HasSuffix(r.raw, `,"read_only":false}`+"\n") {
		t.Fatalf("info golden: %s", r.raw)
	}
}

func TestTimeoutFlagForms(t *testing.T) {
	for _, flag := range [][]string{{"--timeout=400ms"}, {"--timeout", "0.4"}} {
		start := time.Now()
		wantErr(t, pggo(t, append([]string{"query", baseURL, "SELECT pg_sleep(10)"}, flag...)...), "timeout", "57014", true)
		if d := time.Since(start); d > 2500*time.Millisecond {
			t.Fatalf("%v took %v", flag, d)
		}
	}
	// exec is bounded too.
	wantErr(t, pggo(t, "exec", baseURL, "SELECT pg_sleep(10)", "--timeout", "300ms"), "timeout", "57014", true)
	// bench is bounded too.
	start := time.Now()
	r := pggo(t, "bench", baseURL, "--iterations", "10000", "--timeout", "300ms")
	if r.m["ok"] != false || time.Since(start) > 3*time.Second {
		t.Fatalf("bench not bounded: %s", r.raw)
	}
}
