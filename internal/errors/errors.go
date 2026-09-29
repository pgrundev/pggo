// Package errors defines pgGo's stable, machine-readable error contract.
//
// Every failure pgGo reports has exactly one Type from the fixed set below,
// the PostgreSQL SQLSTATE when the server supplied one, and a retryable flag,
// so an agent never has to infer error categories from message strings.
package errors

import (
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/pgrundev/pggo"
)

// Error types. This set is part of the public contract; do not rename.
const (
	Connection     = "connection_error"
	Authentication = "authentication_error"
	Timeout        = "timeout"
	Postgres       = "postgres_error"
	InvalidInput   = "invalid_input"
	Protocol       = "protocol_error"
)

// Error is the structured error emitted as {"ok":false,"error":{...}}.
type Error struct {
	Type      string
	Code      string // SQLSTATE; empty when not from the server
	Message   string
	Detail    string
	Hint      string
	Position  int // 1-based character position in the SQL, 0 if unknown
	Retryable bool
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%s): %s", e.Type, e.Code, e.Message)
	}
	return e.Type + ": " + e.Message
}

// New returns an error of the given type.
func New(typ, format string, args ...any) *Error {
	return &Error{Type: typ, Message: fmt.Sprintf(format, args...), Retryable: typ == Connection || typ == Timeout}
}

// FromServer classifies a PostgreSQL ErrorResponse by SQLSTATE.
func FromServer(code, message, detail, hint string, position int) *Error {
	e := &Error{Type: Postgres, Code: code, Message: message, Detail: detail, Hint: hint, Position: position}
	switch {
	case code == "28P01" || code == "28000":
		e.Type = Authentication
	case code == "57014": // query_canceled: statement_timeout or our cancel request
		e.Type = Timeout
		e.Retryable = true
	case code == "55P03" || code == "25P03": // lock_not_available, idle_in_transaction_session_timeout
		e.Type = Timeout
		e.Retryable = true
	case strings.HasPrefix(code, "08") && code != "08P01", code == "57P01", code == "57P02", code == "57P03":
		e.Type = Connection
		e.Retryable = true
	case code == "40001", code == "40P01", strings.HasPrefix(code, "53"):
		e.Retryable = true
	}
	return e
}

// Classify converts any Go error into a structured *Error.
func Classify(err error) *Error {
	var pe *Error
	if stderrors.As(err, &pe) {
		return pe
	}
	// Server errors first: a canceled statement is both a context error and a
	// *PgError (57014), and the SQLSTATE is the more useful of the two.
	var pg *pggo.PgError
	if stderrors.As(err, &pg) {
		return FromServer(pg.Code, pg.Message, pg.Detail, pg.Hint, pg.Position)
	}
	if stderrors.Is(err, context.DeadlineExceeded) || stderrors.Is(err, context.Canceled) {
		return New(Timeout, "operation timed out")
	}
	var authErr *pggo.AuthError
	if stderrors.As(err, &authErr) {
		return &Error{Type: Authentication, Message: authErr.Err.Error()}
	}
	var tlsErr *pggo.TLSError
	if stderrors.As(err, &tlsErr) {
		if stderrors.Is(err, os.ErrNotExist) || strings.Contains(tlsErr.Error(), "sslrootcert") {
			return New(InvalidInput, "%v", tlsErr)
		}
		return &Error{Type: Connection, Message: tlsErr.Error(), Retryable: false}
	}
	var protoErr *pggo.ProtocolError
	if stderrors.As(err, &protoErr) {
		return New(Protocol, "%s", protoErr.Msg)
	}
	if stderrors.Is(err, pggo.ErrConnClosed) {
		return New(Connection, "connection closed")
	}
	var ne net.Error
	if stderrors.Is(err, os.ErrDeadlineExceeded) || (stderrors.As(err, &ne) && ne.Timeout()) {
		return New(Timeout, "operation timed out")
	}
	if stderrors.Is(err, syscall.ECONNREFUSED) {
		return New(Connection, "connection refused: %v", err)
	}
	var dnsErr *net.DNSError
	if stderrors.As(err, &dnsErr) {
		e := New(Connection, "cannot resolve host %q", dnsErr.Name)
		e.Retryable = dnsErr.IsTemporary || dnsErr.IsTimeout
		return e
	}
	var opErr *net.OpError
	if stderrors.As(err, &opErr) || stderrors.Is(err, net.ErrClosed) || stderrors.Is(err, syscall.ECONNRESET) || stderrors.Is(err, syscall.EPIPE) {
		return New(Connection, "%v", err)
	}
	if stderrors.Is(err, os.ErrNotExist) {
		return New(Connection, "%v", err)
	}
	if err.Error() == "EOF" || strings.Contains(err.Error(), "unexpected EOF") {
		return New(Connection, "server closed the connection unexpectedly")
	}
	return New(Protocol, "%v", err)
}
