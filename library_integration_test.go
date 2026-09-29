package pggo

// Library integration tests against a real PostgreSQL server:
//
//	PGGO_TEST_URL='postgres://pggo:pggo@127.0.0.1:55418/pggo?sslmode=disable' go test -run Lib .
//
// scripts/test-matrix.sh runs them on PostgreSQL 16, 17, 18 and 19.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func libURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("PGGO_TEST_URL")
	if u == "" {
		t.Skip("PGGO_TEST_URL not set")
	}
	return u
}

func libConn(t *testing.T, opts ...Option) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, libURL(t), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func bg(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustExec(t *testing.T, c *Conn, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(bg(t), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func pgCode(err error) string {
	var pe *PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestLibConnectClose(t *testing.T) {
	c := libConn(t)
	if c.PID() == 0 || c.ServerVersion() == "" || c.IsClosed() || c.TxStatus() != 'I' {
		t.Fatalf("pid=%d version=%q", c.PID(), c.ServerVersion())
	}
	var pid int
	if err := c.QueryRow(bg(t), "SELECT pg_backend_pid()").Scan(&pid); err != nil || uint32(pid) != c.PID() {
		t.Fatalf("PID mismatch: %d vs %d (%v)", pid, c.PID(), err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !c.IsClosed() {
		t.Fatal("not closed")
	}
	if _, err := c.Exec(bg(t), "SELECT 1"); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("use after close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal("second Close:", err)
	}
}

func TestLibQueryStreamsRows(t *testing.T) {
	c := libConn(t)
	rows, err := c.Query(bg(t), "SELECT g, 'row ' || g FROM generate_series(1, $1::int) g", 50000)
	if err != nil {
		t.Fatal(err)
	}
	n, sum := 0, 0
	for rows.Next() {
		var g int
		var s string
		if err := rows.Scan(&g, &s); err != nil {
			t.Fatal(err)
		}
		if s != fmt.Sprintf("row %d", g) {
			t.Fatalf("got %q", s)
		}
		n++
		sum += g
	}
	if rows.Err() != nil || n != 50000 || sum != 50000*50001/2 || rows.CommandTag() != "SELECT 50000" {
		t.Fatalf("err=%v n=%d tag=%s", rows.Err(), n, rows.CommandTag())
	}
	// Closing early drains the rest and leaves the connection usable.
	rows, _ = c.Query(bg(t), "SELECT generate_series(1, 100000)")
	rows.Next()
	rows.Close()
	var one int
	if err := c.QueryRow(bg(t), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("after early close: %v", err)
	}
}

func TestLibQueryRowAndErrNoRows(t *testing.T) {
	c := libConn(t)
	var version string
	if err := c.QueryRow(bg(t), "SELECT current_setting('server_version')").Scan(&version); err != nil || version == "" {
		t.Fatal(err)
	}
	var x int
	err := c.QueryRow(bg(t), "SELECT 1 WHERE false").Scan(&x)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("want ErrNoRows, got %v", err)
	}
	// Extra rows are ignored by QueryRow (only the first is scanned).
	if err := c.QueryRow(bg(t), "SELECT generate_series(7, 9)").Scan(&x); err != nil || x != 7 {
		t.Fatalf("got %d %v", x, err)
	}
	// Errors surface at Scan.
	if err := c.QueryRow(bg(t), "SELECT * FROM missing_table").Scan(&x); pgCode(err) != "42P01" {
		t.Fatalf("got %v", err)
	}
}

func TestLibExec(t *testing.T) {
	c := libConn(t)
	mustExec(t, c, "DROP TABLE IF EXISTS lib_exec")
	mustExec(t, c, "CREATE TABLE lib_exec (id int PRIMARY KEY, v text)")
	tag, err := c.Exec(bg(t), "INSERT INTO lib_exec SELECT g, 'v' FROM generate_series(1, 5) g")
	if err != nil || tag.RowsAffected() != 5 || tag.Command() != "INSERT" {
		t.Fatalf("%q %v", tag, err)
	}
	tag, _ = c.Exec(bg(t), "UPDATE lib_exec SET v = $1 WHERE id > $2", "w", 3)
	if tag.RowsAffected() != 2 || tag.String() != "UPDATE 2" {
		t.Fatalf("%q", tag)
	}
	tag, _ = c.Exec(bg(t), "DELETE FROM lib_exec WHERE id = $1", 1)
	if tag.RowsAffected() != 1 {
		t.Fatalf("%q", tag)
	}
	// Multiple statements are rejected (unnamed extended protocol).
	if _, err := c.Exec(bg(t), "SELECT 1; SELECT 2"); pgCode(err) != "42601" {
		t.Fatalf("stacked statements: %v", err)
	}
}

func TestLibParametersAndNULL(t *testing.T) {
	c := libConn(t)
	var (
		s   string
		i   int64
		b   bool
		f   float64
		ns  *string
		ni  *int64
		arr []string
		ia  []int32
		ts  time.Time
		raw []byte
	)
	when := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	err := c.QueryRow(bg(t), `SELECT $1::text, $2::int8, $3::bool, $4::float8, $5::text, $6::int8, $7::text[], $8::int4[], $9::timestamptz, $10::bytea`,
		"o'brien \"q\" \\ é", int64(math.MaxInt64), true, 1.5, nil, (*int64)(nil), []string{"a", "b c", `q"`, ""}, []int32{1, -2}, when, []byte{0, 0xff}).
		Scan(&s, &i, &b, &f, &ns, &ni, &arr, &ia, &ts, &raw)
	if err != nil {
		t.Fatal(err)
	}
	if s != "o'brien \"q\" \\ é" || i != math.MaxInt64 || !b || f != 1.5 || ns != nil || ni != nil {
		t.Fatalf("%q %d %v %v %v %v", s, i, b, f, ns, ni)
	}
	if !reflect.DeepEqual(arr, []string{"a", "b c", `q"`, ""}) || !reflect.DeepEqual(ia, []int32{1, -2}) {
		t.Fatalf("%q %v", arr, ia)
	}
	if !ts.Equal(when) || !reflect.DeepEqual(raw, []byte{0, 0xff}) {
		t.Fatalf("%v %v", ts, raw)
	}
	// Injection-shaped values are data, not SQL.
	var n int
	if err := c.QueryRow(bg(t), "SELECT count(*) FROM pg_class WHERE relname = $1", "x' OR '1'='1").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	// NULL into a non-pointer is an error, never a silent zero.
	var z string
	if err := c.QueryRow(bg(t), "SELECT NULL::text").Scan(&z); err == nil {
		t.Fatal("NULL into string must fail")
	}
	// Unsupported parameter types fail before anything is sent.
	if _, err := c.Exec(bg(t), "SELECT $1", struct{}{}); err == nil || !strings.Contains(err.Error(), "unsupported parameter type") {
		t.Fatalf("got %v", err)
	}
	if c.IsClosed() {
		t.Fatal("a client-side parameter error must not close the connection")
	}
}

// pgbotRow mirrors the kinds of fields PgBot scans with db tags.
type pgbotRow struct {
	Name     string     `db:"name"`
	Big      int64      `db:"big"`
	Small    int32      `db:"small"`
	Count    int        `db:"count"`
	Ratio    float64    `db:"ratio"`
	Flag     bool       `db:"flag"`
	At       *time.Time `db:"at"`
	MaybeF   *float64   `db:"maybe_f"`
	MaybeS   *string    `db:"maybe_s"`
	MaybeI   *int64     `db:"maybe_i"`
	Tags     []string   `db:"tags"`
	Keys     []int32    `db:"keys"`
	NotInSQL string     `db:"not_in_sql"` // lax: absent columns leave zero values
	Ignored  string     `db:"-"`
}

func TestLibPgBotTypesAndCollect(t *testing.T) {
	c := libConn(t)
	const q = `SELECT relname::text AS name, pg_relation_size(oid) AS big, relnatts::int4 AS small,
		count(*) OVER ()::int AS count, 0.25::numeric AS ratio, relhasindex AS flag,
		now() AS at, NULL::float8 AS maybe_f, NULL::text AS maybe_s, 42::int8 AS maybe_i,
		ARRAY['a','b'] AS tags, '1 3'::int2vector AS keys
		FROM pg_class WHERE relname IN ('pg_class', 'pg_attribute') ORDER BY relname`
	rows, err := c.Query(bg(t), q)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CollectStructs[pgbotRow](rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "pg_attribute" || got[1].Name != "pg_class" || got[0].Count != 2 {
		t.Fatalf("%+v", got)
	}
	r := got[1]
	if r.Big <= 0 || r.Small <= 0 || r.Ratio != 0.25 || !r.Flag || r.At == nil || r.MaybeF != nil || r.MaybeS != nil ||
		r.MaybeI == nil || *r.MaybeI != 42 || !reflect.DeepEqual(r.Tags, []string{"a", "b"}) || !reflect.DeepEqual(r.Keys, []int32{1, 3}) {
		t.Fatalf("%+v", r)
	}
	// Exactly one row.
	rows, _ = c.Query(bg(t), "SELECT 'x' AS name, 1::int8 AS big")
	one, err := CollectOneStruct[pgbotRow](rows)
	if err != nil || one.Name != "x" || one.Big != 1 {
		t.Fatalf("%+v %v", one, err)
	}
	rows, _ = c.Query(bg(t), "SELECT 'x' AS name WHERE false")
	if _, err := CollectOneStruct[pgbotRow](rows); !errors.Is(err, ErrNoRows) {
		t.Fatal(err)
	}
	rows, _ = c.Query(bg(t), "SELECT 'x' AS name FROM generate_series(1,2)")
	if _, err := CollectOneStruct[pgbotRow](rows); !errors.Is(err, ErrTooManyRows) {
		t.Fatal(err)
	}
	// A column without a field is an error.
	rows, _ = c.Query(bg(t), "SELECT 'x' AS name, 1 AS surprise")
	if _, err := CollectStructs[pgbotRow](rows); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("got %v", err)
	}
	// By position (untagged), and untagged field-name matching.
	type ext struct {
		Name   string
		Schema string
	}
	rows, _ = c.Query(bg(t), "SELECT 'plpgsql', 'pg_catalog'")
	byPos, err := CollectStructsByPos[ext](rows)
	if err != nil || len(byPos) != 1 || byPos[0].Schema != "pg_catalog" {
		t.Fatalf("%+v %v", byPos, err)
	}
	type untagged struct{ RelName string }
	rows, _ = c.Query(bg(t), "SELECT 'pg_class' AS rel_name")
	un, err := CollectStructs[untagged](rows)
	if err != nil || un[0].RelName != "pg_class" {
		t.Fatalf("%+v %v", un, err)
	}
	// Integer destinations: range checked, numeric must be whole.
	var i32 int32
	if err := c.QueryRow(bg(t), "SELECT 3000000000::int8").Scan(&i32); err == nil {
		t.Fatal("overflow must fail")
	}
	var i64 int64
	if err := c.QueryRow(bg(t), "SELECT 12.0::numeric").Scan(&i64); err != nil || i64 != 12 {
		t.Fatalf("%d %v", i64, err)
	}
	if err := c.QueryRow(bg(t), "SELECT 12.5::numeric").Scan(&i64); err == nil {
		t.Fatal("fractional numeric into int must fail")
	}
	var oid uint32
	if err := c.QueryRow(bg(t), "SELECT 'pg_class'::regclass::oid").Scan(&oid); err != nil || oid != 1259 {
		t.Fatalf("%d %v", oid, err)
	}
}

func TestLibValuesMatchPgx(t *testing.T) {
	c := libConn(t)
	rows, err := c.Query(bg(t), `SELECT 'x'::name, 'p'::"char", true, 1::int2, 2::int4, 3::int8, 1.5::float4, 2.5::float8, 12.30::numeric,
		'txt'::text, '2026-01-02 03:04:05+00'::timestamptz, '{"a":1}'::jsonb, ARRAY['a','b'], ARRAY[1,2]::int4[], 26::oid, NULL::text, '\xab'::bytea`)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	vals, err := rows.Values()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	// JSON of the values must equal what pgx produces for the same row
	// (PgBot marshals rows.Values() into its MCP output).
	b, _ := json.Marshal(append(vals[:10:10], vals[11:]...))
	want := `["x",112,true,1,2,3,1.5,2.5,12.30,"txt",{"a":1},["a","b"],[1,2],26,null,"qw=="]`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	if ts, ok := vals[10].(time.Time); !ok || ts.Location() != time.Local || !ts.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("timestamptz %#v", vals[10])
	}
}

func TestLibUnknownTypesAreRaw(t *testing.T) {
	c := libConn(t)
	mustExec(t, c, "DROP TYPE IF EXISTS lib_mood CASCADE")
	mustExec(t, c, "CREATE TYPE lib_mood AS ENUM ('ok', 'meh')")
	rows, err := c.Query(bg(t), `SELECT '(1,2)'::point, 'a fat cat'::tsvector, '0/16B3748'::pg_lsn, 'meh'::lib_mood, '1 day'::interval, NULL::point`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	rows.Next()
	raws := make([]RawValue, 6)
	dest := make([]any, 6)
	for i := range raws {
		dest[i] = &raws[i]
	}
	if err := rows.Scan(dest...); err != nil {
		t.Fatal(err)
	}
	want := []string{"(1,2)", "'a' 'cat' 'fat'", "0/16B3748", "meh", "1 day"}
	for i, w := range want {
		got := raws[i].String()
		if i == 2 {
			got = strings.Replace(got, "/0", "/", 1) // PostgreSQL 19 zero-pads pg_lsn output
		}
		if got != w || raws[i].OID == 0 || raws[i].Format != TextFormat || raws[i].IsNull() {
			t.Fatalf("col %d: %+v", i, raws[i])
		}
	}
	if !raws[5].IsNull() {
		t.Fatal("NULL raw value")
	}
	rows.Close()
	// Unknown types also scan into *string and come back from Values as text.
	var p string
	if err := c.QueryRow(bg(t), "SELECT '(3,4)'::point").Scan(&p); err != nil || p != "(3,4)" {
		t.Fatalf("%q %v", p, err)
	}
}

func TestLibErrorsCarrySQLSTATE(t *testing.T) {
	c := libConn(t)
	mustExec(t, c, "DROP TABLE IF EXISTS lib_err")
	mustExec(t, c, "CREATE TABLE lib_err (id int PRIMARY KEY, n int CHECK (n > 0))")
	mustExec(t, c, "INSERT INTO lib_err VALUES (1, 1)")
	cases := []struct {
		sql  string
		args []any
		code string
	}{
		{"SELEC 1", nil, "42601"},
		{"SELECT * FROM no_such_table", nil, "42P01"},
		{"INSERT INTO lib_err VALUES ($1, 1)", []any{1}, "23505"},
		{"INSERT INTO lib_err VALUES (2, $1)", []any{0}, "23514"},
		{"SELECT $1::int", []any{"abc"}, "22P02"},
		{"SELECT 1/0", nil, "22012"},
	}
	for _, tc := range cases {
		_, err := c.Exec(bg(t), tc.sql, tc.args...)
		var pe *PgError
		if !errors.As(err, &pe) || pe.Code != tc.code || pe.Severity != "ERROR" || pe.Message == "" {
			t.Fatalf("%s: got %v", tc.sql, err)
		}
		if tc.code == "23505" && pe.Detail != "Key (id)=(1) already exists." {
			t.Fatalf("detail %q", pe.Detail)
		}
		if tc.code == "42P01" && pe.Position != 15 {
			t.Fatalf("position %d", pe.Position)
		}
	}
	if c.IsClosed() {
		t.Fatal("server errors must not close the connection")
	}
}

func TestLibReadOnlyEnforcedByPostgres(t *testing.T) {
	admin := libConn(t)
	mustExec(t, admin, "DROP TABLE IF EXISTS lib_ro")
	mustExec(t, admin, "CREATE TABLE lib_ro (id int)")

	// 1. Session option: SET default_transaction_read_only = on.
	ro := libConn(t, ReadOnly(), StatementTimeout(5*time.Second), LockTimeout(2*time.Second))
	for _, sql := range []string{
		"INSERT INTO lib_ro VALUES (1)", "UPDATE lib_ro SET id = 2", "DELETE FROM lib_ro",
		"CREATE TABLE lib_ro2 (x int)", "DROP TABLE lib_ro", "TRUNCATE lib_ro",
		"WITH d AS (DELETE FROM lib_ro RETURNING *) SELECT * FROM d", "SELECT nextval('pg_class_oid_index')",
	} {
		if _, err := ro.Exec(bg(t), sql); err == nil {
			t.Fatalf("write allowed on read-only session: %s", sql)
		}
	}
	var st, lt string
	ro.QueryRow(bg(t), "SELECT current_setting('statement_timeout'), current_setting('lock_timeout')").Scan(&st, &lt)
	if st != "5s" || lt != "2s" {
		t.Fatalf("timeouts %q %q", st, lt)
	}

	// 2. Read-only transaction on a normal session.
	rw := libConn(t)
	tx, _ := rw.BeginTx(bg(t), TxOptions{ReadOnly: true})
	if _, err := tx.Exec(bg(t), "INSERT INTO lib_ro VALUES (1)"); pgCode(err) != "25006" {
		t.Fatalf("got %v", err)
	}
	tx.Rollback(bg(t))
	// A SELECT that is not simple text-wise still cannot write.
	tx, _ = rw.BeginTx(bg(t), TxOptions{ReadOnly: true})
	if _, err := tx.Exec(bg(t), "select/**/1; insert into lib_ro values (1)"); err == nil {
		t.Fatal("stacked write accepted")
	}
	tx.Rollback(bg(t))

	var n int
	admin.QueryRow(bg(t), "SELECT count(*) FROM lib_ro").Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows were written", n)
	}
	// SET LOCAL ... = DEFAULT returns to the server default, not the pin
	// (PgBot's settings collector relies on this).
	tx, _ = ro.BeginTx(bg(t), TxOptions{ReadOnly: true})
	tx.Exec(bg(t), "SET LOCAL statement_timeout = DEFAULT")
	tx.QueryRow(bg(t), "SELECT current_setting('statement_timeout')").Scan(&st)
	tx.Commit(bg(t))
	if st == "5s" {
		t.Fatal("SET LOCAL = DEFAULT kept the pinned value; options must be SETs, not startup params")
	}
}

func TestLibTransactions(t *testing.T) {
	c := libConn(t)
	mustExec(t, c, "DROP TABLE IF EXISTS lib_tx")
	mustExec(t, c, "CREATE TABLE lib_tx (id int)")
	tx, _ := c.Begin(bg(t))
	tx.Exec(bg(t), "INSERT INTO lib_tx VALUES (1)")
	if c.TxStatus() != 'T' {
		t.Fatalf("status %q", c.TxStatus())
	}
	if err := tx.Rollback(bg(t)); err != nil {
		t.Fatal(err)
	}
	tx, _ = c.Begin(bg(t))
	tx.Exec(bg(t), "INSERT INTO lib_tx VALUES (2)")
	if err := tx.Commit(bg(t)); err != nil {
		t.Fatal(err)
	}
	var ids []int32
	c.QueryRow(bg(t), "SELECT array_agg(id) FROM lib_tx").Scan(&ids)
	if !reflect.DeepEqual(ids, []int32{2}) {
		t.Fatalf("%v", ids)
	}
	// A failed transaction commits as a rollback.
	tx, _ = c.Begin(bg(t))
	tx.Exec(bg(t), "INSERT INTO lib_tx VALUES (3)")
	tx.Exec(bg(t), "SELECT 1/0")
	if err := tx.Commit(bg(t)); !errors.Is(err, ErrTxCommitRollback) {
		t.Fatalf("got %v", err)
	}
	// Savepoints (PgBot's advise loop).
	tx, _ = c.Begin(bg(t))
	tx.Exec(bg(t), "SAVEPOINT sp")
	tx.Exec(bg(t), "SELECT 1/0")
	tx.Exec(bg(t), "ROLLBACK TO SAVEPOINT sp")
	if _, err := tx.Exec(bg(t), "INSERT INTO lib_tx VALUES (4)"); err != nil {
		t.Fatal("savepoint did not recover:", err)
	}
	tx.Commit(bg(t))
	c.QueryRow(bg(t), "SELECT array_agg(id ORDER BY id) FROM lib_tx").Scan(&ids)
	if !reflect.DeepEqual(ids, []int32{2, 4}) {
		t.Fatalf("%v", ids)
	}
}

func TestLibContextCancellation(t *testing.T) {
	c := libConn(t)
	pid := c.PID()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Exec(ctx, "SELECT pg_sleep(30)")
	d := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || pgCode(err) != "57014" {
		t.Fatalf("want DeadlineExceeded + 57014, got %v", err)
	}
	if d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
	if !c.IsClosed() {
		t.Fatal("canceled connection must not look healthy")
	}
	// The server-side statement is gone.
	admin := libConn(t)
	var n int
	for i := 0; i < 20; i++ {
		admin.QueryRow(bg(t), "SELECT count(*) FROM pg_stat_activity WHERE pid = $1 AND state = 'active'", int(pid)).Scan(&n)
		if n == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n != 0 {
		t.Fatal("statement still running on the server")
	}
	// An already-expired context fails fast and leaves the connection intact.
	dead, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := admin.Exec(dead, "SELECT 1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if admin.IsClosed() {
		t.Fatal("pre-canceled context must not close the connection")
	}
	// Server-side statement_timeout is a clean 57014 without closing.
	st := libConn(t, StatementTimeout(200*time.Millisecond))
	if _, err := st.Exec(bg(t), "SELECT pg_sleep(5)"); pgCode(err) != "57014" || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if st.IsClosed() {
		t.Fatal("server-side timeout must keep the connection")
	}
}

func TestLibSimpleQueryGenericPlan(t *testing.T) {
	c := libConn(t)
	var major int
	c.QueryRow(bg(t), "SELECT current_setting('server_version_num')::int / 10000").Scan(&major)
	if major < 16 {
		t.Skip("GENERIC_PLAN needs PostgreSQL 16+")
	}
	res, err := c.SimpleQuery(bg(t), "EXPLAIN (GENERIC_PLAN, FORMAT JSON) SELECT * FROM pg_class WHERE oid = $1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || len(res[0].Rows) != 1 || !json.Valid(res[0].Rows[0][0]) || res[0].CommandTag != "EXPLAIN" {
		t.Fatalf("%+v", res)
	}
}

func TestLibPrepareDeallocate(t *testing.T) {
	c := libConn(t)
	if err := c.Prepare(bg(t), "lib_ps", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Prepare(bg(t), "lib_ps", "SELECT 1"); pgCode(err) != "42P05" {
		t.Fatalf("duplicate: %v", err)
	}
	if err := c.Deallocate(bg(t), "lib_ps"); err != nil {
		t.Fatal(err)
	}
}

func TestLibPool(t *testing.T) {
	cfg, err := ParseConfig(libURL(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["application_name"] = "lib_pool"
	var mu sync.Mutex
	live := map[uint32]bool{}
	var opened, closed atomic.Int32
	p := NewPool(PoolConfig{
		Config: cfg, MaxConns: 4, MaxConnLifetime: time.Minute,
		AfterConnect: func(ctx context.Context, c *Conn) error {
			opened.Add(1)
			mu.Lock()
			live[c.PID()] = true
			mu.Unlock()
			_, err := c.Exec(ctx, "SET default_transaction_read_only = on")
			return err
		},
		BeforeClose: func(c *Conn) {
			closed.Add(1)
			mu.Lock()
			delete(live, c.PID())
			mu.Unlock()
		},
	})
	defer p.Close()
	// 20 concurrent readers never exceed MaxConns backends.
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := p.BeginTx(bg(t), TxOptions{ReadOnly: true})
			if err != nil {
				errs <- err
				return
			}
			var n int
			if err := tx.QueryRow(bg(t), "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'lib_pool' AND pg_sleep(0.02) IS NOT NULL").Scan(&n); err != nil {
				errs <- err
			} else if n > 4 {
				errs <- fmt.Errorf("%d pool backends", n)
			}
			errs <- tx.Commit(bg(t))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if opened.Load() > 4 {
		t.Fatalf("opened %d connections", opened.Load())
	}
	// AfterConnect's pin applies to every pooled connection.
	if _, err := p.Exec(bg(t), "CREATE TEMP TABLE x (i int)"); pgCode(err) != "25006" {
		t.Fatalf("pin not applied: %v", err)
	}
	// Query/QueryRow release their connection when done.
	for i := 0; i < 10; i++ {
		var one int
		if err := p.QueryRow(bg(t), "SELECT 1").Scan(&one); err != nil {
			t.Fatal(err)
		}
		rows, _ := p.Query(bg(t), "SELECT generate_series(1, 3)")
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
	}
	// A canceled operation discards its connection; the pool replaces it.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	p.Exec(ctx, "SELECT pg_sleep(5)")
	cancel()
	if closed.Load() == 0 {
		t.Fatal("broken connection was returned to the pool")
	}
	var one int
	if err := p.QueryRow(bg(t), "SELECT 1").Scan(&one); err != nil {
		t.Fatal("pool did not recover:", err)
	}
	// Acquire all, as PgBot's Warm does: distinct backends.
	var held []*PoolConn
	pids := map[uint32]bool{}
	for i := 0; i < 4; i++ {
		pc, err := p.Acquire(bg(t))
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, pc)
		pids[pc.Conn().PID()] = true
	}
	if len(pids) != 4 {
		t.Fatalf("%d distinct backends", len(pids))
	}
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, err := p.Acquire(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("5th acquire should block until ctx: %v", err)
	}
	cancel()
	for _, pc := range held {
		pc.Release()
		pc.Release() // idempotent
	}
	// Max lifetime: an expired connection is replaced on next use.
	p2 := NewPool(PoolConfig{Config: cfg, MaxConns: 1, MaxConnLifetime: 50 * time.Millisecond})
	defer p2.Close()
	var pid1, pid2 int
	p2.QueryRow(bg(t), "SELECT pg_backend_pid()").Scan(&pid1)
	time.Sleep(100 * time.Millisecond)
	p2.QueryRow(bg(t), "SELECT pg_backend_pid()").Scan(&pid2)
	if pid1 == pid2 {
		t.Fatal("connection outlived MaxConnLifetime")
	}
}

func TestLibServiceFileAndPassfile(t *testing.T) {
	u, _ := url.Parse(libURL(t))
	pw, _ := u.User.Password()
	dir := t.TempDir()
	svc := filepath.Join(dir, "pg_service.conf")
	os.WriteFile(svc, []byte(fmt.Sprintf("# comment\n[other]\nhost=nowhere\n\n[lib]\nhost=%s\nport=%s\nuser=%s\ndbname=%s\nsslmode=disable\n",
		u.Hostname(), u.Port(), u.User.Username(), strings.TrimPrefix(u.Path, "/"))), 0o600)
	pass := filepath.Join(dir, "pgpass")
	os.WriteFile(pass, []byte(fmt.Sprintf("wrong:*:*:*:nope\n%s:%s:*:%s:%s\n", u.Hostname(), u.Port(), u.User.Username(), strings.ReplaceAll(pw, ":", `\:`))), 0o600)
	t.Setenv("PGSERVICEFILE", svc)
	t.Setenv("PGPASSFILE", pass)
	for _, v := range []string{"PGHOST", "PGPORT", "PGUSER", "PGDATABASE", "PGPASSWORD", "PGSERVICE"} {
		t.Setenv(v, "")
	}
	c, err := Connect(bg(t), "service=lib")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	t.Setenv("PGSERVICE", "lib")
	if c, err = Connect(bg(t), ""); err != nil {
		t.Fatal("PGSERVICE:", err)
	}
	c.Close()
	if _, err := ParseConfig("service=missing"); err == nil {
		t.Fatal("unknown service must fail")
	}
	// A world-readable passfile is ignored, as in libpq.
	os.Chmod(pass, 0o644)
	cfg, _ := ParseConfig("service=lib")
	if cfg.Password != "" {
		t.Fatal("insecure passfile was used")
	}
}

func TestLibDialFunc(t *testing.T) {
	cfg, _ := ParseConfig(libURL(t))
	real := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	var dials atomic.Int32
	cfg.Host = "db.via-tunnel.internal" // never resolved: the dialer decides
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", real)
	}
	c, err := ConnectConfig(bg(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The cancel request goes through the dialer too.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c.Exec(ctx, "SELECT pg_sleep(5)")
	if dials.Load() != 2 {
		t.Fatalf("%d dials; want connect + cancel", dials.Load())
	}
}

func TestLibTLS(t *testing.T) {
	c := libConn(t)
	var ssl bool
	c.QueryRow(bg(t), "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&ssl)
	u, _ := url.Parse(libURL(t))
	q := u.Query()
	q.Set("sslmode", "require")
	u.RawQuery = q.Encode()
	tc, err := Connect(bg(t), u.String())
	if err != nil {
		t.Skipf("server without TLS: %v", err)
	}
	defer tc.Close()
	tc.QueryRow(bg(t), "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&ssl)
	if !ssl {
		t.Fatal("sslmode=require did not use TLS")
	}
	if root := os.Getenv("PGGO_TEST_SSLROOTCERT"); root != "" {
		q.Set("sslmode", "verify-ca")
		q.Set("sslrootcert", root)
		u.RawQuery = q.Encode()
		vc, err := Connect(bg(t), u.String())
		if err != nil {
			t.Fatal("verify-ca:", err)
		}
		vc.Close()
	}
	q.Set("sslmode", "verify-full")
	q.Del("sslrootcert")
	u.RawQuery = q.Encode()
	var te *TLSError
	if _, err := Connect(bg(t), u.String()); !errors.As(err, &te) {
		t.Fatalf("self-signed cert passed verify-full: %v", err)
	}
}

func TestLibAuthSCRAM(t *testing.T) {
	c := libConn(t)
	var method string
	if err := c.QueryRow(bg(t), "SELECT left(rolpassword, 13) FROM pg_authid WHERE rolname = current_user").Scan(&method); err != nil {
		t.Skip("cannot read pg_authid:", err)
	}
	if method != "SCRAM-SHA-256" {
		t.Skipf("role password is %q, not SCRAM", method)
	}
	u, _ := url.Parse(libURL(t))
	u.User = url.UserPassword(u.User.Username(), "wrong")
	_, err := Connect(bg(t), u.String())
	if pgCode(err) != "28P01" {
		t.Fatalf("bad password: %v", err)
	}
	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatal("auth failure should be a ConnectError")
	}
}
