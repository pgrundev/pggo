// Package integration runs the pggo binary against a real PostgreSQL server
// and checks the JSON contract of every response.
//
//	PGGO_TEST_URL='postgres://pggo:pggo@127.0.0.1:55418/pggo?sslmode=disable' go test ./integration/
//
// scripts/test-matrix.sh runs this suite against PostgreSQL 16, 17, 18 and 19.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgrundev/pggo/internal/postgres"
)

var (
	bin     string
	baseURL string
)

func TestMain(m *testing.M) {
	baseURL = os.Getenv("PGGO_TEST_URL")
	if baseURL == "" {
		fmt.Println("PGGO_TEST_URL not set; skipping integration tests")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "pggo-it")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "pggo")
	out, err := exec.Command("go", "build", "-o", bin, "../cmd/pggo").CombinedOutput()
	if err != nil {
		fmt.Printf("build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type response struct {
	raw  string
	exit int
	m    map[string]any
}

// pggo runs the binary and parses its single-line JSON output.
func pggo(t *testing.T, args ...string) response {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "DATABASE_URL=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	r := response{raw: string(out)}
	if ee, ok := err.(*exec.ExitError); ok {
		r.exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run pggo: %v", err)
	}
	if stderr.Len() > 0 {
		t.Errorf("pggo wrote to stderr: %s", stderr.String())
	}
	if strings.Count(r.raw, "\n") != 1 || !strings.HasSuffix(r.raw, "\n") {
		t.Fatalf("output must be exactly one JSON line, got %q", r.raw)
	}
	d := json.NewDecoder(strings.NewReader(r.raw))
	d.UseNumber()
	if err := d.Decode(&r.m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, r.raw)
	}
	return r
}

func keys(m map[string]any) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

func wantKeys(t *testing.T, m map[string]any, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := keys(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

// wantOK checks the success envelope and exact key set.
func wantOK(t *testing.T, r response, keys ...string) {
	t.Helper()
	if r.exit != 0 || r.m["ok"] != true {
		t.Fatalf("want ok, got exit=%d %s", r.exit, r.raw)
	}
	wantKeys(t, r.m, append(keys, "ok")...)
}

// wantErr checks the full error contract: envelope, type, SQLSTATE, retryable,
// and that no unexpected keys appear.
func wantErr(t *testing.T, r response, typ, code string, retryable bool) map[string]any {
	t.Helper()
	if r.exit != 1 || r.m["ok"] != false {
		t.Fatalf("want error exit=1 ok=false, got exit=%d %s", r.exit, r.raw)
	}
	wantKeys(t, r.m, "ok", "error")
	e, ok := r.m["error"].(map[string]any)
	if !ok {
		t.Fatalf("error is not an object: %s", r.raw)
	}
	allowed := map[string]bool{"type": true, "code": true, "message": true, "retryable": true, "detail": true, "hint": true, "position": true}
	for _, k := range []string{"type", "code", "message", "retryable"} {
		if _, ok := e[k]; !ok {
			t.Fatalf("error missing required key %q: %s", k, r.raw)
		}
	}
	for k := range e {
		if !allowed[k] {
			t.Fatalf("unexpected error key %q: %s", k, r.raw)
		}
	}
	if e["type"] != typ {
		t.Fatalf("error.type = %v, want %s: %s", e["type"], typ, r.raw)
	}
	if code == "" {
		if e["code"] != nil {
			t.Fatalf("error.code = %v, want null: %s", e["code"], r.raw)
		}
	} else if e["code"] != code {
		t.Fatalf("error.code = %v, want %s: %s", e["code"], code, r.raw)
	}
	if e["retryable"] != retryable {
		t.Fatalf("error.retryable = %v, want %v: %s", e["retryable"], retryable, r.raw)
	}
	if msg, _ := e["message"].(string); msg == "" {
		t.Fatalf("error.message empty: %s", r.raw)
	}
	return e
}

func rows(t *testing.T, r response) []map[string]any {
	t.Helper()
	if r.m["truncated"] == true {
		wantOK(t, r, "columns", "rows", "row_count", "truncated", "truncated_reason", "hint", "duration_ms")
	} else {
		wantOK(t, r, "columns", "rows", "row_count", "truncated", "duration_ms")
	}
	list := r.m["rows"].([]any)
	out := make([]map[string]any, len(list))
	for i, v := range list {
		out[i] = v.(map[string]any)
	}
	if n := r.m["row_count"].(json.Number).String(); n != fmt.Sprint(len(out)) {
		t.Fatalf("row_count %s != len(rows) %d", n, len(out))
	}
	return out
}

func one(t *testing.T, sql string, params ...string) any {
	t.Helper()
	args := []string{"query", baseURL, sql}
	for _, p := range params {
		args = append(args, "--param", p)
	}
	rs := rows(t, pggo(t, args...))
	if len(rs) != 1 || len(rs[0]) != 1 {
		t.Fatalf("want one row with one column, got %v", rs)
	}
	for _, v := range rs[0] {
		return v
	}
	return nil
}

func withParam(u, k, v string) string {
	p, err := url.Parse(u)
	if err != nil {
		panic(err)
	}
	q := p.Query()
	q.Set(k, v)
	p.RawQuery = q.Encode()
	return p.String()
}

func withPassword(u, pw string) string {
	p, _ := url.Parse(u)
	p.User = url.UserPassword(p.User.Username(), pw)
	return p.String()
}

func setup(t *testing.T, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if r := pggo(t, "exec", baseURL, s); r.m["ok"] != true {
			t.Fatalf("setup %q: %s", s, r.raw)
		}
	}
}

func TestPing(t *testing.T) {
	r := pggo(t, "ping", baseURL)
	wantOK(t, r, "latency_ms")
	if _, err := r.m["latency_ms"].(json.Number).Float64(); err != nil {
		t.Fatal(err)
	}
}

func TestPingFromDatabaseURLEnv(t *testing.T) {
	cmd := exec.Command(bin, "ping")
	cmd.Env = append(os.Environ(), "DATABASE_URL="+baseURL)
	out, err := cmd.Output()
	if err != nil || !strings.HasPrefix(string(out), `{"ok":true,"latency_ms":`) {
		t.Fatalf("ping via DATABASE_URL: %v %s", err, out)
	}
}

func TestPingConnectionFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens here now
	e := wantErr(t, pggo(t, "ping", "postgres://u:p@"+addr+"/db?sslmode=disable"), "connection_error", "", true)
	if !strings.Contains(e["message"].(string), "connection refused") {
		t.Fatalf("message should say connection refused: %v", e["message"])
	}
}

func TestPingAuthenticationFailure(t *testing.T) {
	wantErr(t, pggo(t, "ping", withPassword(baseURL, "wrong-password")), "authentication_error", "28P01", false)
}

// TestAuthMethods needs pg_hba rules for md5user/cleartextuser (scripts/pg-up.sh adds them).
func TestAuthMethods(t *testing.T) {
	if os.Getenv("PGGO_TEST_AUTH") == "" {
		t.Skip("set PGGO_TEST_AUTH=1 on a server prepared by scripts/pg-up.sh")
	}
	md5URL := withParam(baseURL, "password_encryption", "md5")
	setup(t, "DROP ROLE IF EXISTS md5user", "DROP ROLE IF EXISTS cleartextuser")
	if r := pggo(t, "exec", md5URL, "CREATE ROLE md5user LOGIN PASSWORD 'md5pw'"); r.m["ok"] != true {
		t.Fatal(r.raw)
	}
	setup(t, "CREATE ROLE cleartextuser LOGIN PASSWORD 'ctpw'")
	as := func(user, pw string) string {
		p, _ := url.Parse(baseURL)
		p.User = url.UserPassword(user, pw)
		if pw == "" {
			p.User = url.User(user)
		}
		return p.String()
	}
	wantOK(t, pggo(t, "ping", as("md5user", "md5pw")), "latency_ms")
	wantErr(t, pggo(t, "ping", as("md5user", "wrong")), "authentication_error", "28P01", false)
	wantOK(t, pggo(t, "ping", as("cleartextuser", "ctpw")), "latency_ms")
	wantErr(t, pggo(t, "ping", as("cleartextuser", "")), "authentication_error", "", false)
}

func TestPingTLS(t *testing.T) {
	r := pggo(t, "ping", withParam(baseURL, "sslmode", "require"))
	if r.m["ok"] != true {
		t.Skipf("server has no TLS: %s", r.raw)
	}
	e := wantErr(t, pggo(t, "ping", withParam(baseURL, "sslmode", "verify-full")), "connection_error", "", false)
	if !strings.Contains(e["message"].(string), "TLS") {
		t.Fatalf("want TLS error, got %v", e["message"])
	}
}

func TestInfo(t *testing.T) {
	r := pggo(t, "info", baseURL)
	wantOK(t, r, "postgres_version", "database", "user", "read_only")
	u, _ := url.Parse(baseURL)
	if r.m["database"] != strings.TrimPrefix(u.Path, "/") || r.m["user"] != u.User.Username() || r.m["read_only"] != false {
		t.Fatalf("info: %s", r.raw)
	}
	v := r.m["postgres_version"].(string)
	if v == "" || strings.Contains(v, " ") {
		t.Fatalf("postgres_version = %q", v)
	}
	if r := pggo(t, "info", withParam(baseURL, "default_transaction_read_only", "on")); r.m["read_only"] != true {
		t.Fatalf("read_only session: %s", r.raw)
	}
}

func TestQuerySelect1(t *testing.T) {
	r := pggo(t, "query", baseURL, "SELECT 1 AS one")
	rs := rows(t, r)
	if !reflect.DeepEqual(r.m["columns"], []any{"one"}) || rs[0]["one"] != json.Number("1") || r.m["truncated"] != false {
		t.Fatalf("got %s", r.raw)
	}
	if !strings.HasPrefix(r.raw, `{"ok":true,"columns":["one"],"rows":[{"one":1}],"row_count":1,"truncated":false,"duration_ms":`) {
		t.Fatalf("key order / formatting changed: %s", r.raw)
	}
}

func TestQueryMultipleRows(t *testing.T) {
	rs := rows(t, pggo(t, "query", baseURL, "SELECT g AS n, 'row ' || g AS label FROM generate_series(1, 5) g ORDER BY g"))
	if len(rs) != 5 || rs[4]["n"] != json.Number("5") || rs[4]["label"] != "row 5" {
		t.Fatalf("rows: %v", rs)
	}
}

func TestQueryTypes(t *testing.T) {
	cases := []struct {
		sql  string
		want any
	}{
		{"SELECT NULL::int AS v", nil},
		{"SELECT NULL::text AS v", nil},
		{"SELECT true AS v", true},
		{"SELECT false AS v", false},
		{"SELECT 42::int2 AS v", json.Number("42")},
		{"SELECT -42::int4 AS v", json.Number("-42")},
		{"SELECT 9007199254740993::int8 AS v", json.Number("9007199254740993")},
		{"SELECT 1.5::float8 AS v", json.Number("1.5")},
		{"SELECT 0.25::float4 AS v", json.Number("0.25")},
		{"SELECT 'NaN'::float8 AS v", "NaN"},
		{"SELECT 'Infinity'::float8 AS v", "Infinity"},
		{"SELECT 12345678901234567890.123456789::numeric AS v", json.Number("12345678901234567890.123456789")},
		{"SELECT 'NaN'::numeric AS v", "NaN"},
		{"SELECT 'hello'::text AS v", "hello"},
		{`SELECT E'quote " backslash \\ newline \n tab \t unicode é 日本' AS v`, "quote \" backslash \\ newline \n tab \t unicode é 日本"},
		{"SELECT 'x'::varchar(3) AS v", "x"},
		{"SELECT TIMESTAMP '2026-01-02 03:04:05.123456' AS v", "2026-01-02 03:04:05.123456"},
		{"SELECT DATE '2026-01-02' AS v", "2026-01-02"},
		{"SELECT '11111111-2222-3333-4444-555555555555'::uuid AS v", "11111111-2222-3333-4444-555555555555"},
		{"SELECT ARRAY[1,2,3] AS v", "{1,2,3}"},
		{"SELECT '\\xdeadbeef'::bytea AS v", "\\xdeadbeef"},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			if got := one(t, c.sql); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestQueryTimestamptzIsISO(t *testing.T) {
	got := one(t, "SELECT TIMESTAMPTZ '2026-01-02 03:04:05+00' AT TIME ZONE 'UTC' AS v")
	if got != "2026-01-02 03:04:05" {
		t.Fatalf("got %v", got)
	}
	got = one(t, "SELECT now() AS v")
	if s, _ := got.(string); len(s) < 19 || s[4] != '-' || s[10] != ' ' {
		t.Fatalf("timestamptz not ISO: %v", got)
	}
}

func TestQueryJSON(t *testing.T) {
	want := map[string]any{"a": []any{json.Number("1"), "two", nil}, "b": map[string]any{"c": true}}
	for _, typ := range []string{"json", "jsonb"} {
		got := one(t, `SELECT '{"a":[1,"two",null],"b":{"c":true}}'::`+typ+` AS v`)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %#v", typ, got)
		}
	}
	got := one(t, `SELECT $1::jsonb -> 'k' AS v`, `{"k":[1,2]}`)
	if !reflect.DeepEqual(got, []any{json.Number("1"), json.Number("2")}) {
		t.Fatalf("jsonb param: %#v", got)
	}
}

func TestQueryEmptyResult(t *testing.T) {
	r := pggo(t, "query", baseURL, "SELECT 1 AS a, 'x' AS b WHERE false")
	rs := rows(t, r)
	if len(rs) != 0 || !reflect.DeepEqual(r.m["columns"], []any{"a", "b"}) || r.m["truncated"] != false {
		t.Fatalf("got %s", r.raw)
	}
	if !strings.Contains(r.raw, `"rows":[]`) {
		t.Fatalf("empty rows must be [] not null: %s", r.raw)
	}
}

func TestQueryDuplicateColumnNames(t *testing.T) {
	r := pggo(t, "query", baseURL, "SELECT 1 AS id, 2 AS id, 3")
	rs := rows(t, r)
	if !reflect.DeepEqual(r.m["columns"], []any{"id", "id_2", "?column?"}) || rs[0]["id_2"] != json.Number("2") {
		t.Fatalf("got %s", r.raw)
	}
}

func TestQueryParameters(t *testing.T) {
	setup(t,
		"DROP TABLE IF EXISTS it_users",
		"CREATE TABLE it_users (id int PRIMARY KEY, email text, active bool, score float8, meta jsonb, created timestamptz)",
		"INSERT INTO it_users VALUES (42, 'alex@example.com', false, 1.5, '{\"plan\":\"pro\"}', '2026-01-02 03:04:05+00')",
		"INSERT INTO it_users VALUES (43, 'o''brien@example.com', true, NULL, NULL, NULL)",
	)
	r := pggo(t, "query", baseURL, "SELECT id, email FROM it_users WHERE id = $1", "--param", "42")
	if rs := rows(t, r); len(rs) != 1 || rs[0]["email"] != "alex@example.com" {
		t.Fatalf("got %s", r.raw)
	}
	// Values that would break naive interpolation must round-trip untouched.
	r = pggo(t, "query", baseURL, "SELECT id FROM it_users WHERE email = $1", "--param", "o'brien@example.com")
	if rs := rows(t, r); len(rs) != 1 || rs[0]["id"] != json.Number("43") {
		t.Fatalf("got %s", r.raw)
	}
	inj := "x' OR '1'='1"
	if rs := rows(t, pggo(t, "query", baseURL, "SELECT id FROM it_users WHERE email = $1", "--param", inj)); len(rs) != 0 {
		t.Fatalf("parameter was interpolated: %v", rs)
	}
	// Multiple typed params, bool/int/float/json/timestamptz inferred by the server.
	r = pggo(t, "query", baseURL,
		"SELECT count(*) AS n FROM it_users WHERE active = $1 AND id = $2 AND score = $3 AND meta @> $4 AND created = $5",
		"--param", "false", "--param", "42", "--param", "1.5", "--param", `{"plan":"pro"}`, "--param", "2026-01-02T03:04:05Z")
	if rs := rows(t, r); rs[0]["n"] != json.Number("1") {
		t.Fatalf("got %s", r.raw)
	}
	// --params JSON with NULL.
	r = pggo(t, "query", baseURL, "SELECT $1::int AS a, $2::text AS b, $3::text IS NULL AS c, $4::jsonb AS d", "--params", `[7, "s", null, {"k":[1]}]`)
	if rs := rows(t, r); rs[0]["a"] != json.Number("7") || rs[0]["b"] != "s" || rs[0]["c"] != true {
		t.Fatalf("got %s", r.raw)
	}
	// Placeholders inside strings/comments are not placeholders.
	if got := one(t, "SELECT '$1' || $1 /* $2 */ AS v -- $3", "x"); got != "$1x" {
		t.Fatalf("got %v", got)
	}
}

func TestQueryParameterValidation(t *testing.T) {
	e := wantErr(t, pggo(t, "query", baseURL, "SELECT id FROM t WHERE id = ", "--param", "42"), "invalid_input", "", false)
	if !strings.Contains(e["hint"].(string), "single quotes") {
		t.Fatalf("want shell-quoting hint: %v", e)
	}
	wantErr(t, pggo(t, "query", baseURL, "SELECT $1::int, $2::int", "--param", "1"), "invalid_input", "", false)
	wantErr(t, pggo(t, "query", baseURL, "SELECT $1::int"), "invalid_input", "", false)
	wantErr(t, pggo(t, "query", baseURL, "SELECT $1::int", "--params", "not json"), "invalid_input", "", false)
	wantErr(t, pggo(t, "query", baseURL, "SELECT $1::int", "--param", "1", "--params", "[1]"), "invalid_input", "", false)
	wantErr(t, pggo(t, "query", baseURL, "SELECT $1::int", "--param", "abc"), "postgres_error", "22P02", false)
}

func TestQueryIsReadOnly(t *testing.T) {
	setup(t, "DROP TABLE IF EXISTS it_ro", "CREATE TABLE it_ro (id int)")
	e := wantErr(t, pggo(t, "query", baseURL, "INSERT INTO it_ro VALUES (1)"), "postgres_error", "25006", false)
	if !strings.Contains(e["hint"].(string), "pggo exec") {
		t.Fatalf("hint should point to exec: %v", e)
	}
	if rs := rows(t, pggo(t, "query", baseURL, "SELECT count(*) AS n FROM it_ro")); rs[0]["n"] != json.Number("0") {
		t.Fatalf("write leaked through query: %v", rs)
	}
}

func TestExec(t *testing.T) {
	setup(t, "DROP TABLE IF EXISTS it_exec", "CREATE TABLE it_exec (id int PRIMARY KEY, active bool NOT NULL DEFAULT false)")
	check := func(r response, cmd string, n int) {
		t.Helper()
		wantOK(t, r, "command", "rows_affected", "duration_ms")
		if r.m["command"] != cmd || r.m["rows_affected"] != json.Number(fmt.Sprint(n)) {
			t.Fatalf("got %s, want %s %d", r.raw, cmd, n)
		}
	}
	check(pggo(t, "exec", baseURL, "INSERT INTO it_exec (id) VALUES ($1), ($2), ($3)", "--param", "1", "--param", "2", "--param", "42"), "INSERT", 3)
	check(pggo(t, "exec", baseURL, "UPDATE it_exec SET active = $1 WHERE id = $2", "--param", "true", "--param", "42"), "UPDATE", 1)
	check(pggo(t, "exec", baseURL, "UPDATE it_exec SET active = $1 WHERE id = $2", "--param", "true", "--param", "999"), "UPDATE", 0)
	check(pggo(t, "exec", baseURL, "DELETE FROM it_exec WHERE id < $1", "--param", "42"), "DELETE", 2)
	check(pggo(t, "exec", baseURL, "CREATE INDEX IF NOT EXISTS it_exec_active ON it_exec (active)"), "CREATE INDEX", 0)
	if rs := rows(t, pggo(t, "query", baseURL, "SELECT id, active FROM it_exec")); len(rs) != 1 || rs[0]["active"] != true {
		t.Fatalf("exec did not commit: %v", rs)
	}
	// Multiple statements are rejected by the extended protocol: no stacked-query injection.
	wantErr(t, pggo(t, "exec", baseURL, "DELETE FROM it_exec; DROP TABLE it_exec"), "postgres_error", "42601", false)
}

func TestInvalidSQL(t *testing.T) {
	e := wantErr(t, pggo(t, "query", baseURL, "SELEC 1"), "postgres_error", "42601", false)
	if e["position"] != json.Number("1") {
		t.Fatalf("want position 1: %v", e)
	}
}

func TestMissingTable(t *testing.T) {
	r := pggo(t, "query", baseURL, "SELECT * FROM does_not_exist")
	e := wantErr(t, r, "postgres_error", "42P01", false)
	if e["message"] != `relation "does_not_exist" does not exist` || e["position"] != json.Number("15") {
		t.Fatalf("got %s", r.raw)
	}
}

func TestConstraintViolations(t *testing.T) {
	setup(t, "DROP TABLE IF EXISTS it_c", "CREATE TABLE it_c (id int PRIMARY KEY, n int NOT NULL CHECK (n > 0))", "INSERT INTO it_c VALUES (1, 1)")
	e := wantErr(t, pggo(t, "exec", baseURL, "INSERT INTO it_c VALUES ($1, $2)", "--param", "1", "--param", "1"), "postgres_error", "23505", false)
	if e["detail"] != "Key (id)=(1) already exists." {
		t.Fatalf("want detail: %v", e)
	}
	wantErr(t, pggo(t, "exec", baseURL, "INSERT INTO it_c VALUES ($1, $2)", "--param", "2", "--param", "0"), "postgres_error", "23514", false)
	wantErr(t, pggo(t, "exec", baseURL, "INSERT INTO it_c VALUES ($1, NULL)", "--param", "3"), "postgres_error", "23502", false)
}

func connect(t *testing.T) *postgres.Conn {
	t.Helper()
	cfg, err := postgres.ParseURL(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := postgres.Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func run(t *testing.T, c *postgres.Conn, sql string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.Run(ctx, &postgres.Request{SQL: sql})
	return err
}

func TestLockTimeout(t *testing.T) {
	setup(t, "DROP TABLE IF EXISTS it_lock", "CREATE TABLE it_lock (id int PRIMARY KEY)", "INSERT INTO it_lock VALUES (1)")
	holder := connect(t)
	if err := run(t, holder, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if err := run(t, holder, "LOCK TABLE it_lock IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	defer run(t, holder, "ROLLBACK")

	// Server-side lock_timeout passed as a connection parameter.
	e := wantErr(t, pggo(t, "exec", withParam(baseURL, "lock_timeout", "200ms"), "UPDATE it_lock SET id = 2"), "timeout", "55P03", true)
	if !strings.Contains(e["message"].(string), "lock") {
		t.Fatalf("got %v", e)
	}
	// pggo's own --timeout cancels a statement stuck waiting on a lock.
	start := time.Now()
	wantErr(t, pggo(t, "exec", baseURL, "UPDATE it_lock SET id = 2", "--timeout", "500ms"), "timeout", "57014", true)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

func TestDeadlock(t *testing.T) {
	setup(t, "DROP TABLE IF EXISTS it_dl", "CREATE TABLE it_dl (id int PRIMARY KEY, v int)", "INSERT INTO it_dl VALUES (1, 0), (2, 0)")
	a, b := connect(t), connect(t)
	for _, c := range []*postgres.Conn{a, b} {
		if err := run(t, c, "BEGIN"); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(t, a, "UPDATE it_dl SET v = 1 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := run(t, b, "UPDATE it_dl SET v = 1 WHERE id = 2"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = run(t, a, "UPDATE it_dl SET v = 2 WHERE id = 2") }()
	time.Sleep(100 * time.Millisecond)
	go func() { defer wg.Done(); errs[1] = run(t, b, "UPDATE it_dl SET v = 2 WHERE id = 1") }()
	wg.Wait()
	run(t, a, "ROLLBACK")
	run(t, b, "ROLLBACK")
	var got error
	for _, err := range errs {
		if err != nil {
			got = err
		}
	}
	if got == nil {
		t.Fatal("expected a deadlock error")
	}
	if s := got.Error(); !strings.Contains(s, "40P01") || !strings.HasPrefix(s, "postgres_error") {
		t.Fatalf("got %v", got)
	}
}

func TestConnectionTimeout(t *testing.T) {
	// A server that accepts TCP but never speaks the protocol.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	start := time.Now()
	e := wantErr(t, pggo(t, "ping", "postgres://u:p@"+l.Addr().String()+"/db", "--timeout", "500ms"), "timeout", "", true)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("connect timeout took %v", d)
	}
	if !strings.Contains(e["message"].(string), "500ms") {
		t.Fatalf("message should mention the timeout: %v", e)
	}
}

func TestStatementTimeout(t *testing.T) {
	start := time.Now()
	e := wantErr(t, pggo(t, "query", baseURL, "SELECT pg_sleep(30)", "--timeout", "1s"), "timeout", "57014", true)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("statement timeout took %v", d)
	}
	if !strings.Contains(e["message"].(string), "1s") {
		t.Fatalf("got %v", e)
	}
	// The server-side statement must be gone, not left running.
	time.Sleep(200 * time.Millisecond)
	if n := one(t, "SELECT count(*) AS n FROM pg_stat_activity WHERE query = 'SELECT pg_sleep(30)'"); n != json.Number("0") {
		t.Fatalf("statement still running after timeout: %v", n)
	}
	// Server's own statement_timeout also maps to type timeout.
	wantErr(t, pggo(t, "query", withParam(baseURL, "statement_timeout", "100"), "SELECT pg_sleep(5)"), "timeout", "57014", true)
}

func TestLargeResultTruncation(t *testing.T) {
	setup(t, "DROP TABLE IF EXISTS it_events",
		"CREATE TABLE it_events (id bigint PRIMARY KEY, kind text, payload jsonb, created timestamptz)",
		"INSERT INTO it_events SELECT g, 'kind_' || (g % 7), jsonb_build_object('n', g), '2026-01-01'::timestamptz + g * interval '1 second' FROM generate_series(1, 100000) g",
	)
	start := time.Now()
	r := pggo(t, "query", baseURL, "SELECT * FROM it_events ORDER BY id")
	rs := rows(t, r)
	if len(rs) != 100 || r.m["truncated"] != true || r.m["truncated_reason"] != "max_rows" || r.m["hint"] == nil {
		t.Fatalf("default limit not applied: row_count=%v truncated=%v", r.m["row_count"], r.m["truncated"])
	}
	wantKeys(t, r.m, "ok", "columns", "rows", "row_count", "truncated", "truncated_reason", "hint", "duration_ms")
	if len(r.raw) > 64*1024+1024 {
		t.Fatalf("output too large: %d bytes", len(r.raw))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("truncated query took %v; server should stop after max rows", d)
	}

	// Exactly max-rows rows is not truncated.
	r = pggo(t, "query", baseURL, "SELECT id FROM it_events ORDER BY id LIMIT 10", "--max-rows", "10")
	if rs := rows(t, r); len(rs) != 10 || r.m["truncated"] != false {
		t.Fatalf("got row_count=%v truncated=%v", r.m["row_count"], r.m["truncated"])
	}
	r = pggo(t, "query", baseURL, "SELECT id FROM it_events ORDER BY id LIMIT 11", "--max-rows", "10")
	if rs := rows(t, r); len(rs) != 10 || r.m["truncated"] != true || rs[9]["id"] != json.Number("10") {
		t.Fatalf("got %v", r.m["row_count"])
	}

	// Byte limit.
	r = pggo(t, "query", baseURL, "SELECT id, repeat('x', 1000) AS big FROM it_events ORDER BY id", "--max-bytes", "5000")
	rs = rows(t, r)
	if len(rs) == 0 || len(rs) >= 5 || r.m["truncated"] != true || r.m["truncated_reason"] != "max_bytes" {
		t.Fatalf("byte limit: row_count=%v truncated=%v reason=%v", r.m["row_count"], r.m["truncated"], r.m["truncated_reason"])
	}

	// Raising the limit works and is still reported honestly.
	r = pggo(t, "query", baseURL, "SELECT id FROM it_events", "--max-rows", "100000", "--max-bytes", "100000000")
	if rs := rows(t, r); len(rs) != 100000 || r.m["truncated"] != false {
		t.Fatalf("got row_count=%v truncated=%v", r.m["row_count"], r.m["truncated"])
	}
}

func TestDeterministicOutput(t *testing.T) {
	strip := func(s string) string { return s[:strings.Index(s, `"duration_ms"`)] }
	sql := `SELECT g AS id, g::text AS s, g % 2 = 0 AS even, jsonb_build_object('z', 1, 'a', g) AS j FROM generate_series(1, 20) g ORDER BY g`
	a, b := pggo(t, "query", baseURL, sql), pggo(t, "query", baseURL, sql)
	if strip(a.raw) != strip(b.raw) {
		t.Fatalf("non-deterministic output:\n%s\n%s", a.raw, b.raw)
	}
	// Row keys follow column order, not alphabetical order.
	if !strings.Contains(a.raw, `{"id":1,"s":"1","even":false,"j":{"a": 1, "z": 1}}`) {
		t.Fatalf("key order: %s", a.raw)
	}
}

func TestBench(t *testing.T) {
	r := pggo(t, "bench", baseURL, "--iterations", "5")
	wantOK(t, r, "iterations", "connect_ms", "select1_ms", "duration_ms")
	for _, k := range []string{"connect_ms", "select1_ms"} {
		wantKeys(t, r.m[k].(map[string]any), "min", "p50", "p95", "p99", "max")
	}
}

func TestInvalidInput(t *testing.T) {
	cases := [][]string{
		{"frobnicate"},
		{"query", baseURL},
		{"query"},
		{"query", baseURL, "SELECT 1", "--bogus", "1"},
		{"query", baseURL, "SELECT 1", "--timeout", "soon"},
		{"query", baseURL, "SELECT 1", "--max-rows", "0"},
		{"exec", baseURL, "SELECT 1", "--max-rows", "5"},
		{"ping", "not a url"},
		{"ping", "postgres://h:99999/db"},
		{"query", baseURL, "   "},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			wantErr(t, pggo(t, args...), "invalid_input", "", false)
		})
	}
}

func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{}, {"help"}, {"--help"}} {
		r := pggo(t, args...)
		if r.m["ok"] != true || r.m["commands"] == nil {
			t.Fatalf("help: %s", r.raw)
		}
	}
	wantOK(t, pggo(t, "version"), "version")
}

func TestSQLFromStdin(t *testing.T) {
	cmd := exec.Command(bin, "query", baseURL, "-", "--param", "it's")
	cmd.Stdin = strings.NewReader("SELECT $1::text AS v")
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), `"rows":[{"v":"it's"}]`) {
		t.Fatalf("%v %s", err, out)
	}
}
