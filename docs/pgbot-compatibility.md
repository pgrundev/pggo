# PgBot → pgGo compatibility audit

This audit covers PgBot at commit `08882b5` (module `github.com/pgrundev/pgbot`, Go 1.27, `github.com/jackc/pgx/v5 v5.10.0`).
It lists every PostgreSQL touchpoint in PgBot, and it decides what the pgGo library must provide.
The rule is that **only what PgBot actually uses gets implemented**.

## Where PgBot talks to PostgreSQL

pgx is imported by 13 non-test files and 14 test files:

- **`internal/conn`** (the connection layer): `connect.go`, `pooler.go`, `capability.go`.
- **`internal/collect`**, where 25 collectors go through three generic helpers in `collector.go` (`queryOne`, `queryMany`, `scalar`), plus direct pool queries in `health.go`, `ash.go` and `waitstudy.go`.
- **`internal/erd`**, `internal/pglog`, and the commands `activity`, `advise`, `logs` and `mcp_tools`.
- `database/sql` appears only in `internal/store` (the local SQLite baseline store via modernc), so it is **not PostgreSQL** and is out of scope.

## Compatibility table

| PgBot usage | Current API (pgx) | Required pgGo feature | Supported? |
|---|---|---|---|
| Parse the connection string: URL, `key=value`, `PG*` env | `pgxpool.ParseConfig`, `pgconn.ParseConfig` | `pggo.ParseConfig` | ✅ existing parser, now public |
| `$PGSERVICE` → `service=name` read from `PGSERVICEFILE` / `~/.pg_service.conf` (has its own test: `pgservice_test.go`) | pgx reads the service file | service-file lookup in `ParseConfig` | ✅ new: small INI reader |
| Password from `~/.pgpass` / `PGPASSFILE` when the DSN has none (relied on by the SSH-tunnel design) | pgx `pgpassfile` | passfile lookup in `ParseConfig` | ✅ new |
| Drop the client-only `channel_binding` param | deleted from `RuntimeParams` | already ignored by pgGo | ✅ existing |
| Override the target database (`--all-databases`) | `cfg.ConnConfig.Database = …` | `Config.Database` field | ✅ |
| `application_name = pgbot` | `RuntimeParams["application_name"]` | `Config.RuntimeParams` | ✅ |
| Route the TCP leg through an SSH jump host, with TLS negotiated **after** the dial | `ConnConfig.DialFunc` | `Config.DialFunc` | ✅ new |
| TLS `sslmode` disable…verify-full, `sslrootcert` | pgx TLS | existing pgGo TLS | ✅ existing |
| Client certificates (`sslcert`/`sslkey`) | not used anywhere in PgBot | — | ⛔ not needed, deferred |
| SCRAM-SHA-256 / MD5 / cleartext auth | pgx | existing pgGo auth | ✅ existing |
| Pool: max 4 conns, min 0, 5-minute lifetime, `AfterConnect` (session pins + register PID), `BeforeClose` (unregister PID) | `pgxpool.Pool`, `NewWithConfig` | minimal `pggo.Pool` with exactly these knobs | ✅ new, minimal (see "Pooling" below) |
| `Warm()`: acquire every connection and release it | `Pool.Acquire`, `Conn.Release` | `Pool.Acquire` / `PoolConn.Release` | ✅ |
| Backend PID for self-exclusion | `c.PgConn().PID()` | `Conn.PID()` | ✅ |
| Session pins: `SET statement_timeout / lock_timeout / idle_in_transaction_session_timeout / default_transaction_read_only / stats_fetch_consistency` | `Conn.Exec` in `AfterConnect` | `Conn.Exec` | ✅ (kept as SETs, not startup params, so `SET LOCAL … = DEFAULT` still unpins them) |
| Read-only transaction per collector, committed | `Pool.BeginTx(ReadOnly)`, `Tx.Commit`, `Tx.Rollback` | `BeginTx(TxOptions{ReadOnly:true})`, `Commit`, `Rollback` | ✅ new |
| `SET LOCAL …` inside the transaction; `SAVEPOINT` / `ROLLBACK TO` (advise) | `Tx.Exec` | `Tx.Exec` | ✅ |
| Query and stream rows | `Query`, `Rows.Next/Scan/Err/Close` | `Query`, streaming `Rows` | ✅ new |
| Single-row reads | `QueryRow(...).Scan` | `QueryRow`, `ErrNoRows` | ✅ new |
| Map rows onto structs by `db:"col"` tag, lax on missing fields (≈20 row types) | `pgx.CollectRows(rows, pgx.RowToStructByNameLax[T])` | `pggo.CollectStructs[T]` | ✅ new |
| Exactly one row onto a struct | `pgx.CollectExactlyOneRow(..., RowToStructByNameLax[T])` | `pggo.CollectOneStruct[T]` | ✅ new |
| Rows onto a struct by position (the extension probe) | `pgx.RowToStructByPos[T]` | `pggo.CollectStructsByPos[T]` | ✅ new |
| Rows as generic values for JSON (MCP `schema_of`) | `rows.FieldDescriptions()`, `rows.Values()` | `Rows.Columns()`, `Rows.Values()`, with pgx-identical Go types for the types used (e.g. `"char"` → `int32`) | ✅ new |
| Parameters `$1…$n`: `string`, `int`, `int64`, `uint32` (oid), `bool` | extended protocol | text-format parameters, unnamed statement | ✅ existing core |
| `EXPLAIN (GENERIC_PLAN)` sent byte-for-byte with bare `$1` and no binds | `PgConn().Exec(...).ReadAll()` (simple query protocol) | `Conn.SimpleQuery` | ✅ new |
| Prepared-statement probe (pooler detection) | `Conn.Prepare` / `Deallocate` | `Conn.Prepare` / `Deallocate` | ✅ new, minimal |
| Transaction-pooler fallback | `QueryExecModeSimpleProtocol` | not needed: pgGo only uses the **unnamed** statement, parsed and executed in one `Sync`, which transaction poolers (PgBouncer, Supavisor, PgDog) route as a unit. PgBot still runs its probe and reports it; the flag no longer changes the wire protocol. Verified through PgBouncer in transaction mode. | ✅ by design |
| Schema-qualified, quoted identifiers | `pgx.Identifier{…}.Sanitize()` | `pggo.QuoteIdentifier` | ✅ new, tiny |
| SQLSTATE (`42501`, …) | `*pgconn.PgError` + `errors.As` | `*pggo.PgError` + `errors.As` | ✅ |
| Context deadlines on every call; ASH polls with a per-poll budget | pgx ctx | ctx on every call; cancel request; connection marked broken if it cannot resync | ✅ |
| Scan targets (field types in all `db` structs plus direct scans) | pgx codecs | `string, int, int32, int64, uint32, float64, bool, time.Time, []byte, []string, []int32`, pointer (nullable) forms of each, `any`, `RawValue` | ✅ new |
| PostgreSQL types read | pgx codecs | `bool, int2/4/8, oid, float4/8, numeric, text, varchar, name, "char", timestamptz, timestamp, date, json/jsonb, text[], int2/4/8[], oid[], bytea, regclass-as-text`; any other type → text via `*string` or `RawValue` | ✅ |
| COPY | — | — | ⛔ not used |
| LISTEN / NOTIFY | — | — | ⛔ not used |
| Batch API | — | — | ⛔ not used |
| `database/sql` driver | — (only SQLite uses database/sql) | — | ⛔ not needed |
| Replication protocol, large objects | — | — | ⛔ not used |
| Multiple hosts / `target_session_attrs` | — | — | ⛔ not used |

## Pooling

The task says not to build a pool unless PgBot needs one. PgBot does. `collect/runner.go` runs collectors in parallel through two `errgroup`s with `SetLimit(4)`. ASH and lock sampling run in a background goroutine during the sampling window. `Warm` depends on 4 distinct backends being open at once so their PIDs can be excluded.

A single connection would serialize the whole inspection and change timing-sensitive results. That would be a behavior change, which the task forbids.

So pgGo gets a **minimal** pool containing exactly what `internal/conn` configures: `MaxConns`, `MaxConnLifetime`, `AfterConnect`, `BeforeClose`, and `Acquire`/`Release`, plus `Query`/`QueryRow`/`Exec`/`BeginTx` convenience methods.
It leaves out health checks, idle reaping, min-conns warmers, stats and statement caches.

## Session pins stay SETs

The task suggests `pggo.ReadOnly()`, `StatementTimeout()` and `LockTimeout()` options. pgGo provides them, and applies them with `SET` right after authentication. They are **not** startup parameters, for two reasons:

- Transaction poolers reject unknown startup parameters.
- PgBot's settings collector depends on `SET LOCAL x = DEFAULT` reverting to the *server's* value. A startup parameter would become the session default, so PgBot would report its own pins as the server's configuration.

PgBot keeps its own ordered pin list (four pins, plus `stats_fetch_consistency` on PG15+) in `AfterConnect`. PostgreSQL enforces them, not SQL-text inspection. The strongest guarantee remains the `pg_monitor` role with no write grants.

## Out of scope for this milestone

- Client certificates
- A general-purpose pool
- A binary result format
- Custom codecs
- Named-statement caching
- `database/sql`
- COPY
- LISTEN/NOTIFY
