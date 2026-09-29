package errors

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestFromServer(t *testing.T) {
	cases := []struct {
		code      string
		typ       string
		retryable bool
	}{
		{"28P01", Authentication, false},
		{"28000", Authentication, false},
		{"57014", Timeout, true},
		{"55P03", Timeout, true},
		{"25P03", Timeout, true},
		{"08006", Connection, true},
		{"08001", Connection, true},
		{"08P01", Postgres, false}, // protocol_violation is a bug in the request, not the network
		{"57P01", Connection, true},
		{"57P02", Connection, true},
		{"57P03", Connection, true},
		{"40001", Postgres, true},
		{"40P01", Postgres, true},
		{"53300", Postgres, true},
		{"53100", Postgres, true},
		{"42P01", Postgres, false},
		{"42601", Postgres, false},
		{"23505", Postgres, false},
		{"22P02", Postgres, false},
		{"3D000", Postgres, false},
		{"25006", Postgres, false},
		{"", Postgres, false},
	}
	for _, c := range cases {
		e := FromServer(c.code, "msg", "", "", 0)
		if e.Type != c.typ || e.Retryable != c.retryable || e.Code != c.code {
			t.Errorf("FromServer(%q) = %s retryable=%v, want %s retryable=%v", c.code, e.Type, e.Retryable, c.typ, c.retryable)
		}
	}
}

func TestFromServerKeepsFields(t *testing.T) {
	e := FromServer("23505", "dup", "Key (id)=(1) already exists.", "h", 7)
	if e.Message != "dup" || e.Detail != "Key (id)=(1) already exists." || e.Hint != "h" || e.Position != 7 {
		t.Fatalf("%+v", e)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	pe := New(InvalidInput, "bad")
	cases := []struct {
		name      string
		err       error
		typ       string
		retryable bool
	}{
		{"passthrough", pe, InvalidInput, false},
		{"wrapped passthrough", fmt.Errorf("ctx: %w", pe), InvalidInput, false},
		{"deadline", os.ErrDeadlineExceeded, Timeout, true},
		{"net timeout", &net.OpError{Op: "read", Err: timeoutErr{}}, Timeout, true},
		{"refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, Connection, true},
		{"reset", &net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, Connection, true},
		{"dns permanent", &net.DNSError{Name: "nohost.invalid", Err: "no such host", IsNotFound: true}, Connection, false},
		{"dns temporary", &net.DNSError{Name: "h", Err: "server misbehaving", IsTemporary: true}, Connection, true},
		{"eof", io.EOF, Connection, true},
		{"unexpected eof", io.ErrUnexpectedEOF, Connection, true},
		{"closed", net.ErrClosed, Connection, true},
		{"unknown", stderrors.New("weird"), Protocol, false},
		{"context deadline wraps", fmt.Errorf("x: %w", os.ErrDeadlineExceeded), Timeout, true},
	}
	for _, c := range cases {
		e := Classify(c.err)
		if e.Type != c.typ || e.Retryable != c.retryable {
			t.Errorf("%s: Classify = %s retryable=%v, want %s retryable=%v", c.name, e.Type, e.Retryable, c.typ, c.retryable)
		}
		if e.Message == "" {
			t.Errorf("%s: empty message", c.name)
		}
	}
	_ = context.Canceled
}

func TestErrorString(t *testing.T) {
	if s := FromServer("42P01", "missing", "", "", 0).Error(); s != "postgres_error (42P01): missing" {
		t.Fatal(s)
	}
	if s := New(Timeout, "slow").Error(); s != "timeout: slow" {
		t.Fatal(s)
	}
}
