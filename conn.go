// Package pggo is a small, dependency-free PostgreSQL client that speaks the
// wire protocol (v3) directly.
//
// It provides connections, streaming queries with $N parameters, transactions,
// struct scanning, structured server errors, context cancellation (with a
// server-side cancel request), TLS, and SCRAM-SHA-256/MD5/cleartext auth. It
// intentionally does not try to be pgx: there is no statement cache, no binary
// format, no COPY or LISTEN/NOTIFY, and only a minimal pool.
package pggo

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cancelGrace is how long to keep reading after a context fires (and a cancel
// request is sent) before abandoning the socket.
const cancelGrace = 1500 * time.Millisecond

// Conn is a single PostgreSQL connection. It is not safe for concurrent use;
// use a Pool to share connections between goroutines.
type Conn struct {
	cfg     *Config
	nc      net.Conn
	r       *bufio.Reader
	w       []byte
	body    []byte
	network string
	addr    string
	pid     uint32
	secret  uint32
	params  map[string]string

	txStatus byte     // from the last ReadyForQuery: 'I' idle, 'T' in tx, 'E' failed tx
	pending  []string // statements to run before the next request (deferred BEGIN)
	busy     bool     // a Rows is open
	closed   atomic.Bool
	canceled atomic.Bool // the current operation's context fired
	mu       sync.Mutex  // guards nc against the cancel goroutine
}

// Option configures session behavior at connect time.
type Option func(*Config)

// ReadOnly makes every transaction on the session read-only
// (SET default_transaction_read_only = on). PostgreSQL enforces it.
func ReadOnly() Option {
	return func(c *Config) { c.sessionSQL = append(c.sessionSQL, "SET default_transaction_read_only = on") }
}

// StatementTimeout sets the session's statement_timeout.
func StatementTimeout(d time.Duration) Option {
	return func(c *Config) {
		c.sessionSQL = append(c.sessionSQL, fmt.Sprintf("SET statement_timeout = %d", d.Milliseconds()))
	}
}

// LockTimeout sets the session's lock_timeout.
func LockTimeout(d time.Duration) Option {
	return func(c *Config) {
		c.sessionSQL = append(c.sessionSQL, fmt.Sprintf("SET lock_timeout = %d", d.Milliseconds()))
	}
}

// Connect parses connString (see ParseConfig) and connects.
func Connect(ctx context.Context, connString string, opts ...Option) (*Conn, error) {
	cfg, err := ParseConfig(connString)
	if err != nil {
		return nil, err
	}
	return ConnectConfig(ctx, cfg, opts...)
}

// ConnectConfig connects using cfg. The context bounds dialing, TLS, and
// authentication. Options are applied with SET after authentication, so a
// failure to apply one fails the connect.
func ConnectConfig(ctx context.Context, cfg *Config, opts ...Option) (*Conn, error) {
	cfg = cfg.Copy()
	for _, o := range opts {
		o(cfg)
	}
	c := &Conn{cfg: cfg, params: map[string]string{}, txStatus: 'I'}
	c.network, c.addr = cfg.Addr()
	fail := func(err error) (*Conn, error) {
		if c.nc != nil {
			c.nc.Close()
		}
		if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
		return nil, &ConnectError{Addr: c.addr, Err: err}
	}
	var nc net.Conn
	var err error
	if cfg.DialFunc != nil {
		nc, err = cfg.DialFunc(ctx, c.network, c.addr)
	} else {
		var d net.Dialer
		nc, err = d.DialContext(ctx, c.network, c.addr)
	}
	if err != nil {
		return fail(err)
	}
	c.nc = nc
	stop := context.AfterFunc(ctx, func() { c.setDeadline(time.Now()) })
	if dl, ok := ctx.Deadline(); ok {
		nc.SetDeadline(dl)
	}
	err = c.startup()
	if !stop() || err != nil {
		if err == nil {
			err = ctx.Err()
		}
		return fail(err)
	}
	c.nc.SetDeadline(time.Time{})
	for _, s := range cfg.sessionSQL {
		if _, err := c.Exec(ctx, s); err != nil {
			c.Close()
			return nil, &ConnectError{Addr: c.addr, Err: err}
		}
	}
	return c, nil
}

func (c *Conn) setDeadline(t time.Time) {
	c.mu.Lock()
	c.nc.SetDeadline(t)
	c.mu.Unlock()
}

// PID is the server process ID of this connection's backend.
func (c *Conn) PID() uint32 { return c.pid }

// ParameterStatus returns a server-reported setting such as server_version,
// TimeZone, or in_hot_standby ("" if not reported).
func (c *Conn) ParameterStatus(name string) string { return c.params[name] }

// ServerVersion returns the server version without build details, e.g. "17.2".
func (c *Conn) ServerVersion() string {
	v := c.params["server_version"]
	if i := strings.IndexByte(v, ' '); i > 0 {
		v = v[:i]
	}
	return v
}

// Config returns the configuration the connection was opened with.
func (c *Conn) Config() *Config { return c.cfg.Copy() }

// IsClosed reports whether the connection is closed or unusable.
func (c *Conn) IsClosed() bool { return c.closed.Load() }

// TxStatus reports the server's transaction state after the last completed
// request: 'I' (idle), 'T' (in a transaction), or 'E' (in a failed transaction).
func (c *Conn) TxStatus() byte { return c.txStatus }

// Close terminates the session. Any open transaction is rolled back by the server.
func (c *Conn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.setDeadline(time.Now().Add(100 * time.Millisecond))
	c.nc.Write([]byte{'X', 0, 0, 0, 4})
	return c.nc.Close()
}

// fatal marks the connection unusable after a network or protocol failure.
func (c *Conn) fatal(err error) error {
	if !c.closed.Swap(true) {
		c.nc.Close()
	}
	return err
}

func (c *Conn) startup() error {
	cfg := c.cfg
	if c.network == "tcp" && cfg.SSLMode != "disable" && cfg.SSLMode != "allow" {
		if err := c.negotiateTLS(); err != nil {
			return err
		}
	}
	c.r = bufio.NewReaderSize(c.nc, 32*1024)

	c.w = append(c.w[:0], 0, 0, 0, 0, 0, 3, 0, 0) // length placeholder, protocol 3.0
	kv := func(k, v string) { c.w = append(append(append(append(c.w, k...), 0), v...), 0) }
	kv("user", cfg.User)
	kv("database", cfg.Database)
	kv("client_encoding", "UTF8")
	kv("DateStyle", "ISO, MDY")
	app := "pggo"
	for k, v := range cfg.RuntimeParams {
		switch k {
		case "application_name":
			app = v
		case "client_encoding", "DateStyle":
			// fixed: result decoding depends on them
		default:
			kv(k, v)
		}
	}
	kv("application_name", app)
	c.w = append(c.w, 0)
	binary.BigEndian.PutUint32(c.w, uint32(len(c.w)))
	if _, err := c.nc.Write(c.w); err != nil {
		return err
	}

	needPassword := errors.New("server requested a password but none was provided")
	var sc *scram
	for {
		typ, body, err := c.recv()
		if err != nil {
			return err
		}
		switch typ {
		case 'R':
			if len(body) < 4 {
				return &ProtocolError{"malformed authentication message"}
			}
			switch code := binary.BigEndian.Uint32(body); code {
			case 0: // AuthenticationOk
			case 3: // cleartext
				if cfg.Password == "" {
					return &AuthError{needPassword}
				}
				if err := c.send('p', append([]byte(cfg.Password), 0)); err != nil {
					return err
				}
			case 5: // MD5
				if cfg.Password == "" || len(body) < 8 {
					return &AuthError{needPassword}
				}
				inner := md5.Sum([]byte(cfg.Password + cfg.User))
				outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), body[4:8]...))
				if err := c.send('p', append([]byte("md5"+hex.EncodeToString(outer[:])), 0)); err != nil {
					return err
				}
			case 10: // SASL
				if !strings.Contains(string(body[4:]), "SCRAM-SHA-256\x00") {
					return &AuthError{errors.New("server offered no supported SASL mechanism (need SCRAM-SHA-256)")}
				}
				if cfg.Password == "" {
					return &AuthError{needPassword}
				}
				sc = newScram(cfg.Password)
				first := sc.clientFirst()
				msg := append([]byte("SCRAM-SHA-256\x00"), 0, 0, 0, 0)
				binary.BigEndian.PutUint32(msg[len(msg)-4:], uint32(len(first)))
				if err := c.send('p', append(msg, first...)); err != nil {
					return err
				}
			case 11: // SASLContinue
				if sc == nil {
					return &ProtocolError{"unexpected SASLContinue"}
				}
				final, err := sc.clientFinal(body[4:])
				if err != nil {
					return &AuthError{fmt.Errorf("SCRAM: %w", err)}
				}
				if err := c.send('p', final); err != nil {
					return err
				}
			case 12: // SASLFinal
				if sc == nil || !sc.verifyServer(body[4:]) {
					return &AuthError{errors.New("SCRAM: server signature mismatch")}
				}
			default:
				return &AuthError{fmt.Errorf("unsupported authentication method (code %d)", code)}
			}
		case 'K':
			if len(body) >= 8 {
				c.pid, c.secret = binary.BigEndian.Uint32(body), binary.BigEndian.Uint32(body[4:])
			}
		case 'S':
			c.paramStatus(body)
		case 'E':
			return parseError(body)
		case 'N', 'v':
		case 'Z':
			if len(body) > 0 {
				c.txStatus = body[0]
			}
			return nil
		default:
			return &ProtocolError{fmt.Sprintf("unexpected message %q during startup", typ)}
		}
	}
}

// AuthError is a client-side authentication failure (the server's own
// rejections arrive as *PgError with SQLSTATE 28P01/28000).
type AuthError struct{ Err error }

func (e *AuthError) Error() string { return "authentication: " + e.Err.Error() }
func (e *AuthError) Unwrap() error { return e.Err }

// TLSError is a failed TLS negotiation (refused by the server or verification failure).
type TLSError struct{ Err error }

func (e *TLSError) Error() string { return "TLS: " + e.Err.Error() }
func (e *TLSError) Unwrap() error { return e.Err }

// ProtocolError means the server sent something pggo could not understand.
type ProtocolError struct{ Msg string }

func (e *ProtocolError) Error() string { return "protocol error: " + e.Msg }

func (c *Conn) negotiateTLS() error {
	cfg := c.cfg
	if _, err := c.nc.Write([]byte{0, 0, 0, 8, 4, 210, 22, 47}); err != nil { // SSLRequest 80877103
		return err
	}
	var b [1]byte
	if _, err := io.ReadFull(c.nc, b[:]); err != nil {
		return err
	}
	if b[0] != 'S' {
		if cfg.SSLMode == "prefer" {
			return nil
		}
		return &TLSError{fmt.Errorf("server does not support SSL but sslmode=%s", cfg.SSLMode)}
	}
	tc := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	var roots *x509.CertPool
	if cfg.SSLRootCert != "" {
		pem, err := os.ReadFile(cfg.SSLRootCert)
		if err != nil {
			return &TLSError{fmt.Errorf("cannot read sslrootcert: %w", err)}
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return &TLSError{errors.New("sslrootcert contains no PEM certificates")}
		}
		tc.RootCAs = roots
	}
	mode := cfg.SSLMode
	if mode == "require" && roots != nil {
		mode = "verify-ca" // libpq: require + a root cert verifies the chain
	}
	switch mode {
	case "prefer", "require":
		tc.InsecureSkipVerify = true // libpq semantics: encrypt, do not verify
	case "verify-ca":
		tc.InsecureSkipVerify = true
		tc.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			certs := make([]*x509.Certificate, len(raw))
			for i, r := range raw {
				cert, err := x509.ParseCertificate(r)
				if err != nil {
					return err
				}
				certs[i] = cert
			}
			opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
			for _, ic := range certs[1:] {
				opts.Intermediates.AddCert(ic)
			}
			_, err := certs[0].Verify(opts)
			return err
		}
	}
	t := tls.Client(c.nc, tc)
	if err := t.Handshake(); err != nil {
		var ne net.Error
		if errors.As(err, &ne) {
			return err
		}
		return &TLSError{fmt.Errorf("handshake failed: %w", err)}
	}
	c.mu.Lock()
	c.nc = t
	c.mu.Unlock()
	return nil
}

func (c *Conn) paramStatus(body []byte) {
	parts := strings.SplitN(string(body), "\x00", 3)
	if len(parts) >= 2 {
		c.params[parts[0]] = parts[1]
	}
}

// send writes one message immediately.
func (c *Conn) send(typ byte, body []byte) error {
	c.w = c.w[:0]
	c.msg(typ, body...)
	_, err := c.nc.Write(c.w)
	return err
}

// msg appends one framed message to the write buffer.
func (c *Conn) msg(typ byte, body ...byte) {
	c.w = append(c.w, typ, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(c.w[len(c.w)-4:], uint32(len(body)+4))
	c.w = append(c.w, body...)
}

// recv reads one message. The body is only valid until the next recv.
func (c *Conn) recv() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[1:])) - 4
	if n < 0 || n > 1<<30 {
		return 0, nil, &ProtocolError{fmt.Sprintf("invalid message length %d", n)}
	}
	if cap(c.body) < n {
		c.body = make([]byte, n)
	}
	c.body = c.body[:n]
	if _, err := io.ReadFull(c.r, c.body); err != nil {
		return 0, nil, err
	}
	return hdr[0], c.body, nil
}

// cancelRequest asks the server (on a separate connection) to cancel whatever
// this backend is running, and bounds how long the caller keeps waiting.
func (c *Conn) cancelRequest() {
	c.canceled.Store(true)
	c.setDeadline(time.Now().Add(cancelGrace))
	var nc net.Conn
	var err error
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c.cfg.DialFunc != nil {
		nc, err = c.cfg.DialFunc(ctx, c.network, c.addr)
	} else {
		var d net.Dialer
		nc, err = d.DialContext(ctx, c.network, c.addr)
	}
	if err != nil {
		return
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(time.Second))
	b := []byte{0, 0, 0, 16, 4, 210, 22, 46, 0, 0, 0, 0, 0, 0, 0, 0} // CancelRequest 80877102
	binary.BigEndian.PutUint32(b[8:], c.pid)
	binary.BigEndian.PutUint32(b[12:], c.secret)
	nc.Write(b)
	io.Copy(io.Discard, nc) // the server closes when done
}

// QuoteIdentifier quotes each part as a SQL identifier and joins them with
// dots: QuoteIdentifier("my schema", "t") == `"my schema"."t"`.
func QuoteIdentifier(parts ...string) string {
	q := make([]string, len(parts))
	for i, p := range parts {
		q[i] = `"` + strings.ReplaceAll(strings.ReplaceAll(p, "\x00", ""), `"`, `""`) + `"`
	}
	return strings.Join(q, ".")
}
