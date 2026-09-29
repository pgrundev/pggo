package pggo

import (
	"errors"
	"fmt"
)

var (
	// ErrNoRows is returned by Row.Scan and CollectOneStruct when the query returned no rows.
	ErrNoRows = errors.New("no rows in result set")
	// ErrTooManyRows is returned by CollectOneStruct when the query returned more than one row.
	ErrTooManyRows = errors.New("too many rows in result set")
	// ErrConnBusy means a previous Rows is still open on this connection.
	ErrConnBusy = errors.New("conn busy: close the previous Rows first")
	// ErrConnClosed means the connection was closed, either explicitly or because
	// an earlier operation was canceled or failed at the network level.
	ErrConnClosed = errors.New("conn closed")
	// ErrTxClosed means Commit or Rollback was already called.
	ErrTxClosed = errors.New("tx is closed")
	// ErrTxCommitRollback means COMMIT was answered with ROLLBACK because the
	// transaction had already failed.
	ErrTxCommitRollback = errors.New("commit unexpectedly resulted in rollback")
)

// PgError is an error reported by the PostgreSQL server. Code is the SQLSTATE;
// classify errors by Code, never by Message.
type PgError struct {
	Severity string
	Code     string
	Message  string
	Detail   string
	Hint     string
	Position int // 1-based character offset into the statement, 0 if unknown
	Where    string
}

func (e *PgError) Error() string {
	return e.Severity + ": " + e.Message + " (SQLSTATE " + e.Code + ")"
}

// ConnectError wraps any failure while establishing a connection.
type ConnectError struct {
	Addr string // host:port or unix socket path
	Err  error
}

func (e *ConnectError) Error() string {
	return fmt.Sprintf("connect to %s: %v", e.Addr, e.Err)
}

func (e *ConnectError) Unwrap() error { return e.Err }

// parseError decodes an ErrorResponse message body.
func parseError(body []byte) *PgError {
	e := &PgError{}
	for len(body) > 1 {
		f := body[0]
		end := 1
		for end < len(body) && body[end] != 0 {
			end++
		}
		v := string(body[1:end])
		switch f {
		case 'S':
			e.Severity = v
		case 'V':
			e.Severity = v // non-localized severity, preferred when present
		case 'C':
			e.Code = v
		case 'M':
			e.Message = v
		case 'D':
			e.Detail = v
		case 'H':
			e.Hint = v
		case 'P':
			for _, c := range v {
				if c < '0' || c > '9' {
					break
				}
				e.Position = e.Position*10 + int(c-'0')
			}
		case 'W':
			e.Where = v
		}
		if end >= len(body) {
			break
		}
		body = body[end+1:]
	}
	return e
}
