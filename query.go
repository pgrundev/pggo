package pggo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Column describes one result column.
type Column struct {
	Name    string
	TypeOID uint32
}

// CommandTag is the server's completion tag, e.g. "UPDATE 3" or "CREATE TABLE".
type CommandTag string

// RowsAffected is the trailing row count of the tag (0 if it has none).
func (t CommandTag) RowsAffected() int64 {
	s := string(t)
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(s[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Command is the tag without its row counts, e.g. "INSERT" for "INSERT 0 3".
func (t CommandTag) Command() string {
	f := strings.Fields(string(t))
	for len(f) > 0 {
		if _, err := strconv.ParseInt(f[len(f)-1], 10, 64); err != nil {
			break
		}
		f = f[:len(f)-1]
	}
	return strings.Join(f, " ")
}

func (t CommandTag) String() string { return string(t) }

// QueryOption changes how a single Query runs. Pass it among the arguments;
// it is not sent as a parameter.
type QueryOption interface{ queryOption() }

type maxRowsOption int32

func (maxRowsOption) queryOption() {}

// MaxRows asks the server to produce at most n rows (a portal row limit), so
// a large result is never computed or transmitted past n. Rows then reports
// Suspended() == true if the result had more.
func MaxRows(n int) QueryOption { return maxRowsOption(int32(n)) }

// Query runs one statement with $1..$n parameters and streams its rows. The
// caller must Close the Rows (or read them to the end) before the next call.
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (*Rows, error) {
	var maxRows int32
	params := args[:0:0]
	for _, a := range args {
		if o, ok := a.(maxRowsOption); ok {
			maxRows = int32(o)
			continue
		}
		params = append(params, a)
	}
	return c.start(ctx, sql, params, maxRows)
}

// QueryRow runs a query expected to return at most one row. Errors are
// deferred to Row.Scan, which returns ErrNoRows for an empty result.
func (c *Conn) QueryRow(ctx context.Context, sql string, args ...any) *Row {
	rows, err := c.Query(ctx, sql, args...)
	return &Row{rows: rows, err: err}
}

// Exec runs one statement, discarding any rows.
func (c *Conn) Exec(ctx context.Context, sql string, args ...any) (CommandTag, error) {
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		return "", err
	}
	for rows.Next() {
	}
	rows.Close()
	return rows.tag, rows.Err()
}

func (c *Conn) usable() error {
	if c.closed.Load() {
		return ErrConnClosed
	}
	if c.busy {
		return ErrConnBusy
	}
	return nil
}

// appendStatement appends Parse/Bind[/Describe]/Execute for one unnamed statement.
func (c *Conn) appendStatement(sql string, params [][]byte, describe bool, maxRows int32) {
	// Parse: unnamed statement; parameter types are inferred by the server.
	c.w = append(c.w, 'P', 0, 0, 0, 0)
	start := len(c.w) - 4
	c.w = append(c.w, 0)
	c.w = append(append(c.w, sql...), 0)
	c.w = append(c.w, 0, 0)
	binary.BigEndian.PutUint32(c.w[start:], uint32(len(c.w)-start))
	// Bind: unnamed portal; parameters and results in text format.
	c.w = append(c.w, 'B', 0, 0, 0, 0)
	start = len(c.w) - 4
	c.w = append(c.w, 0, 0, 0, 0)
	c.w = binary.BigEndian.AppendUint16(c.w, uint16(len(params)))
	for _, p := range params {
		if p == nil {
			c.w = binary.BigEndian.AppendUint32(c.w, 0xFFFFFFFF)
			continue
		}
		c.w = binary.BigEndian.AppendUint32(c.w, uint32(len(p)))
		c.w = append(c.w, p...)
	}
	c.w = append(c.w, 0, 0)
	binary.BigEndian.PutUint32(c.w[start:], uint32(len(c.w)-start))
	if describe {
		c.msg('D', 'P', 0)
	}
	c.msg('E', binary.BigEndian.AppendUint32([]byte{0}, uint32(maxRows))...)
}

// appendPending adds deferred statements (BEGIN) ahead of the next request.
// They share its Sync: if one fails, the server skips everything after it,
// so the request can never run outside the transaction it was meant for.
func (c *Conn) appendPending() int {
	n := len(c.pending)
	for _, s := range c.pending {
		c.appendStatement(s, nil, false, 0)
	}
	c.pending = c.pending[:0]
	return n
}

// watch arms cancellation for one request: when ctx fires, a cancel request
// is sent and the socket gets a short deadline. (No deadline needs clearing
// here: a connection whose context fired is closed, never reused.)
func (c *Conn) watch(ctx context.Context) func() bool {
	c.canceled.Store(false)
	return context.AfterFunc(ctx, c.cancelRequest)
}

func (c *Conn) start(ctx context.Context, sql string, args []any, maxRows int32) (*Rows, error) {
	if err := c.usable(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	params := make([][]byte, len(args))
	for i, a := range args {
		p, err := encodeParam(a)
		if err != nil {
			return nil, fmt.Errorf("pggo: parameter $%d: %w", i+1, err)
		}
		params[i] = p
	}
	c.w = c.w[:0]
	prefix := c.appendPending()
	c.appendStatement(sql, params, true, maxRows)
	c.msg('S')

	r := &Rows{c: c, ctx: ctx, prefix: prefix}
	r.stop = c.watch(ctx)
	c.busy = true
	if _, err := c.nc.Write(c.w); err != nil {
		r.fail(err)
		return nil, r.err
	}
	// Read up to the row description (or the end, for statements without rows).
	for !r.done && !r.header {
		r.step()
	}
	if r.err != nil {
		for !r.done {
			r.step()
		}
		return nil, r.err
	}
	return r, nil
}

// Rows streams the result of a query. It is not safe for concurrent use.
type Rows struct {
	c         *Conn
	ctx       context.Context
	stop      func() bool
	cols      []Column
	vals      [][]byte
	tag       CommandTag
	err       error
	prefix    int // CommandCompletes still owed to deferred statements
	header    bool
	done      bool
	suspended bool
	gotRow    bool // the last step delivered a DataRow
	onClose   func()
}

// step handles one message; it sets vals when a DataRow arrives.
func (r *Rows) step() {
	c := r.c
	typ, body, err := c.recv()
	if err != nil {
		r.fail(err)
		return
	}
	switch typ {
	case 'T':
		r.cols = parseRowDescription(body)
		r.header = true
	case 'n':
		r.header = true
	case 'D':
		if err := r.parseDataRow(body); err != nil {
			r.fail(err)
			return
		}
		r.gotRow = true
	case 'C':
		if r.prefix > 0 {
			r.prefix--
		} else {
			r.tag = CommandTag(strings.TrimRight(string(body), "\x00"))
			r.header = true
		}
	case 'I':
		r.header = true
	case 's':
		r.suspended = true
	case 'E':
		if r.err == nil {
			r.err = parseError(body)
		}
	case 'Z':
		if len(body) > 0 {
			c.txStatus = body[0]
		}
		r.finish()
	case 'S':
		c.paramStatus(body)
	case '1', '2', 'N', 'A':
	default:
		r.fail(&ProtocolError{fmt.Sprintf("unexpected message %q", typ)})
	}
}

func (r *Rows) parseDataRow(body []byte) error {
	if len(body) < 2 {
		return &ProtocolError{"malformed DataRow"}
	}
	n := int(binary.BigEndian.Uint16(body))
	p := body[2:]
	r.vals = r.vals[:0]
	for i := 0; i < n; i++ {
		if len(p) < 4 {
			return &ProtocolError{"malformed DataRow"}
		}
		l := int32(binary.BigEndian.Uint32(p))
		p = p[4:]
		if l < 0 {
			r.vals = append(r.vals, nil)
			continue
		}
		if int(l) > len(p) {
			return &ProtocolError{"malformed DataRow"}
		}
		r.vals = append(r.vals, p[:l:l])
		p = p[l:]
	}
	if len(r.cols) != n {
		return &ProtocolError{fmt.Sprintf("DataRow has %d values for %d columns", n, len(r.cols))}
	}
	return nil
}

// fail records a network/protocol failure and closes the connection.
func (r *Rows) fail(err error) {
	var ne net.Error
	switch {
	case r.c.canceled.Load() && (errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()):
		err = fmt.Errorf("%w: server did not acknowledge the cancel request", context.Cause(r.ctx))
	case r.err != nil:
		err = r.err // e.g. FATAL 57P01 then EOF: the server's error says more
	}
	r.err = err
	r.c.fatal(err)
	r.finish()
}

func (r *Rows) finish() {
	if r.done {
		return
	}
	r.done = true
	r.vals = nil
	c := r.c
	c.busy = false
	if !r.stop() || c.canceled.Load() {
		// The context fired: a cancel request may still be in flight and could
		// hit a later statement, so this connection must not be reused.
		// A request that completed (ReadyForQuery, no error) keeps its result:
		// it may have committed. A server error caused by the cancel is wrapped
		// so errors.Is(err, context.DeadlineExceeded) and errors.As(*PgError) both work.
		c.fatal(ErrConnClosed)
		var pe *PgError
		if errors.As(r.err, &pe) {
			r.err = fmt.Errorf("%w: %w", context.Cause(r.ctx), r.err)
		}
	}
	if r.onClose != nil {
		r.onClose()
		r.onClose = nil
	}
}

// Next advances to the next row. It returns false at the end or on error; check Err.
func (r *Rows) Next() bool {
	for !r.done {
		r.gotRow = false
		r.step()
		if r.gotRow {
			return true
		}
	}
	return false
}

// Columns describes the result columns.
func (r *Rows) Columns() []Column { return r.cols }

// RawValues returns the current row's values in PostgreSQL text format (nil
// for NULL). The slices are only valid until the next call to Next.
func (r *Rows) RawValues() [][]byte { return r.vals }

// CommandTag is available after the rows have been read to the end.
func (r *Rows) CommandTag() CommandTag { return r.tag }

// Suspended reports whether a MaxRows limit cut the result short.
func (r *Rows) Suspended() bool { return r.suspended }

// Err returns the error, if any, that ended iteration.
func (r *Rows) Err() error { return r.err }

// Close reads any remaining rows and releases the connection. It is safe to
// call more than once.
func (r *Rows) Close() {
	for !r.done {
		r.step()
	}
}

// Row is the result of QueryRow.
type Row struct {
	rows *Rows
	err  error
}

// Scan copies the first row into dest and closes the rows. It returns
// ErrNoRows if there were none.
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	rows := r.rows
	if !rows.Next() {
		rows.Close()
		if rows.Err() != nil {
			return rows.Err()
		}
		return ErrNoRows
	}
	err := rows.Scan(dest...)
	rows.Close()
	if err != nil {
		return err
	}
	return rows.Err()
}

// roundTrip sends c.w, then reads to ReadyForQuery, passing each message to
// handle. It returns the first server error.
func (c *Conn) roundTrip(ctx context.Context, handle func(typ byte, body []byte) error) error {
	r := &Rows{c: c, ctx: ctx}
	r.stop = c.watch(ctx)
	c.busy = true
	if _, err := c.nc.Write(c.w); err != nil {
		r.fail(err)
		return r.err
	}
	for !r.done {
		typ, body, err := c.recv()
		if err != nil {
			r.fail(err)
			break
		}
		switch typ {
		case 'E':
			if r.err == nil {
				r.err = parseError(body)
			}
		case 'Z':
			if len(body) > 0 {
				c.txStatus = body[0]
			}
			r.finish()
		case 'S':
			c.paramStatus(body)
		case 'N', 'A':
		default:
			if err := handle(typ, body); err != nil {
				r.fail(err)
			}
		}
	}
	return r.err
}

// flushPending runs deferred statements now (before a request that cannot
// share their Sync).
func (c *Conn) flushPending(ctx context.Context) error {
	if len(c.pending) == 0 {
		return nil
	}
	c.w = c.w[:0]
	c.appendPending()
	c.msg('S')
	return c.roundTrip(ctx, func(byte, []byte) error { return nil })
}

// Result is one statement's result from SimpleQuery. Values are text format.
type Result struct {
	Columns    []Column
	Rows       [][][]byte
	CommandTag CommandTag
}

// SimpleQuery sends sql with the simple query protocol: no parameters, the
// text reaches the server byte for byte (so "$1" stays literal, as EXPLAIN
// (GENERIC_PLAN) needs), and it may contain several statements.
func (c *Conn) SimpleQuery(ctx context.Context, sql string) ([]Result, error) {
	if err := c.usable(); err != nil {
		return nil, err
	}
	if err := c.flushPending(ctx); err != nil {
		return nil, err
	}
	c.w = c.w[:0]
	c.msg('Q', append([]byte(sql), 0)...)
	var out []Result
	cur := -1
	err := c.roundTrip(ctx, func(typ byte, body []byte) error {
		switch typ {
		case 'T':
			out = append(out, Result{Columns: parseRowDescription(body)})
			cur = len(out) - 1
		case 'D':
			if cur < 0 {
				return &ProtocolError{"DataRow before RowDescription"}
			}
			r := &Rows{cols: out[cur].Columns}
			if err := r.parseDataRow(body); err != nil {
				return err
			}
			row := make([][]byte, len(r.vals))
			for i, v := range r.vals {
				if v != nil {
					row[i] = append([]byte{}, v...)
				}
			}
			out[cur].Rows = append(out[cur].Rows, row)
		case 'C':
			tag := CommandTag(strings.TrimRight(string(body), "\x00"))
			if cur >= 0 && out[cur].CommandTag == "" {
				out[cur].CommandTag = tag
			} else {
				out = append(out, Result{CommandTag: tag})
			}
			cur = -1
		case 'I':
		default:
			return &ProtocolError{fmt.Sprintf("unexpected message %q", typ)}
		}
		return nil
	})
	return out, err
}

// Prepare creates a named prepared statement on the server.
func (c *Conn) Prepare(ctx context.Context, name, sql string) error {
	if err := c.usable(); err != nil {
		return err
	}
	if err := c.flushPending(ctx); err != nil {
		return err
	}
	c.w = c.w[:0]
	c.msg('P', append(append(append(append([]byte(name), 0), sql...), 0), 0, 0)...)
	c.msg('S')
	return c.roundTrip(ctx, func(typ byte, _ []byte) error {
		if typ != '1' {
			return &ProtocolError{fmt.Sprintf("unexpected message %q", typ)}
		}
		return nil
	})
}

// Deallocate drops a named prepared statement.
func (c *Conn) Deallocate(ctx context.Context, name string) error {
	if err := c.usable(); err != nil {
		return err
	}
	if err := c.flushPending(ctx); err != nil {
		return err
	}
	c.w = c.w[:0]
	c.msg('C', append(append([]byte{'S'}, name...), 0)...)
	c.msg('S')
	return c.roundTrip(ctx, func(typ byte, _ []byte) error {
		if typ != '3' {
			return &ProtocolError{fmt.Sprintf("unexpected message %q", typ)}
		}
		return nil
	})
}

func parseRowDescription(body []byte) []Column {
	if len(body) < 2 {
		return nil
	}
	n := int(binary.BigEndian.Uint16(body))
	p := body[2:]
	cols := make([]Column, 0, n)
	for i := 0; i < n; i++ {
		end := 0
		for end < len(p) && p[end] != 0 {
			end++
		}
		if end+19 > len(p) {
			break
		}
		name := string(p[:end])
		p = p[end+1:]
		cols = append(cols, Column{Name: name, TypeOID: binary.BigEndian.Uint32(p[6:])})
		p = p[18:]
	}
	return cols
}
