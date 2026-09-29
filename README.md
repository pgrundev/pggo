<div align="center">

# 🐘 pgGo

**A tiny, fast PostgreSQL adapter designed for LLMs and coding agents.**

JSON in, JSON out · structured errors · parameterized by default · always bounded

<br>

[![Go Reference](https://pkg.go.dev/badge/github.com/pgrundev/pggo.svg)](https://pkg.go.dev/github.com/pgrundev/pggo)
[![Go Report Card](https://goreportcard.com/badge/github.com/pgrundev/pggo)](https://goreportcard.com/report/github.com/pgrundev/pggo)
[![Go](https://img.shields.io/badge/go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16%20%7C%2017%20%7C%2018%20%7C%2019beta-4169E1?logo=postgresql&logoColor=white)](#tests)
<br>
[![Dependencies](https://img.shields.io/badge/dependencies-0-brightgreen)](go.mod)
[![Binary size](https://img.shields.io/badge/binary-4.0%20MB%20%C2%B7%201.7%20MB%20gzip-blue)](#benchmarks)
[![Agent tests](https://img.shields.io/badge/agent%20tests-Sonnet%206%2F6-8A2BE2)](#agent-tests)
[![GitHub stars](https://img.shields.io/github/stars/pgrundev/pggo?style=social)](https://github.com/pgrundev/pggo/stargazers)

[Commands](#commands) · [Agent contract](#the-agent-contract) · [Tests](#tests) · [Agent tests](#agent-tests) · [Benchmarks](#benchmarks)

</div>

---

Tiny PostgreSQL client for AI agents, written in Go.

**Zero dependencies. One binary. PostgreSQL wire protocol. JSON everywhere.**

```bash
pggo query "$DATABASE_URL" \
  'SELECT id, email FROM users WHERE id = $1' \
  --param 42
```

```json
{"ok":true,"columns":["id","email"],
 "rows":[{"id":42,"email":"alex@example.com"}],
 "row_count":1,"truncated":false,"duration_ms":8.5}
```

Built for agents:

→ deterministic JSON output  
→ structured PostgreSQL errors + SQLSTATE  
→ safe parameterized queries  
→ read-only `query`  
→ strict timeouts and server-side cancellation  
→ bounded output by default  
→ one statement per call  
→ zero dependencies

### Tiny

~1,800 lines of Go (excluding tests).

```text
Binary                4.03 MB
gzip                  1.68 MB
Peak memory           6.4 MiB
Connect + SELECT 1    8.5 ms p50
```

Tested against PostgreSQL 16, 17, 18 and 19 beta.

No pgx.  
No libpq.  
No psql parsing.

pgGo speaks the PostgreSQL protocol directly.

> **pgx is for humans and applications. pgGo is for agents.**

## Build

```bash
make build          # bin/pggo (go 1.22+, CGO_ENABLED=0, stripped)
make size           # reproducible size report for linux/amd64, linux/arm64, darwin/arm64
```

## Commands

| command | does |
|---|---|
| `pggo ping [URL]` | connect, authenticate, `SELECT 1` → `{"ok":true,"latency_ms":3.4}` |
| `pggo info [URL]` | `postgres_version`, `database`, `user`, `read_only` |
| `pggo query [URL] 'SQL' [--param V]...` | one **read-only** statement → rows as JSON objects |
| `pggo exec [URL] 'SQL' [--param V]...` | one statement that writes (DML/DDL), autocommit → `command`, `rows_affected` |
| `pggo bench [URL] [--iterations N]` | connect and warm `SELECT 1` latency: min/p50/p95/p99/max |
| `pggo help` / `pggo version` | self-description as JSON |

URL is a `postgres://` URL or libpq `key=value` string. If omitted, pggo uses `$DATABASE_URL`, then the libpq `PG*` variables.
`sslmode` supports `disable|allow|prefer|require|verify-ca|verify-full` with libpq semantics (default `prefer`).
Unknown URL parameters (e.g. `lock_timeout=2s`) are sent to the server as session settings.

| flag | default | |
|---|---|---|
| `--param VALUE` | | next `$N` value, repeatable, always sent separately from the SQL |
| `--params JSON` | | all values as a JSON array; the only way to pass `NULL` |
| `--timeout D` | `10s` | whole command: connect + execute |
| `--max-rows N` | `100` | query only |
| `--max-bytes N` | `65536` | query only; bytes of row JSON |
| `--iterations N` | `10` | bench only |

SQL `-` reads the statement from stdin.

## The agent contract

**Output.** One JSON object per line on stdout, nothing on stderr. Exit code 0 means `ok:true` and 1 means `ok:false`. Keys come out in a fixed order and row keys follow column order, so the same result always produces the same bytes (apart from `duration_ms`). Duplicate column names are suffixed (`id`, `id_2`).

**Types.** `bool` → JSON boolean. `int2/4/8`, `oid`, `float4/8` and `numeric` → JSON numbers, written verbatim so no precision is lost. `NaN`/`Infinity` → strings. `json`/`jsonb` → embedded JSON. `NULL` → `null`. Everything else (timestamps in ISO format, uuid, arrays, bytea, ...) → PostgreSQL's text form as a string.

**Errors.**

```json
{"ok":false,"error":{"type":"postgres_error","code":"42P01","message":"relation \"foo\" does not exist","position":15,"retryable":false}}
```

`type` is always one of `connection_error`, `authentication_error`, `timeout`, `postgres_error`, `invalid_input` or `protocol_error`. `code` is the SQLSTATE, or `null` when the error did not come from the server. `detail`, `hint` and `position` appear when available. `retryable` is true for network failures, timeouts, `40001` and `40P01`.
Classification is by SQLSTATE, so agents never need to parse messages: `28P01`/`28000` → authentication, `57014`/`55P03` → timeout, `08xxx` → connection.

**Safety defaults.**
- `query` runs inside `BEGIN READ ONLY … ROLLBACK`, pipelined in the same round trip. A write attempted via `query` fails with `25006` and a hint to use `exec`.
- Only one statement per call: the extended protocol rejects `a; b` (`42601`), which blocks stacked-query injection.
- Before contacting the server, pggo checks that the number of `$N` placeholders matches the number of `--param`s (skipping strings, comments and dollar-quotes). This catches the classic `"… $1"` shell-expansion bug and tells the agent how to fix it.
- On `--timeout`, pggo sends a PostgreSQL cancel request, so the statement stops on the server rather than running on. The process exits within `timeout + 1.5s`, even if the server never answers.
- `query` asks the server for at most `max-rows + 1` rows (portal row limit), so `SELECT * FROM events` on 100k rows costs about as much as 101 rows. Truncation is always explicit:
  `"truncated":true,"truncated_reason":"max_rows","hint":"result has more than 100 rows; add WHERE/ORDER BY/LIMIT, or raise --max-rows"`.

## Tests

```bash
make test           # unit tests (placeholder scanner, DSN, SCRAM RFC 7677 vector, value encoding, stats)
make test-matrix    # integration suite against PostgreSQL 16, 17, 18, 19beta1 (Docker, TLS on)
```

The integration suite (`integration/`) runs the real binary. Every error test checks the full JSON contract: exact key set, `type`, `code`, `retryable` and a non-empty message. It covers ping (ok, refused, bad password, TLS require and verify-full, MD5 and cleartext auth), info, and query results across the types (NULL, bool, ints, int8 > 2^53, float, NaN, numeric, text escaping, timestamp, json/jsonb, uuid, arrays, bytea). It also covers empty results, duplicate columns, parameters (injection strings, NULL through `--params`, placeholders inside literals), exec INSERT/UPDATE/DELETE/DDL, stacked statements, invalid SQL, a missing table, unique/check/not-null violations, lock timeouts (server `lock_timeout`, and `--timeout` while blocked), deadlock, connect timeout (a silent TCP server), statement timeout (checking that the backend is gone afterwards), 100k-row truncation, the byte limit, deterministic output, bench, and invalid input.

Result on 2026-09-29: **all pass on 16, 17, 18.6 and 19beta1.**

## Agent tests

```bash
make agent-test                                    # Sonnet, all tasks
AGENT_MODEL=haiku agent_tests/run.sh --runs 3      # smaller model, repeated
```

`agent_tests/harness.py` gives Claude Code a plain-English task plus one sentence: the database is at `$DATABASE_URL` and `pggo` is installed. It gets no docs or examples. The agent runs in an empty directory, may only run `pggo`, and has no MCP servers. A shim records every pggo call, and the checks look at *how* the agent used the tool (parameterized? `exec` for writes? bounded output?) as well as the final answer and the database state afterwards.

| task | checks |
|---|---|
| ping | a successful connectivity check; answer says the DB is available |
| find_user | `alex@example.com` passed via `--param`, never inside the SQL |
| activate | `exec` UPDATE with `--param`; user 42 is active; no other rows changed |
| diagnose | agent sees `42P01`, finds the real table, returns the email |
| all_events | "Show me all events" on 100k rows: no response > 70 KB; truncation or total reported |
| recent_events | "Show me recent events": bounded output; latest rows shown |

Results (2026-09-29):
- **Sonnet: 6/6.** It read `pggo --help` first and parameterized everything.
- **Haiku: 8/9** on repeated find_user, activate and all_events, after the error-hint changes below. It was 2/6 before them.

What the agent tests changed in pggo. Haiku does not read `--help`: it guesses `pggo 'SELECT …'` and inlines values. The terse "unknown command" error, and the placeholder-mismatch error, now include a worked `$1 … --param` example. After that, Haiku switched to parameterized queries on its next call in every find_user run.
**Known limitation:** if a model goes straight to a valid `pggo exec` with literal values, nothing fails and pggo does not rewrite it. pggo makes parameterization easy and teaches it through its errors, but it cannot force it.

## Benchmarks

```bash
make bench          # writes benchmarks/results/<timestamp>.md
```

`benchmarks/runner` compares `pggo`, `psql`, and `pgxmin` (the smallest reasonable pgx v5 program, `benchmarks/pgxmin`). It measures each separately: binary size, cold start, connect + `SELECT 1` in a new process, peak RSS, and warm `SELECT 1` on one connection, reporting p50/p95/p99.
The numbers below are from 2026-09-29: macOS arm64 (M-series, 10 CPUs), PostgreSQL 18.6 in Docker, `sslmode=disable`, 300 process samples and 5000 warm queries. Times are in ms.

| | pggo | pgxmin (pgx v5) | psql 18 |
|---|---|---|---|
| binary (stripped) | **4.03 MB** (1.68 MB gzip) | 8.89 MB | 0.73 MB + 0.36 MB libpq (+ system libs) |
| cold start p50 / p99 | 3.68 / 13.76 | 4.16 / 9.63 | 5.99 / 12.59 |
| connect + SELECT 1 p50 / p95 / p99 | **8.53** / 14.87 / 18.32 | 9.75 / 16.80 / 23.07 | 23.90 / 36.87 / 52.27 |
| peak RSS p50 | **6.4 MiB** | 9.0 MiB | 12.7 MiB |
| warm SELECT 1 p50 / p95 / p99 | 0.34 / 0.70 / 1.34 | 0.33 / 0.61 / 1.29 | 0.34 / 0.85 / 1.51 |

How to read this:
- **pggo is not faster than pgx at query execution.** Warm round trips are the same within noise; pgx is marginally ahead because it caches prepared statements.
- pggo's per-process edge (about 1 ms at p50) comes from the smaller binary and lower startup and memory cost. That edge is what matters for one-shot agent calls.
- The Docker-for-Mac network proxy dominates absolute latency, and tails are noisy on a laptop.

**On the ~1 MB target:** it was not reached. 1.6–1.8 MB gzipped (`make size`) is the practical floor for a Go binary that includes `crypto/tls`. The Go runtime and TLS/x509 make up almost all of it; pggo's own code is about 32 KB. Dropping TLS would get close to 1 MB, but TLS is required for nearly every hosted PostgreSQL, so it stays.

## Scope and limitations (v0.0.1)

- Auth: SCRAM-SHA-256, MD5, cleartext (all three covered by integration tests). There is no channel binding (`SCRAM-SHA-256-PLUS`), GSSAPI or client certificates.
- Parameters use server-side type inference. If a type is ambiguous (`SELECT $1`), you get text, or a `42P18` error; cast with `$1::int`.
- Arrays, ranges, intervals etc. come back as PostgreSQL text strings.
- `exec` returns no rows, so use `query` for `RETURNING`, and note that `query` is read-only. For v0.0.1, `INSERT … RETURNING` reports `rows_affected` only.
- Not included on purpose: ORM, migrations, pooling, schema tools, MCP, interactive shell.

## Layout

```
cmd/pggo/            CLI, argument parsing, JSON help
internal/postgres/   wire protocol: startup, TLS, auth (SCRAM/MD5), extended query, cancel, value → JSON
internal/errors/     the error contract and SQLSTATE classification
internal/output/     deterministic JSON writer
internal/bench/      pggo bench
integration/         black-box tests against real PostgreSQL
agent_tests/         LLM usability suite
benchmarks/          pgx baseline + comparison runner (separate go.mod, so pggo itself has zero deps)
scripts/             pg-up/pg-down (Docker PG 16–19), test matrix, bench
```
