// Command pggo is a tiny PostgreSQL adapter for LLMs and coding agents.
// Every invocation prints exactly one JSON document to stdout.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pgrundev/pggo/internal/bench"
	perr "github.com/pgrundev/pggo/internal/errors"
	"github.com/pgrundev/pggo/internal/output"
	"github.com/pgrundev/pggo/internal/postgres"
)

const version = "0.0.1"

const (
	defaultTimeout    = 10 * time.Second
	defaultMaxRows    = 100
	defaultMaxBytes   = 64 * 1024
	defaultIterations = 10
)

type options struct {
	cmd        string
	url        string
	sql        string
	params     []*string
	timeout    time.Duration
	maxRows    int
	maxBytes   int
	iterations int
}

func main() {
	start := time.Now()
	out, err := run(os.Args[1:], start)
	if err != nil {
		output.Write(output.Error(perr.Classify(err)))
		os.Exit(1)
	}
	output.Write(out)
}

func run(args []string, start time.Time) ([]byte, error) {
	o, err := parseArgs(args)
	if err != nil {
		return nil, err
	}
	switch o.cmd {
	case "help":
		return helpJSON(), nil
	case "version":
		return output.NewObject().Bool("ok", true).String("version", version).Bytes(), nil
	}
	cfg, err := postgres.ParseURL(o.url)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = o.timeout
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	if o.cmd == "bench" {
		r, err := bench.Run(ctx, cfg, o.iterations)
		if err != nil {
			return nil, err
		}
		return output.NewObject().Bool("ok", true).Int("iterations", int64(o.iterations)).
			Raw("connect_ms", r.Connect.JSON()).Raw("select1_ms", r.Query.JSON()).
			Ms("duration_ms", msSince(start)).Bytes(), nil
	}

	conn, err := postgres.Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	switch o.cmd {
	case "ping":
		if _, err := conn.Run(ctx, &postgres.Request{SQL: "SELECT 1"}); err != nil {
			return nil, err
		}
		return output.NewObject().Bool("ok", true).Ms("latency_ms", msSince(start)).Bytes(), nil
	case "info":
		return info(ctx, conn)
	case "query":
		return query(ctx, conn, o, start)
	case "exec":
		res, err := conn.Run(ctx, &postgres.Request{SQL: o.sql, Params: o.params})
		if err != nil {
			return nil, err
		}
		cmd, n := postgres.ParseCommandTag(res.CommandTag)
		return output.NewObject().Bool("ok", true).String("command", cmd).Int("rows_affected", n).
			Ms("duration_ms", msSince(start)).Bytes(), nil
	}
	return nil, perr.New(perr.InvalidInput, "unknown command %q", o.cmd)
}

func info(ctx context.Context, conn *postgres.Conn) ([]byte, error) {
	var vals []string
	_, err := conn.Run(ctx, &postgres.Request{
		SQL: "SELECT current_database(), current_user, " +
			"(pg_is_in_recovery() OR current_setting('default_transaction_read_only') = 'on')::text",
		OnRow: func(v [][]byte) {
			for _, b := range v {
				vals = append(vals, string(b))
			}
		},
	})
	if err != nil {
		return nil, err
	}
	if len(vals) != 3 {
		return nil, perr.New(perr.Protocol, "unexpected info result")
	}
	return output.NewObject().Bool("ok", true).String("postgres_version", conn.ServerVersion()).
		String("database", vals[0]).String("user", vals[1]).Bool("read_only", vals[2] == "true").Bytes(), nil
}

func query(ctx context.Context, conn *postgres.Conn, o *options, start time.Time) ([]byte, error) {
	var (
		rows      = []byte{'['}
		count     int
		received  int
		byteLimit bool
		cols      []postgres.Column
		rowBuf    []byte
		names     [][]byte
	)
	req := &postgres.Request{SQL: o.sql, Params: o.params, ReadOnly: true, MaxRows: o.maxRows + 1}
	req.OnRow = func(vals [][]byte) {
		received++
		if count >= o.maxRows || byteLimit {
			return
		}
		rowBuf = append(rowBuf[:0], '{')
		for i, v := range vals {
			if i > 0 {
				rowBuf = append(rowBuf, ',')
			}
			rowBuf = append(append(rowBuf, names[i]...), ':')
			rowBuf = postgres.AppendValue(rowBuf, cols[i].Type, v)
		}
		rowBuf = append(rowBuf, '}')
		if len(rows)+len(rowBuf) > o.maxBytes {
			byteLimit = true
			return
		}
		if count > 0 {
			rows = append(rows, ',')
		}
		rows = append(rows, rowBuf...)
		count++
	}
	req.OnColumns = func(c []postgres.Column) {
		cols, names = c, columnKeys(c)
	}
	if _, err := conn.Run(ctx, req); err != nil {
		return nil, err
	}
	colJSON := []byte{'['}
	for i, n := range names {
		if i > 0 {
			colJSON = append(colJSON, ',')
		}
		colJSON = append(colJSON, n...)
	}
	colJSON = append(colJSON, ']')

	truncated := received > o.maxRows || byteLimit
	obj := output.NewObject().Bool("ok", true).Raw("columns", colJSON).Raw("rows", append(rows, ']')).
		Int("row_count", int64(count)).Bool("truncated", truncated)
	if truncated {
		reason := "max_rows"
		hint := fmt.Sprintf("result has more than %d rows; add WHERE/ORDER BY/LIMIT, or raise --max-rows", o.maxRows)
		if byteLimit {
			reason = "max_bytes"
			hint = fmt.Sprintf("result exceeds %d bytes of JSON; select fewer columns/rows, or raise --max-bytes", o.maxBytes)
		}
		obj.String("truncated_reason", reason).String("hint", hint)
	}
	return obj.Ms("duration_ms", msSince(start)).Bytes(), nil
}

// columnKeys returns JSON-encoded, de-duplicated column names ("id", "id_2", ...).
func columnKeys(cols []postgres.Column) [][]byte {
	keys := make([][]byte, len(cols))
	seen := map[string]bool{}
	for i, c := range cols {
		name := c.Name
		key := name
		for k := 2; seen[key]; k++ {
			key = name + "_" + strconv.Itoa(k)
		}
		seen[key] = true
		keys[i] = output.AppendString(nil, key)
	}
	return keys
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Nanoseconds()) / 1e6 }

var commandFlags = map[string][]string{
	"ping":    {"timeout"},
	"info":    {"timeout"},
	"query":   {"timeout", "param", "params", "max-rows", "max-bytes"},
	"exec":    {"timeout", "param", "params"},
	"bench":   {"timeout", "iterations"},
	"version": {},
	"help":    {},
}

func parseArgs(args []string) (*options, error) {
	o := &options{timeout: defaultTimeout, maxRows: defaultMaxRows, maxBytes: defaultMaxBytes, iterations: defaultIterations}
	if len(args) == 0 {
		o.cmd = "help"
		return o, nil
	}
	o.cmd = args[0]
	switch o.cmd {
	case "-h", "--help":
		o.cmd = "help"
	case "-v", "--version":
		o.cmd = "version"
	}
	allowed, ok := commandFlags[o.cmd]
	if !ok {
		name := o.cmd
		if len(name) > 40 {
			name = name[:40] + "..."
		}
		e := invalid("unknown command %q; commands: ping, info, query, exec, bench, version, help", name)
		e.Hint = "usage: pggo query 'SELECT * FROM users WHERE email = $1' --param alex@example.com (reads), " +
			"pggo exec 'UPDATE users SET active = $1 WHERE id = $2' --param true --param 42 (writes). " +
			"Put values in --param, never inside the SQL. Run `pggo help` for details."
		return nil, e
	}
	var pos []string
	var jsonParams string
	var hasParam, hasParams bool
	noMoreFlags := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if noMoreFlags || !strings.HasPrefix(a, "--") || a == "-" {
			pos = append(pos, a)
			continue
		}
		if a == "--" {
			noMoreFlags = true
			continue
		}
		name, val, hasVal := strings.Cut(a[2:], "=")
		if !contains(allowed, name) {
			return nil, invalid("unknown flag --%s for %s; valid flags: %s", name, o.cmd, flagList(allowed))
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, invalid("flag --%s requires a value", name)
			}
			i++
			val = args[i]
		}
		var err error
		switch name {
		case "param":
			v := val
			o.params = append(o.params, &v)
			hasParam = true
		case "params":
			jsonParams, hasParams = val, true
		case "timeout":
			o.timeout, err = parseTimeout(val)
		case "max-rows":
			o.maxRows, err = parsePositive(name, val)
		case "max-bytes":
			o.maxBytes, err = parsePositive(name, val)
		case "iterations":
			o.iterations, err = parsePositive(name, val)
			if err == nil && o.iterations > 10000 {
				err = invalid("--iterations must be <= 10000")
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if hasParam && hasParams {
		return nil, invalid("use either --param (repeatable) or --params (JSON array), not both")
	}
	if hasParams {
		p, err := parseJSONParams(jsonParams)
		if err != nil {
			return nil, err
		}
		o.params = p
	}

	switch o.cmd {
	case "help", "version":
		return o, nil
	case "query", "exec":
		switch len(pos) {
		case 1:
			o.sql = pos[0]
		case 2:
			o.url, o.sql = pos[0], pos[1]
		case 0:
			return nil, invalid("missing SQL; usage: pggo %s [DATABASE_URL] 'SQL' [--param VALUE]...", o.cmd)
		default:
			return nil, invalid("too many arguments (%d); quote the SQL as one argument and pass values with --param", len(pos))
		}
		if isURL(o.sql) {
			return nil, invalid("missing SQL after the connection URL; usage: pggo %s [DATABASE_URL] 'SQL' [--param VALUE]...", o.cmd)
		}
		if o.sql == "-" {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
			if err != nil {
				return nil, invalid("reading SQL from stdin: %v", err)
			}
			o.sql = string(b)
		}
		if strings.TrimSpace(o.sql) == "" {
			return nil, invalid("SQL is empty")
		}
		if err := checkPlaceholders(o.sql, len(o.params)); err != nil {
			return nil, err
		}
	default:
		if len(pos) > 1 {
			return nil, invalid("too many arguments; usage: pggo %s [DATABASE_URL]", o.cmd)
		}
		if len(pos) == 1 {
			o.url = pos[0]
		}
	}
	if o.url == "" {
		o.url = os.Getenv("DATABASE_URL")
	}
	return o, nil
}

func checkPlaceholders(sql string, n int) error {
	max := postgres.MaxPlaceholder(sql)
	switch {
	case max == n:
		return nil
	case max == 0:
		e := invalid("%d parameter(s) given but the SQL has no $1..$N placeholders", n)
		e.Hint = "replace each value in the SQL with $1, $2, ... in the order of the --param flags, e.g. " +
			"pggo exec 'UPDATE users SET active = $1 WHERE id = $2' --param true --param 42. " +
			"If you did write $1 inside double quotes, the shell expanded it; use single quotes"
		return e
	case n == 0:
		e := invalid("SQL uses placeholders up to $%d but no parameters were given", max)
		e.Hint = "pass one --param VALUE per placeholder, in order ($1 first)"
		return e
	default:
		e := invalid("SQL uses placeholders up to $%d but %d parameter(s) were given", max, n)
		e.Hint = "pass exactly one --param per placeholder, in order ($1 first)"
		return e
	}
}

func parseJSONParams(s string) ([]*string, error) {
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var raw []json.RawMessage
	if err := d.Decode(&raw); err != nil {
		return nil, invalid("--params must be a JSON array, e.g. '[42, \"text\", null, true]': %v", err)
	}
	out := make([]*string, len(raw))
	for i, r := range raw {
		r = bytes.TrimSpace(r)
		var v string
		switch {
		case string(r) == "null":
			continue
		case r[0] == '"':
			json.Unmarshal(r, &v)
		case r[0] == '{' || r[0] == '[':
			var buf bytes.Buffer
			json.Compact(&buf, r)
			v = buf.String()
		default: // number, true, false: their JSON text is valid PostgreSQL input
			v = string(r)
		}
		out[i] = &v
	}
	return out, nil
}

func parseTimeout(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		if n, err2 := strconv.ParseFloat(s, 64); err2 == nil {
			d, err = time.Duration(n*float64(time.Second)), nil
		}
	}
	if err != nil || d <= 0 {
		return 0, invalid("invalid --timeout %q; use a duration like 5s, 500ms, or 2m", s)
	}
	return d, nil
}

func parsePositive(name, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, invalid("--%s must be a positive integer, got %q", name, s)
	}
	return n, nil
}

func invalid(format string, args ...any) *perr.Error {
	return perr.New(perr.InvalidInput, format, args...)
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func flagList(f []string) string {
	if len(f) == 0 {
		return "(none)"
	}
	return "--" + strings.Join(f, ", --")
}
