package postgres

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	perr "github.com/pgrundev/pggo/internal/errors"
)

// Column describes one result column.
type Column struct {
	Name string
	Type uint32 // type OID
}

// Request is a single parameterized statement.
type Request struct {
	SQL      string
	Params   []*string // text-format values; nil means SQL NULL
	MaxRows  int       // server-side row limit for the portal; 0 = unlimited
	ReadOnly bool      // run inside BEGIN READ ONLY ... ROLLBACK
	// OnColumns, if set, receives the column list before any row.
	OnColumns func(cols []Column)
	// OnRow receives each row. vals[i] is nil for NULL and is only valid
	// during the call.
	OnRow func(vals [][]byte)
}

// Result is the outcome of a Request.
type Result struct {
	Columns    []Column
	CommandTag string
	Suspended  bool // the portal hit MaxRows before the result set ended
}

// Run executes req using the extended query protocol. All messages are
// pipelined in a single write, so a read-only query costs one round trip.
// The statement is canceled server-side when ctx expires.
func (c *Conn) Run(ctx context.Context, req *Request) (*Result, error) {
	c.w = c.w[:0]
	if req.ReadOnly {
		c.msg('Q', []byte("BEGIN READ ONLY\x00")...)
	}
	// Parse: unnamed statement, let the server infer all parameter types.
	c.w = append(c.w, 'P', 0, 0, 0, 0)
	start := len(c.w) - 4
	c.w = append(c.w, 0)
	c.w = append(append(c.w, req.SQL...), 0)
	c.w = append(c.w, 0, 0)
	binary.BigEndian.PutUint32(c.w[start:], uint32(len(c.w)-start))
	// Bind: unnamed portal, all parameters and results in text format.
	c.w = append(c.w, 'B', 0, 0, 0, 0)
	start = len(c.w) - 4
	c.w = append(c.w, 0, 0, 0, 0)
	c.w = binary.BigEndian.AppendUint16(c.w, uint16(len(req.Params)))
	for _, p := range req.Params {
		if p == nil {
			c.w = binary.BigEndian.AppendUint32(c.w, 0xFFFFFFFF)
			continue
		}
		c.w = binary.BigEndian.AppendUint32(c.w, uint32(len(*p)))
		c.w = append(c.w, *p...)
	}
	c.w = append(c.w, 0, 0)
	binary.BigEndian.PutUint32(c.w[start:], uint32(len(c.w)-start))
	c.msg('D', 'P', 0)
	c.msg('E', binary.BigEndian.AppendUint32([]byte{0}, uint32(req.MaxRows))...)
	c.msg('S')
	if req.ReadOnly {
		c.msg('Q', []byte("ROLLBACK\x00")...)
	}

	stop := context.AfterFunc(ctx, c.cancel)
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		c.nc.SetDeadline(dl.Add(cancelGrace))
	}
	defer c.nc.SetDeadline(time.Time{})

	if _, err := c.nc.Write(c.w); err != nil {
		return nil, c.runErr(err)
	}

	res := &Result{}
	var firstErr *perr.Error
	mainPhase, wantReady, ready := 0, 1, 0
	if req.ReadOnly {
		mainPhase, wantReady = 1, 3
	}
	vals := make([][]byte, 0, 16)
	for ready < wantReady {
		typ, body, err := c.recv()
		if err != nil {
			return nil, c.runErr(err)
		}
		switch typ {
		case 'T':
			if ready == mainPhase {
				res.Columns = parseRowDescription(body)
				if req.OnColumns != nil {
					req.OnColumns(res.Columns)
				}
			}
		case 'D':
			if ready != mainPhase || req.OnRow == nil {
				continue
			}
			vals = vals[:0]
			if len(body) < 2 {
				return nil, perr.New(perr.Protocol, "malformed DataRow")
			}
			n := int(binary.BigEndian.Uint16(body))
			p := body[2:]
			for i := 0; i < n; i++ {
				if len(p) < 4 {
					return nil, perr.New(perr.Protocol, "malformed DataRow")
				}
				l := int32(binary.BigEndian.Uint32(p))
				p = p[4:]
				if l < 0 {
					vals = append(vals, nil)
					continue
				}
				if int(l) > len(p) {
					return nil, perr.New(perr.Protocol, "malformed DataRow")
				}
				vals = append(vals, p[:l:l])
				p = p[l:]
			}
			req.OnRow(vals)
		case 'C':
			if ready == mainPhase {
				res.CommandTag = strings.TrimRight(string(body), "\x00")
			}
		case 's':
			res.Suspended = true
		case 'E':
			if firstErr == nil {
				firstErr = parseError(body)
			}
		case 'S':
			c.paramStatus(body)
		case 'Z':
			ready++
		case '1', '2', 'n', 'I', 'N', 'A':
		default:
			return nil, perr.New(perr.Protocol, "unexpected message %q", typ)
		}
	}
	if firstErr != nil {
		if firstErr.Code == "57014" && c.canceled.Load() {
			firstErr.Message = fmt.Sprintf("statement canceled: exceeded timeout of %s", c.timeout)
		}
		return nil, firstErr
	}
	return res, nil
}

func (c *Conn) runErr(err error) error {
	e := perr.Classify(err)
	if e.Type == perr.Timeout {
		e.Message = fmt.Sprintf("no response from server within timeout of %s", c.timeout)
	}
	return e
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
		cols = append(cols, Column{Name: name, Type: binary.BigEndian.Uint32(p[6:])})
		p = p[18:]
	}
	return cols
}

// ParseCommandTag splits "INSERT 0 3" into ("INSERT", 3) and "CREATE TABLE" into ("CREATE TABLE", 0).
func ParseCommandTag(tag string) (string, int64) {
	f := strings.Fields(tag)
	var n int64
	i := len(f)
	for i > 0 {
		v, err := strconv.ParseInt(f[i-1], 10, 64)
		if err != nil {
			break
		}
		if i == len(f) {
			n = v
		}
		i--
	}
	return strings.Join(f[:i], " "), n
}
