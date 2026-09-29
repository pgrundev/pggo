package postgres

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
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	perr "github.com/pgrundev/pggo/internal/errors"
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
		b = append(b, u32(c.Type)...)
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
	c, err := ParseURL("postgres://alice:secret@" + s.addr + "/app?sslmode=disable" + extra)
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

func wantType(t *testing.T, err error, typ string) *perr.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s error, got nil", typ)
	}
	e := perr.Classify(err)
	if e.Type != typ {
		t.Fatalf("got %s (%v), want %s", e.Type, e, typ)
	}
	return e
}

func TestFakeStartupParams(t *testing.T) {
	got := make(chan map[string]string, 1)
	s := startFake(t, 0, func(f *fakeConn) {
		got <- f.params
		f.handshake()
		f.readUntil('X')
	})
	c, err := Connect(ctxTimeout(t, 2*time.Second), s.cfg(t, "&application_name=myagent&search_path=app"))
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
			c, err := Connect(ctxTimeout(t, 2*time.Second), s.cfg(t, ""))
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
		_, err := Connect(ctxTimeout(t, 2*time.Second), cfg)
		e := wantType(t, err, perr.Authentication)
		if !strings.Contains(e.Message, "password") {
			t.Fatalf("auth %d: %v", code, e)
		}
	}
}

func TestFakeUnsupportedAuth(t *testing.T) {
	for _, body := range [][]byte{u32(7), u32(9), append(append(u32(10), cstr("SCRAM-SHA-256-PLUS")...), 0)} {
		s := startFake(t, 0, func(f *fakeConn) { f.send('R', body...); f.read() })
		wantType(t, connectErr(t, s, ""), perr.Authentication)
	}
}

func connectErr(t *testing.T, s *fakeServer, extra string) error {
	t.Helper()
	c, err := Connect(ctxTimeout(t, 2*time.Second), s.cfg(t, extra))
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
			wantType(t, connectErr(t, s, ""), perr.Authentication)
		})
	}
}

func TestFakeStartupError(t *testing.T) {
	s := startFake(t, 0, func(f *fakeConn) { f.errorResponse("3D000", `database "app" does not exist`) })
	e := wantType(t, connectErr(t, s, ""), perr.Postgres)
	if e.Code != "3D000" {
		t.Fatalf("%+v", e)
	}
	s = startFake(t, 0, func(f *fakeConn) { f.errorResponse("53300", "too many clients") })
	if e := wantType(t, connectErr(t, s, ""), perr.Postgres); !e.Retryable {
		t.Fatal("too many connections should be retryable")
	}
}

func TestFakeServerClosesDuringStartup(t *testing.T) {
	s := startFake(t, 0, func(f *fakeConn) {})
	wantType(t, connectErr(t, s, ""), perr.Connection)
	s = startFake(t, 0, func(f *fakeConn) { f.authOK() })
	wantType(t, connectErr(t, s, ""), perr.Connection)
}

func TestFakeStartupStallTimesOut(t *testing.T) {
	s := startFake(t, 0, func(f *fakeConn) { f.authOK(); time.Sleep(3 * time.Second) })
	start := time.Now()
	cfg := s.cfg(t, "")
	cfg.Timeout = 300 * time.Millisecond
	_, err := Connect(ctxTimeout(t, 300*time.Millisecond), cfg)
	e := wantType(t, err, perr.Timeout)
	if time.Since(start) > time.Second || !strings.Contains(e.Message, "300ms") {
		t.Fatalf("took %v: %v", time.Since(start), e)
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
		e := wantType(t, connectErr(t, s, "&sslmode="+mode), perr.Connection)
		if !strings.Contains(e.Message, "SSL") {
			t.Fatalf("%s: %v", mode, e)
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
	wantType(t, connectErr(t, s, ""), perr.Protocol)
	s = startFake(t, 0, func(f *fakeConn) { f.send('R') })
	wantType(t, connectErr(t, s, ""), perr.Protocol)
}

// queryFake connects to a server that completes the handshake and then runs script.
func queryFake(t *testing.T, script func(f *fakeConn)) (*fakeServer, *Conn) {
	t.Helper()
	s := startFake(t, 0, func(f *fakeConn) { f.handshake(); script(f) })
	c, err := Connect(ctxTimeout(t, 2*time.Second), s.cfg(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return s, c
}

func TestFakeQueryWireFormat(t *testing.T) {
	seen := make(chan map[byte][][]byte, 1)
	_, c := queryFake(t, func(f *fakeConn) {
		seen <- f.readUntil('S')
		f.send('1')
		f.send('2')
		f.send('T', rowDescription(Column{"id", oidInt4}, Column{"name", 25})...)
		f.send('N', 'M', 'n', 'o', 't', 'e', 0, 0) // notices are ignored
		f.send('D', dataRow(sp("1"), sp("a"))...)
		f.send('D', dataRow(sp("2"), nil)...)
		f.send('C', cstr("SELECT 2")...)
		f.ready('I')
		f.readUntil('X')
	})
	var rows [][]string
	var cols []Column
	res, err := c.Run(ctxTimeout(t, 2*time.Second), &Request{
		SQL:       "SELECT id, name FROM t WHERE a = $1 AND b = $2 AND c = $3",
		Params:    []*string{sp("x"), nil, sp("")},
		MaxRows:   7,
		OnColumns: func(c []Column) { cols = c },
		OnRow: func(v [][]byte) {
			r := []string{}
			for _, b := range v {
				if b == nil {
					r = append(r, "<NULL>")
				} else {
					r = append(r, string(b))
				}
			}
			rows = append(rows, r)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommandTag != "SELECT 2" || len(cols) != 2 || cols[0].Name != "id" || cols[0].Type != oidInt4 {
		t.Fatalf("%+v %+v", res, cols)
	}
	if fmt.Sprint(rows) != "[[1 a] [2 <NULL>]]" {
		t.Fatalf("rows %v", rows)
	}
	m := <-seen
	parse := m['P'][0]
	if string(parse) != "\x00SELECT id, name FROM t WHERE a = $1 AND b = $2 AND c = $3\x00\x00\x00" {
		t.Fatalf("Parse %q", parse)
	}
	bind := m['B'][0]
	wantBind := "\x00\x00\x00\x00\x00\x03" + "\x00\x00\x00\x01x" + "\xff\xff\xff\xff" + "\x00\x00\x00\x00" + "\x00\x00"
	if string(bind) != wantBind {
		t.Fatalf("Bind %q\nwant %q", bind, wantBind)
	}
	if string(m['D'][0]) != "P\x00" {
		t.Fatalf("Describe %q", m['D'][0])
	}
	if string(m['E'][0]) != "\x00\x00\x00\x00\x07" {
		t.Fatalf("Execute %q", m['E'][0])
	}
	if _, ok := m['Q']; ok {
		t.Fatal("non-read-only request must not send BEGIN")
	}
}

func TestFakeReadOnlyPipeline(t *testing.T) {
	seen := make(chan []string, 1)
	_, c := queryFake(t, func(f *fakeConn) {
		var order []string
		for len(order) < 7 {
			typ, b := f.read()
			if typ == 'Q' {
				order = append(order, "Q:"+strings.TrimRight(string(b), "\x00"))
			} else {
				order = append(order, string(typ))
			}
		}
		seen <- order
		f.send('C', cstr("BEGIN")...)
		f.ready('T')
		f.send('1')
		f.send('2')
		f.send('T', rowDescription(Column{"n", oidInt4})...)
		f.send('D', dataRow(sp("1"))...)
		f.send('D', dataRow(sp("2"))...)
		f.send('s') // PortalSuspended
		f.ready('T')
		f.send('C', cstr("ROLLBACK")...)
		f.ready('I')
		f.readUntil('X')
	})
	n := 0
	res, err := c.Run(ctxTimeout(t, 2*time.Second), &Request{SQL: "SELECT n", ReadOnly: true, MaxRows: 2, OnRow: func([][]byte) { n++ }})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(<-seen, " "); got != "Q:BEGIN READ ONLY P B D E S Q:ROLLBACK" {
		t.Fatalf("pipeline order: %s", got)
	}
	if !res.Suspended || n != 2 || res.CommandTag != "" {
		t.Fatalf("%+v rows=%d", res, n)
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
	e := wantType(t, func() error {
		_, err := c.Run(ctxTimeout(t, time.Second), &Request{SQL: "SELECT * FROM x"})
		return err
	}(), perr.Postgres)
	if e.Code != "42P01" {
		t.Fatal(e)
	}
	// The connection is still usable: the error path consumed everything up to ReadyForQuery.
	if _, err := c.Run(ctxTimeout(t, time.Second), &Request{SQL: "SELECT 1"}); err != nil {
		t.Fatalf("connection not reusable after error: %v", err)
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
		"huge message length": func(f *fakeConn) {
			f.c.Write([]byte{'D', 0x7F, 0xFF, 0xFF, 0xFF})
		},
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
			_, err := c.Run(ctxTimeout(t, 2*time.Second), &Request{SQL: "SELECT a", OnRow: func([][]byte) {}})
			wantType(t, err, perr.Protocol)
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
	_, err := c.Run(ctxTimeout(t, 2*time.Second), &Request{SQL: "SELECT a", OnRow: func([][]byte) {}})
	e := wantType(t, err, perr.Connection)
	if !e.Retryable {
		t.Fatal("dropped connection should be retryable")
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
	c.timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := c.Run(ctxTimeout(t, 200*time.Millisecond), &Request{SQL: "SELECT pg_sleep(10)"})
	e := wantType(t, err, perr.Timeout)
	if e.Code != "57014" || !strings.Contains(e.Message, "200ms") {
		t.Fatalf("%+v", e)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestFakeCancelKeyAndIgnoredCancel(t *testing.T) {
	// Server ignores the cancel entirely: pggo must still give up after the grace period.
	var s *fakeServer
	s, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		time.Sleep(10 * time.Second)
	})
	start := time.Now()
	_, err := c.Run(ctxTimeout(t, 200*time.Millisecond), &Request{SQL: "SELECT pg_sleep(10)"})
	wantType(t, err, perr.Timeout)
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
	if _, err := c.Run(ctx, &Request{SQL: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	cancel() // context ends after Run returned: must not trigger a cancel request
	select {
	case <-s.cancels:
		t.Fatal("cancel request sent for a query that already finished")
	case <-time.After(200 * time.Millisecond):
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
	res, err := c.Run(ctxTimeout(t, time.Second), &Request{SQL: "SET TimeZone = 'UTC'"})
	if err != nil || res.CommandTag != "SET" || c.params["TimeZone"] != "UTC" {
		t.Fatalf("%v %+v %v", err, res, c.params)
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestFakeFatalThenClose(t *testing.T) {
	_, c := queryFake(t, func(f *fakeConn) {
		f.readUntil('S')
		f.send('1')
		f.send('2')
		f.errorResponse("57P01", "terminating connection due to administrator command")
		// server closes without ReadyForQuery
	})
	e := wantType(t, func() error { _, err := c.Run(ctxTimeout(t, time.Second), &Request{SQL: "SELECT 1"}); return err }(), perr.Connection)
	if e.Code != "57P01" || !e.Retryable {
		t.Fatalf("SQLSTATE lost: %+v", e)
	}
}
