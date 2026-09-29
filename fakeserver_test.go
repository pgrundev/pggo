package pggo

// Protocol-level tests against a scripted fake server. These cover failure
// modes a real PostgreSQL rarely produces on demand: broken auth exchanges,
// malformed messages, dropped connections, and a server that ignores cancel.

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	fakePID    = 4242
	fakeSecret = 0xC0FFEE
)

type fakeConn struct {
	t      *testing.T
	c      net.Conn
	r      *bufio.Reader
	params map[string]string
}

func (f *fakeConn) send(typ byte, body ...byte) {
	b := append([]byte{typ, 0, 0, 0, 0}, body...)
	binary.BigEndian.PutUint32(b[1:], uint32(len(body)+4))
	f.c.Write(b)
}

func (f *fakeConn) read() (byte, []byte) {
	var hdr [5]byte
	if _, err := io.ReadFull(f.r, hdr[:]); err != nil {
		return 0, nil
	}
	body := make([]byte, binary.BigEndian.Uint32(hdr[1:])-4)
	io.ReadFull(f.r, body)
	return hdr[0], body
}

// readUntil reads messages until one of type typ arrives, returning all seen.
func (f *fakeConn) readUntil(typ byte) map[byte][][]byte {
	seen := map[byte][][]byte{}
	for {
		t, b := f.read()
		if t == 0 {
			return seen
		}
		seen[t] = append(seen[t], b)
		if t == typ {
			return seen
		}
	}
}

func u32(v uint32) []byte  { return binary.BigEndian.AppendUint32(nil, v) }
func cstr(s string) []byte { return append([]byte(s), 0) }

func (f *fakeConn) authOK()           { f.send('R', u32(0)...) }
func (f *fakeConn) ready(st byte)     { f.send('Z', st) }
func (f *fakeConn) param(k, v string) { f.send('S', append(cstr(k), cstr(v)...)...) }
func (f *fakeConn) handshake() {
	f.authOK()
	f.param("server_version", "17.2 (Debian 17.2-1.pgdg120+1)")
	f.send('K', append(u32(fakePID), u32(fakeSecret)...)...)
	f.ready('I')
}

func (f *fakeConn) errorResponse(code, msg string) {
	var b []byte
	b = append(b, 'S')
	b = append(b, cstr("ERROR")...)
	b = append(b, 'C')
	b = append(b, cstr(code)...)
	b = append(b, 'M')
	b = append(b, cstr(msg)...)
	b = append(b, 0)
	f.send('E', b...)
}

func rowDescription(cols ...Column) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(cols)))
	for _, c := range cols {
		b = append(b, cstr(c.Name)...)
		b = append(b, u32(0)...)
		b = append(b, 0, 0)
		b = append(b, u32(c.TypeOID)...)
		b = append(b, 0xFF, 0xFF)
		b = append(b, u32(0xFFFFFFFF)...)
		b = append(b, 0, 0)
	}
	return b
}

func dataRow(vals ...*string) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(vals)))
	for _, v := range vals {
		if v == nil {
			b = append(b, u32(0xFFFFFFFF)...)
			continue
		}
		b = append(b, u32(uint32(len(*v)))...)
		b = append(b, *v...)
	}
	return b
}

func sp(s string) *string { return &s }

type fakeServer struct {
	addr    string
	cancels chan [2]uint32
	sslReq  chan bool
}

// startFake runs handler for every regular connection. SSLRequests are
// answered with sslReply ('N' by default); cancel requests go to cancels.
func startFake(t *testing.T, sslReply byte, handler func(f *fakeConn)) *fakeServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	s := &fakeServer{addr: l.Addr().String(), cancels: make(chan [2]uint32, 4), sslReq: make(chan bool, 4)}
	if sslReply == 0 {
		sslReply = 'N'
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					var hdr [8]byte
					if _, err := io.ReadFull(r, hdr[:]); err != nil {
						return
					}
					n := binary.BigEndian.Uint32(hdr[:])
					code := binary.BigEndian.Uint32(hdr[4:])
					rest := make([]byte, n-8)
					io.ReadFull(r, rest)
					switch code {
					case 80877103:
						s.sslReq <- true
						c.Write([]byte{sslReply})
						continue
					case 80877102:
						s.cancels <- [2]uint32{binary.BigEndian.Uint32(rest), binary.BigEndian.Uint32(rest[4:])}
						return
					}
					params := map[string]string{}
					parts := strings.Split(string(rest), "\x00")
					for i := 0; i+1 < len(parts); i += 2 {
						if parts[i] != "" {
							params[parts[i]] = parts[i+1]
						}
					}
					handler(&fakeConn{t: t, c: c, r: r, params: params})
					return
				}
			}(c)
		}
	}()
	return s
}

func (s *fakeServer) cfg(t *testing.T, extra string) *Config {
	t.Helper()
	c, err := ParseConfig("postgres://alice:secret@" + s.addr + "/app?sslmode=disable" + extra)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ctxTimeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// Error categories as a library user sees them.
const (
	catAuth       = "authentication"
	catPostgres   = "postgres"
	catConnection = "connection"
	catTimeout    = "timeout"
	catProtocol   = "protocol"
	catTLS        = "tls"
)

func category(err error) string {
	var pe *PgError
	if errors.As(err, &pe) {
		switch {
		case strings.HasPrefix(pe.Code, "28"):
			return catAuth
		case pe.Code == "57014":
			return catTimeout
		case strings.HasPrefix(pe.Code, "08"), strings.HasPrefix(pe.Code, "57P"):
			return catConnection
		}
		return catPostgres
	}
	var ae *AuthError
	var te *TLSError
	var pr *ProtocolError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return catTimeout
	case errors.As(err, &ae):
		return catAuth
	case errors.As(err, &te):
		return catTLS
	case errors.As(err, &pr):
		return catProtocol
	}
	return catConnection
}

// wantType checks the error category and returns the *PgError, if any.
func wantType(t *testing.T, err error, cat string) *PgError {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s error, got nil", cat)
	}
	if got := category(err); got != cat {
		t.Fatalf("got %s (%v), want %s", got, err, cat)
	}
	var pe *PgError
	errors.As(err, &pe)
	return pe
}

func TestFakeStartupParams(t *testing.T) {
	got := make(chan map[string]string, 1)
	s := startFake(t, 0, func(f *fakeConn) {
		got <- f.params
		f.handshake()
		f.readUntil('X')
	})
	c, err := ConnectConfig(ctxTimeout(t, 2*time.Second), s.cfg(t, "&application_name=myagent&search_path=app"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := <-got
	for k, v := range map[string]string{"user": "alice", "database": "app", "client_encoding": "UTF8", "DateStyle": "ISO, MDY", "application_name": "myagent", "search_path": "app"} {
		if p[k] != v {
			t.Errorf("startup %s = %q, want %q", k, p[k], v)
		}
	}
	if c.ServerVersion() != "17.2" {
		t.Errorf("ServerVersion = %q", c.ServerVersion())
	}
}

func TestFakeCleartextAndMD5(t *testing.T) {
	for _, method := range []string{"cleartext", "md5"} {
		t.Run(method, func(t *testing.T) {
			got := make(chan string, 1)
			salt := []byte{1, 2, 3, 4}
			s := startFake(t, 0, func(f *fakeConn) {
				if method == "cleartext" {
					f.send('R', u32(3)...)
				} else {
					f.send('R', append(u32(5), salt...)...)
				}
				_, pw := f.read()
				got <- strings.TrimRight(string(pw), "\x00")
				f.handshake()
				f.readUntil('X')
			})
			c, err := ConnectConfig(ctxTimeout(t, 2*time.Second), s.cfg(t, ""))
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
			want := "secret"
			if method == "md5" {
				inner := md5.Sum([]byte("secretalice"))
				outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), salt...))
				want = "md5" + hex.EncodeToString(outer[:])
			}
			if pw := <-got; pw != want {
				t.Fatalf("password message %q, want %q", pw, want)
			}
		})
	}
}

func TestFakeMissingPassword(t *testing.T) {
	for _, code := range []uint32{3, 5, 10} {
		s := startFake(t, 0, func(f *fakeConn) {
			body := u32(code)
			if code == 5 {
				body = append(body, 1, 2, 3, 4)
			}
			if code == 10 {
				body = append(body, cstr("SCRAM-SHA-256")...)
				body = append(body, 0)
			}
			f.send('R', body...)
			f.read()
		})
		cfg := s.cfg(t, "")
		cfg.Password = ""
		_, err := ConnectConfig(ctxTimeout(t, 2*time.Second), cfg)
		wantType(t, err, catAuth)
		if !strings.Contains(err.Error(), "password") {
			t.Fatalf("auth %d: %v", code, err)
		}
	}
}

func TestFakeUnsupportedAuth(t *testing.T) {
	for _, body := range [][]byte{u32(7), u32(9), append(append(u32(10), cstr("SCRAM-SHA-256-PLUS")...), 0)} {
		s := startFake(t, 0, func(f *fakeConn) { f.send('R', body...); f.read() })
		wantType(t, connectErr(t, s, ""), catAuth)
	}
}

func connectErr(t *testing.T, s *fakeServer, extra string) error {
	t.Helper()
	c, err := ConnectConfig(ctxTimeout(t, 2*time.Second), s.cfg(t, extra))
	if err == nil {
		c.Close()
	}
	return err
}

// scramServer runs the server side of SCRAM-SHA-256; mutate lets a test break it.
func scramServer(f *fakeConn, password string, mutate func(step string, msg string) string) bool {
	f.send('R', append(append(u32(10), cstr("SCRAM-SHA-256")...), 0)...)
	_, b := f.read() // SASLInitialResponse
	i := strings.IndexByte(string(b), 0)
	first := string(b[i+5:])
	bare := strings.TrimPrefix(first, "n,,")
	cnonce := bare[strings.Index(bare, "r=")+2:]
	salt := []byte("0123456789abcdef")
	serverFirst := "r=" + cnonce + "SRV,s=" + b64(salt) + ",i=4096"
	serverFirst = mutate("first", serverFirst)
	f.send('R', append(u32(11), serverFirst...)...)
	_, final := f.read()
	if final == nil {
		return false
	}
	withoutProof := string(final[:strings.Index(string(final), ",p=")])
	salted := pbkdf2SHA256([]byte(password), salt, 4096)
	auth := bare + "," + serverFirst + "," + withoutProof
	sig := hmacSHA256(hmacSHA256(salted, "Server Key"), auth)
	f.send('R', append(u32(12), mutate("final", "v="+b64(sig))...)...)
	return true
}

func TestFakeSCRAM(t *testing.T) {
	ok := func(_, m string) string { return m }
	s := startFake(t, 0, func(f *fakeConn) {
		if scramServer(f, "secret", ok) {
			f.handshake()
			f.readUntil('X')
		}
	})
	if err := connectErr(t, s, ""); err != nil {
		t.Fatalf("valid SCRAM exchange failed: %v", err)
	}

	breakages := map[string]func(step, m string) string{
		"bad server signature": func(step, m string) string {
			if step == "final" {
				return "v=" + b64([]byte("definitely not the signature...!"))
			}
			return m
		},
		"server nonce not extending client nonce": func(step, m string) string {
			if step == "first" {
				return "r=someoneelse,s=" + b64([]byte("salt")) + ",i=4096"
			}
			return m
		},
		"zero iterations": func(step, m string) string {
			if step == "first" {
				return strings.Replace(m, "i=4096", "i=0", 1)
			}
			return m
		},
		"bad salt": func(step, m string) string {
			if step == "first" {
				return m[:strings.Index(m, ",s=")] + ",s=!!!,i=4096"
			}
			return m
		},
	}
	for name, mutate := range breakages {
		t.Run(name, func(t *testing.T) {
			s := startFake(t, 0, func(f *fakeConn) {
				if scramServer(f, "secret", mutate) {
					f.handshake()
				}
			})
			wantType(t, connectErr(t, s, ""), catAuth)
		})
	}
}

func TestFakeStartupError(t *testing.T) {
	s := startFake(t, 0, func(f *fakeConn) { f.errorResponse("3D000", `database "app" does not exist`) })
	e := wantType(t, connectErr(t, s, ""), catPostgres)
	if e.Code != "3D000" {
		t.Fatalf("%+v", e)
	}
	s = startFake(t, 0, func(f *fakeConn) { f.errorResponse("53300", "too many clients") })
	if e := wantType(t, connectErr(t, s, ""), catPostgres); e.Code != "53300" {
		t.Fatalf("want 53300, got %v", e)
	}
}

func TestFakeServerClosesDuringStartup(t *testing.T) {
	s := startFake(t, 0, func(f *fakeConn) {})
	wantType(t, connectErr(t, s, ""), catConnection)
	s = startFake(t, 0, func(f *fakeConn) { f.authOK() })
	wantType(t, connectErr(t, s, ""), catConnection)
}

func TestFakeStartupStallTimesOut(t *testing.T) {
	s := startFake(t, 0, func(f *fakeConn) { f.authOK(); time.Sleep(3 * time.Second) })
	start := time.Now()
	_, err := ConnectConfig(ctxTimeout(t, 300*time.Millisecond), s.cfg(t, ""))
	wantType(t, err, catTimeout)
	var ce *ConnectError
	if !errors.As(err, &ce) || time.Since(start) > time.Second {
		t.Fatalf("took %v: %v", time.Since(start), err)
	}
}

func TestFakeSSLNegotiation(t *testing.T) {
	s := startFake(t, 'N', func(f *fakeConn) { f.handshake(); f.readUntil('X') })
	if err := connectErr(t, s, "&sslmode=prefer"); err != nil {
		t.Fatalf("prefer must fall back to plaintext: %v", err)
	}
	if !<-s.sslReq {
		t.Fatal("no SSLRequest sent")
	}
	for _, mode := range []string{"require", "verify-ca", "verify-full"} {
		err := connectErr(t, s, "&sslmode="+mode)
		wantType(t, err, catTLS)
		if !strings.Contains(err.Error(), "SSL") {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	// disable must not send an SSLRequest at all.
	if err := connectErr(t, s, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.sslReq:
		// drain the require/verify requests
	default:
	}
}

func TestFakeUnexpectedStartupMessage(t *testing.T) {
	s := startFake(t, 0, func(f *fakeConn) { f.send('D', 0, 0) })
	wantType(t, connectErr(t, s, ""), catProtocol)
	s = startFake(t, 0, func(f *fakeConn) { f.send('R') })
	wantType(t, connectErr(t, s, ""), catProtocol)
}

// queryFake connects to a server that completes the handshake and then runs script.
func queryFake(t *testing.T, script func(f *fakeConn)) (*fakeServer, *Conn) {
	t.Helper()
	s := startFake(t, 0, func(f *fakeConn) { f.handshake(); script(f) })
	c, err := ConnectConfig(ctxTimeout(t, 2*time.Second), s.cfg(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return s, c
}

// drain reads all rows and returns the error that ended iteration.
func drain(rows *Rows, err error) error {
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	rows.Close()
	return rows.Err()
}

func TestFakeQueryWireFormat(t *testing.T) {
	seen := make(chan map[byte][][]byte, 1)
	_, c := queryFake(t, func(f *fakeConn) {
		seen <- f.readUntil('S')
		f.send('1')
		f.send('2')
		f.send('T', rowDescription(Column{"id", OIDInt4}, Column{"name", 25})...)
		f.send('N', 'M', 'n', 'o', 't', 'e', 0, 0) // notices are ignored
		f.send('D', dataRow(sp("1"), sp("a"))...)
		f.send('D', dataRow(sp("2"), nil)...)
		f.send('C', cstr("SELECT 2")...)
		f.ready('I')
		f.readUntil('X')
	})
	rows, err := c.Query(ctxTimeout(t, 2*time.Second),
		"SELECT id, name FROM t WHERE a = $1 AND b = $2 AND c = $3", "x", nil, "", MaxRows(7))
	if err != nil {
		t.Fatal(err)
	}
	cols := rows.Columns()
	var got []string
	for rows.Next() {
		var id int
		var name *string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		n := "<NULL>"
		if name != nil {
			n = *name
		}
		got = append(got, fmt.Sprintf("%d:%s", id, n))
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if rows.CommandTag() != "SELECT 2" || len(cols) != 2 || cols[0].Name != "id" || cols[0].TypeOID != OIDInt4 {
		t.Fatalf("%v %+v", rows.CommandTag(), cols)
	}
	if strings.Join(got, ",") != "1:a,2:<NULL>" {
		t.Fatalf("rows %v", got)
	}
	m := <-seen
	if string(m['P'][0]) != "\x00SELECT id, name FROM t WHERE a = $1 AND b = $2 AND c = $3\x00\x00\x00" {
		t.Fatalf("Parse %q", m['P'][0])
	}
	wantBind := "\x00\x00\x00\x00\x00\x03" + "\x00\x00\x00\x01x" + "\xff\xff\xff\xff" + "\x00\x00\x00\x00" + "\x00\x00"
	if string(m['B'][0]) != wantBind {
		t.Fatalf("Bind %q\nwant %q", m['B'][0], wantBind)
	}
	if string(m['D'][0]) != "P\x00" {
		t.Fatalf("Describe %q", m['D'][0])
	}
	if string(m['E'][0]) != "\x00\x00\x00\x00\x07" {
		t.Fatalf("Execute %q", m['E'][0])
	}
	if _, ok := m['Q']; ok {
		t.Fatal("no simple-query messages expected")
	}
}

func TestFakeTxBeginIsPipelined(t *testing.T) {
	seen := make(chan []string, 2)
	_, c := queryFake(t, func(f *fakeConn) {
		var order []string
		for {
			typ, b := f.read()
			if typ == 'P' {
				sql := strings.Split(string(b[1:]), "\x00")[0]
				order = append(order, "P:"+sql)
			} else {
				order = append(order, string(typ))
			}
			if typ == 'S' {
				break
			}
		}
		seen <- order
		f.send('1')
		f.send('2')
		f.send('C', cstr("BEGIN")...)
		f.send('1')
		f.send('2')
		f.send('T', rowDescription(Column{"n", OIDInt4})...)
		f.send('D', dataRow(sp("1"))...)
		f.send('D', dataRow(sp("2"))...)
		f.send('s') // PortalSuspended
		f.ready('T')
		m := f.readUntil('S')
		seen <- []string{strings.Split(string(m['P'][0][1:]), "\x00")[0]}
		f.send('1')
		f.send('2')
		f.send('n')
		f.send('C', cstr("ROLLBACK")...)
		f.ready('I')
		f.readUntil('X')
	})
	ctx := ctxTimeout(t, 2*time.Second)
	tx, err := c.BeginTx(ctx, TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "SELECT n", MaxRows(2))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if rows.Err() != nil || n != 2 || !rows.Suspended() || rows.CommandTag() != "" {
		t.Fatalf("err=%v n=%d suspended=%v", rows.Err(), n, rows.Suspended())
	}
	if got := strings.Join(<-seen, " "); got != "P:BEGIN READ ONLY B E P:SELECT n B D E S" {
		t.Fatalf("pipeline: %s", got)
	}
	if c.TxStatus() != 'T' {
		t.Fatalf("tx status %q", c.TxStatus())
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got[0] != "ROLLBACK" {
		t.Fatalf("rollback sent %v", got)
	}
}

func TestFakeFailedBeginSkipsStatement(t *testing.T) {
	// If BEGIN fails, the server skips the rest of the pipeline up to Sync:
	// the statement must not be reported as having run.
	_, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		f.send('1')
		f.send('2')
		f.errorResponse("25001", "BEGIN failed")
		f.ready('I')
		f.readUntil('X')
	})
	ctx := ctxTimeout(t, time.Second)
	tx, _ := c.BeginTx(ctx, TxOptions{ReadOnly: true})
	e := wantType(t, drain(tx.Query(ctx, "SELECT 1")), catPostgres)
	if e.Code != "25001" {
		t.Fatal(e)
	}
}

func TestFakeEmptyTxSendsNothing(t *testing.T) {
	got := make(chan byte, 1)
	_, c := queryFake(t, func(f *fakeConn) {
		typ, _ := f.read()
		got <- typ
	})
	ctx := ctxTimeout(t, time.Second)
	tx, _ := c.BeginTx(ctx, TxOptions{})
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); !errors.Is(err, ErrTxClosed) {
		t.Fatalf("second commit: %v", err)
	}
	c.Close()
	if typ := <-got; typ != 'X' {
		t.Fatalf("an unused transaction sent %q to the server", typ)
	}
}

func TestFakeCommitRollback(t *testing.T) {
	_, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S') // BEGIN + statement
		f.send('1')
		f.send('2')
		f.send('C', cstr("BEGIN")...)
		f.errorResponse("22012", "division by zero")
		f.ready('E')
		f.readUntil('S') // COMMIT
		f.send('1')
		f.send('2')
		f.send('n')
		f.send('C', cstr("ROLLBACK")...)
		f.ready('I')
		f.readUntil('X')
	})
	ctx := ctxTimeout(t, time.Second)
	tx, _ := c.BeginTx(ctx, TxOptions{})
	if _, err := tx.Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("want error")
	}
	if c.TxStatus() != 'E' {
		t.Fatalf("status %q", c.TxStatus())
	}
	if err := tx.Commit(ctx); !errors.Is(err, ErrTxCommitRollback) {
		t.Fatalf("commit of failed tx: %v", err)
	}
}

func TestFakeQueryErrorStillDrainsToReady(t *testing.T) {
	_, c := queryFake(t, func(f *fakeConn) {
		for i := 0; i < 2; i++ {
			f.readUntil('S')
			if i == 0 {
				f.errorResponse("42P01", `relation "x" does not exist`)
			} else {
				f.send('C', cstr("SELECT 0")...)
			}
			f.ready('I')
		}
		f.readUntil('X')
	})
	_, err := c.Query(ctxTimeout(t, time.Second), "SELECT * FROM x")
	if e := wantType(t, err, catPostgres); e.Code != "42P01" || e.Severity != "ERROR" {
		t.Fatal(e)
	}
	if !strings.Contains(err.Error(), "(SQLSTATE 42P01)") {
		t.Fatalf("error text: %v", err)
	}
	// The connection is still usable: the error path consumed everything up to ReadyForQuery.
	if _, err := c.Exec(ctxTimeout(t, time.Second), "SELECT 1"); err != nil {
		t.Fatalf("connection not reusable after error: %v", err)
	}
}

func TestFakeConnBusy(t *testing.T) {
	_, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		f.send('1')
		f.send('2')
		f.send('T', rowDescription(Column{"a", 25})...)
		f.send('D', dataRow(sp("x"))...)
		f.send('C', cstr("SELECT 1")...)
		f.ready('I')
		f.readUntil('X')
	})
	ctx := ctxTimeout(t, time.Second)
	rows, err := c.Query(ctx, "SELECT a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "SELECT 2"); !errors.Is(err, ErrConnBusy) {
		t.Fatalf("second query while rows open: %v", err)
	}
	rows.Close()
	rows.Close() // idempotent
	if c.IsClosed() {
		t.Fatal("closing rows must not close the connection")
	}
}

func TestFakeMalformedMessages(t *testing.T) {
	cases := map[string]func(f *fakeConn){
		"unknown message type": func(f *fakeConn) { f.send('?', 1, 2, 3) },
		"short DataRow":        func(f *fakeConn) { f.send('D', 0) },
		"DataRow value overruns": func(f *fakeConn) {
			f.send('D', append(binary.BigEndian.AppendUint16(nil, 1), u32(100)...)...)
		},
		"DataRow missing length": func(f *fakeConn) { f.send('D', 0, 2, 0, 0) },
		"DataRow column count":   func(f *fakeConn) { f.send('D', dataRow(sp("a"), sp("b"))...) },
		"huge message length":    func(f *fakeConn) { f.c.Write([]byte{'D', 0x7F, 0xFF, 0xFF, 0xFF}) },
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			_, c := queryFake(t, func(f *fakeConn) {
				f.readUntil('S')
				f.send('1')
				f.send('2')
				f.send('T', rowDescription(Column{"a", 25})...)
				bad(f)
				time.Sleep(time.Second)
			})
			wantType(t, drain(c.Query(ctxTimeout(t, 2*time.Second), "SELECT a")), catProtocol)
			if !c.IsClosed() {
				t.Fatal("a protocol error must close the connection")
			}
		})
	}
}

func TestFakeServerDiesMidQuery(t *testing.T) {
	_, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		f.send('1')
		f.send('2')
		f.send('T', rowDescription(Column{"a", 25})...)
		f.send('D', dataRow(sp("partial"))...)
		// close without CommandComplete / ReadyForQuery
	})
	wantType(t, drain(c.Query(ctxTimeout(t, 2*time.Second), "SELECT a")), catConnection)
	if !c.IsClosed() {
		t.Fatal("connection should be closed")
	}
	if _, err := c.Exec(context.Background(), "SELECT 1"); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("reuse after failure: %v", err)
	}
}

func TestFakeCancelOnTimeout(t *testing.T) {
	// Server honors the cancel: replies 57014 after receiving it.
	var s *fakeServer
	s, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		<-s.cancels
		f.errorResponse("57014", "canceling statement due to user request")
		f.ready('I')
		f.readUntil('X')
	})
	start := time.Now()
	_, err := c.Exec(ctxTimeout(t, 200*time.Millisecond), "SELECT pg_sleep(10)")
	e := wantType(t, err, catTimeout)
	if e == nil || e.Code != "57014" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want both 57014 and DeadlineExceeded: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	// A canceled connection is never reused: a late cancel could hit the next statement.
	if !c.IsClosed() {
		t.Fatal("connection must be closed after cancellation")
	}
}

func TestFakeCancelWhileStreaming(t *testing.T) {
	var s *fakeServer
	s, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		f.send('1')
		f.send('2')
		f.send('T', rowDescription(Column{"a", 25})...)
		f.send('D', dataRow(sp("first"))...)
		<-s.cancels
		f.errorResponse("57014", "canceling statement due to user request")
		f.ready('I')
	})
	ctx, cancel := context.WithCancel(context.Background())
	rows, err := c.Query(ctx, "SELECT a FROM big")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal(rows.Err())
	}
	cancel()
	for rows.Next() {
	}
	if !errors.Is(rows.Err(), context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", rows.Err())
	}
	if !c.IsClosed() {
		t.Fatal("connection must be closed")
	}
}

func TestFakeCancelKeyAndIgnoredCancel(t *testing.T) {
	// Server ignores the cancel entirely: the caller still gets control back
	// within timeout + grace.
	var s *fakeServer
	s, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		time.Sleep(10 * time.Second)
	})
	start := time.Now()
	_, err := c.Exec(ctxTimeout(t, 200*time.Millisecond), "SELECT pg_sleep(10)")
	wantType(t, err, catTimeout)
	d := time.Since(start)
	if d < 200*time.Millisecond || d > 200*time.Millisecond+cancelGrace+500*time.Millisecond {
		t.Fatalf("gave up after %v; want timeout + grace (%v)", d, cancelGrace)
	}
	select {
	case key := <-s.cancels:
		if key != [2]uint32{fakePID, fakeSecret} {
			t.Fatalf("cancel key %v, want pid %d secret %d", key, fakePID, fakeSecret)
		}
	case <-time.After(time.Second):
		t.Fatal("no cancel request was sent")
	}
}

func TestFakeNoCancelWhenFast(t *testing.T) {
	var s *fakeServer
	s, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		f.send('C', cstr("SELECT 0")...)
		f.ready('I')
		f.readUntil('X')
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := c.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	cancel() // context ends after the call returned: must not trigger a cancel request
	select {
	case <-s.cancels:
		t.Fatal("cancel request sent for a query that already finished")
	case <-time.After(200 * time.Millisecond):
	}
	if c.IsClosed() {
		t.Fatal("connection closed without cause")
	}
}

func TestFakeParameterStatusDuringQuery(t *testing.T) {
	_, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		f.param("TimeZone", "UTC")
		f.send('A', append(append(u32(1), cstr("chan")...), cstr("payload")...)...)
		f.send('C', cstr("SET")...)
		f.ready('I')
		f.readUntil('X')
	})
	tag, err := c.Exec(ctxTimeout(t, time.Second), "SET TimeZone = 'UTC'")
	if err != nil || tag != "SET" || c.ParameterStatus("TimeZone") != "UTC" {
		t.Fatalf("%v %q %v", err, tag, c.params)
	}
}

func TestFakeFatalThenClose(t *testing.T) {
	_, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		f.send('1')
		f.send('2')
		f.errorResponse("57P01", "terminating connection due to administrator command")
		// server closes without ReadyForQuery
	})
	e := wantType(t, drain(c.Query(ctxTimeout(t, time.Second), "SELECT 1")), catConnection)
	if e == nil || e.Code != "57P01" {
		t.Fatalf("SQLSTATE lost: %v", e)
	}
}

func TestFakeSimpleQuery(t *testing.T) {
	sent := make(chan string, 1)
	_, c := queryFake(t, func(f *fakeConn) {
		typ, b := f.read()
		sent <- string(typ) + strings.TrimRight(string(b), "\x00")
		f.send('T', rowDescription(Column{"QUERY PLAN", OIDJSON})...)
		f.send('D', dataRow(sp(`[{"Plan":{}}]`))...)
		f.send('C', cstr("EXPLAIN")...)
		f.send('C', cstr("SET")...)
		f.ready('I')
		f.readUntil('X')
	})
	res, err := c.SimpleQuery(ctxTimeout(t, time.Second), "EXPLAIN (GENERIC_PLAN) SELECT * FROM t WHERE id = $1; SET x = 1")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != "QEXPLAIN (GENERIC_PLAN) SELECT * FROM t WHERE id = $1; SET x = 1" {
		t.Fatalf("sent %q", got)
	}
	if len(res) != 2 || string(res[0].Rows[0][0]) != `[{"Plan":{}}]` || res[0].CommandTag != "EXPLAIN" || res[1].CommandTag != "SET" {
		t.Fatalf("%+v", res)
	}
}

func TestFakePrepareDeallocate(t *testing.T) {
	_, c := queryFake(t, func(f *fakeConn) {
		m := f.readUntil('S')
		if !strings.HasPrefix(string(m['P'][0]), "ps1\x00SELECT 1\x00") {
			f.errorResponse("XX000", "bad parse")
		} else {
			f.send('1')
		}
		f.ready('I')
		m = f.readUntil('S')
		if string(m['C'][0]) != "Sps1\x00" {
			f.errorResponse("XX000", "bad close")
		} else {
			f.send('3')
		}
		f.ready('I')
		f.readUntil('S')
		f.errorResponse("26000", "prepared statement not supported by pooler")
		f.ready('I')
		f.readUntil('X')
	})
	ctx := ctxTimeout(t, time.Second)
	if err := c.Prepare(ctx, "ps1", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Deallocate(ctx, "ps1"); err != nil {
		t.Fatal(err)
	}
	if e := wantType(t, c.Prepare(ctx, "ps2", "SELECT 1"), catPostgres); e.Code != "26000" {
		t.Fatal(e)
	}
}

func TestFakeDialFunc(t *testing.T) {
	s := startFake(t, 0, func(f *fakeConn) { f.handshake(); f.readUntil('X') })
	var dialed string
	cfg, _ := ParseConfig("postgres://alice:secret@db.internal:5432/app?sslmode=disable")
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = addr
		var d net.Dialer
		return d.DialContext(ctx, "tcp", s.addr) // e.g. an SSH channel
	}
	c, err := ConnectConfig(ctxTimeout(t, time.Second), cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if dialed != "db.internal:5432" {
		t.Fatalf("DialFunc got %q", dialed)
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
