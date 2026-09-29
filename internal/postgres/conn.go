// Package postgres is a minimal PostgreSQL wire-protocol (v3) client: just
// enough for pgGo's commands. Startup, TLS, cleartext/MD5/SCRAM-SHA-256 auth,
// the extended query protocol with text-format parameters, and cancel requests.
package postgres

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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	perr "github.com/pgrundev/pggo/internal/errors"
)

// cancelGrace is how long we keep waiting for the server's reply after
// sending a cancel request, before giving up on the socket entirely.
const cancelGrace = 1500 * time.Millisecond

// Conn is a single PostgreSQL connection. Not safe for concurrent use.
type Conn struct {
	nc       net.Conn
	r        *bufio.Reader
	w        []byte
	body     []byte
	network  string
	addr     string
	pid      uint32
	secret   uint32
	params   map[string]string
	canceled atomic.Bool
	timeout  time.Duration
}

// Connect dials, negotiates TLS, and authenticates. The context deadline bounds
// the whole handshake.
func Connect(ctx context.Context, cfg *Config) (*Conn, error) {
	c := &Conn{params: map[string]string{}, timeout: cfg.Timeout}
	if dl, ok := ctx.Deadline(); ok && c.timeout == 0 {
		c.timeout = time.Until(dl).Round(time.Millisecond)
	}
	if strings.HasPrefix(cfg.Host, "/") {
		c.network, c.addr = "unix", fmt.Sprintf("%s/.s.PGSQL.%d", cfg.Host, cfg.Port)
	} else {
		c.network, c.addr = "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, c.network, c.addr)
	if err != nil {
		return nil, c.connectErr(ctx, err)
	}
	c.nc = nc
	if dl, ok := ctx.Deadline(); ok {
		nc.SetDeadline(dl)
	}
	if err := c.startup(cfg); err != nil {
		nc.Close()
		return nil, c.connectErr(ctx, err)
	}
	nc.SetDeadline(time.Time{})
	return c, nil
}

func (c *Conn) connectErr(ctx context.Context, err error) error {
	var pe *perr.Error
	if errors.As(err, &pe) {
		return pe
	}
	e := perr.Classify(err)
	if e.Type == perr.Timeout || ctx.Err() != nil {
		e = perr.New(perr.Timeout, "could not connect to %s within %s", c.addr, c.timeout)
	} else if e.Type == perr.Connection && e.Code == "" {
		cause := err
		for u := errors.Unwrap(cause); u != nil; u = errors.Unwrap(cause) {
			cause = u
		}
		e.Message = fmt.Sprintf("could not connect to %s: %v", c.addr, cause)
	}
	return e
}

// ServerVersion returns the server_version reported at startup, e.g. "17.2".
func (c *Conn) ServerVersion() string {
	v := c.params["server_version"]
	if i := strings.IndexByte(v, ' '); i > 0 {
		v = v[:i]
	}
	return v
}

// ServerReadOnly reports whether the server said at startup that sessions are
// read-only (PostgreSQL 14+ reports default_transaction_read_only and in_hot_standby).
func (c *Conn) ServerReadOnly() bool {
	return c.params["default_transaction_read_only"] == "on" || c.params["in_hot_standby"] == "on"
}

// Close sends Terminate and closes the socket.
func (c *Conn) Close() error {
	c.nc.SetDeadline(time.Now().Add(100 * time.Millisecond))
	c.nc.Write([]byte{'X', 0, 0, 0, 4})
	return c.nc.Close()
}

func (c *Conn) startup(cfg *Config) error {
	if c.network == "tcp" && cfg.SSLMode != "disable" && cfg.SSLMode != "allow" {
		if err := c.negotiateTLS(cfg); err != nil {
			return err
		}
	}
	c.r = bufio.NewReaderSize(c.nc, 32*1024)

	c.w = c.w[:0]
	c.w = append(c.w, 0, 0, 0, 0, 0, 3, 0, 0) // length placeholder, protocol 3.0
	kv := func(k, v string) { c.w = append(append(append(append(c.w, k...), 0), v...), 0) }
	kv("user", cfg.User)
	kv("database", cfg.Database)
	kv("client_encoding", "UTF8")
	kv("DateStyle", "ISO, MDY")
	app := "pggo"
	for k, v := range cfg.Params {
		if k == "application_name" {
			app = v
			continue
		}
		kv(k, v)
	}
	kv("application_name", app)
	c.w = append(c.w, 0)
	binary.BigEndian.PutUint32(c.w, uint32(len(c.w)))
	if _, err := c.nc.Write(c.w); err != nil {
		return err
	}

	var sc *scram
	for {
		typ, body, err := c.recv()
		if err != nil {
			return err
		}
		switch typ {
		case 'R':
			if len(body) < 4 {
				return fmt.Errorf("malformed authentication message")
			}
			code := binary.BigEndian.Uint32(body)
			switch code {
			case 0: // AuthenticationOk
			case 3: // cleartext
				if cfg.Password == "" {
					return perr.New(perr.Authentication, "server requested a password but none was provided (set it in the URL or PGPASSWORD)")
				}
				if err := c.send('p', append([]byte(cfg.Password), 0)); err != nil {
					return err
				}
			case 5: // MD5
				if cfg.Password == "" || len(body) < 8 {
					return perr.New(perr.Authentication, "server requested a password but none was provided (set it in the URL or PGPASSWORD)")
				}
				inner := md5.Sum([]byte(cfg.Password + cfg.User))
				outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), body[4:8]...))
				if err := c.send('p', append([]byte("md5"+hex.EncodeToString(outer[:])), 0)); err != nil {
					return err
				}
			case 10: // SASL
				if !strings.Contains(string(body[4:]), "SCRAM-SHA-256\x00") {
					return perr.New(perr.Authentication, "server offered no supported SASL mechanism (need SCRAM-SHA-256)")
				}
				if cfg.Password == "" {
					return perr.New(perr.Authentication, "server requested a password but none was provided (set it in the URL or PGPASSWORD)")
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
					return fmt.Errorf("unexpected SASLContinue")
				}
				final, err := sc.clientFinal(body[4:])
				if err != nil {
					return perr.New(perr.Authentication, "SCRAM: %v", err)
				}
				if err := c.send('p', final); err != nil {
					return err
				}
			case 12: // SASLFinal
				if sc == nil || !sc.verifyServer(body[4:]) {
					return perr.New(perr.Authentication, "SCRAM: server signature mismatch")
				}
			default:
				return perr.New(perr.Authentication, "unsupported authentication method (code %d)", code)
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
			return nil
		default:
			return fmt.Errorf("unexpected message %q during startup", typ)
		}
	}
}

func (c *Conn) negotiateTLS(cfg *Config) error {
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
		return perr.New(perr.Connection, "server does not support SSL but sslmode=%s", cfg.SSLMode)
	}
	tc := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	var roots *x509.CertPool
	if cfg.SSLRootCert != "" {
		pem, err := os.ReadFile(cfg.SSLRootCert)
		if err != nil {
			return perr.New(perr.InvalidInput, "cannot read sslrootcert: %v", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return perr.New(perr.InvalidInput, "sslrootcert contains no PEM certificates")
		}
		tc.RootCAs = roots
	}
	switch cfg.SSLMode {
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
		if _, ok := err.(net.Error); ok {
			return err
		}
		e := perr.New(perr.Connection, "TLS handshake failed: %v", err)
		e.Retryable = false
		return e
	}
	c.nc = t
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

// recv reads one message. The returned body is only valid until the next recv.
func (c *Conn) recv() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[1:])) - 4
	if n < 0 || n > 1<<30 {
		return 0, nil, fmt.Errorf("invalid message length %d", n)
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

// cancel sends a CancelRequest on a fresh connection (best effort).
func (c *Conn) cancel() {
	c.canceled.Store(true)
	nc, err := net.DialTimeout(c.network, c.addr, time.Second)
	if err != nil {
		return
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(time.Second))
	b := []byte{0, 0, 0, 16, 4, 210, 22, 46, 0, 0, 0, 0, 0, 0, 0, 0} // 80877102
	binary.BigEndian.PutUint32(b[8:], c.pid)
	binary.BigEndian.PutUint32(b[12:], c.secret)
	nc.Write(b)
	io.Copy(io.Discard, nc) // server closes when done
}

func parseError(body []byte) *perr.Error {
	var code, msg, detail, hint string
	pos := 0
	for len(body) > 1 {
		f := body[0]
		end := 1
		for end < len(body) && body[end] != 0 {
			end++
		}
		v := string(body[1:end])
		switch f {
		case 'C':
			code = v
		case 'M':
			msg = v
		case 'D':
			detail = v
		case 'H':
			hint = v
		case 'P':
			pos, _ = strconv.Atoi(v)
		}
		if end >= len(body) {
			break
		}
		body = body[end+1:]
	}
	return perr.FromServer(code, msg, detail, hint, pos)
}
