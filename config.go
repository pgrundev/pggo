package pggo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Config describes how to connect. Build one with ParseConfig and adjust fields
// before calling ConnectConfig.
type Config struct {
	Host        string // hostname, IP, or unix socket directory (starts with "/")
	Port        int
	User        string
	Password    string
	Database    string
	SSLMode     string // disable | allow | prefer | require | verify-ca | verify-full
	SSLRootCert string

	// RuntimeParams are sent in the startup message as session settings
	// (application_name, search_path, ...).
	RuntimeParams map[string]string

	// DialFunc, if set, opens the network connection instead of net.Dialer
	// (e.g. through an SSH tunnel). TLS is negotiated on top of what it returns.
	DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

	// sessionSQL runs right after authentication (from Connect options).
	sessionSQL []string
}

// Copy returns a deep copy of c.
func (c *Config) Copy() *Config {
	n := *c
	n.RuntimeParams = make(map[string]string, len(c.RuntimeParams))
	for k, v := range c.RuntimeParams {
		n.RuntimeParams[k] = v
	}
	n.sessionSQL = append([]string(nil), c.sessionSQL...)
	return &n
}

// libpq parameters that are accepted but not acted on; never sent to the server.
var ignoredParams = map[string]bool{
	"channel_binding": true, "connect_timeout": true, "sslcert": true, "sslkey": true,
	"sslcrl": true, "sslnegotiation": true, "target_session_attrs": true, "gssencmode": true,
	"krbsrvname": true, "sslcompression": true, "keepalives": true, "keepalives_idle": true,
	"keepalives_interval": true, "keepalives_count": true, "sslpassword": true, "requiressl": true,
}

// ParseConfig parses a postgres:// URL or a libpq keyword=value string. Unset
// fields come from the libpq environment (PGHOST, PGPORT, PGUSER, PGPASSWORD,
// PGDATABASE, PGSSLMODE, PGSSLROOTCERT), a connection service file
// (service=name or PGSERVICE; PGSERVICEFILE or ~/.pg_service.conf), and the
// password file (PGPASSFILE or ~/.pgpass), in libpq's precedence order.
func ParseConfig(connString string) (*Config, error) {
	kv, err := parseConnString(connString)
	if err != nil {
		return nil, err
	}
	service := kv["service"]
	if service == "" {
		service = os.Getenv("PGSERVICE")
	}
	if service != "" {
		svc, err := readService(service)
		if err != nil {
			return nil, err
		}
		for k, v := range svc {
			if _, set := kv[k]; !set {
				kv[k] = v
			}
		}
	}
	delete(kv, "service")

	get := func(key, env, def string) string {
		if v := kv[key]; v != "" {
			return v
		}
		if v := os.Getenv(env); v != "" {
			return v
		}
		return def
	}
	c := &Config{RuntimeParams: map[string]string{}}
	c.Host = get("host", "PGHOST", "localhost")
	c.User = get("user", "PGUSER", "")
	if c.User == "" {
		if u, err := user.Current(); err == nil {
			c.User = u.Username
		}
	}
	c.Database = get("dbname", "PGDATABASE", c.User)
	c.SSLMode = get("sslmode", "PGSSLMODE", "prefer")
	c.SSLRootCert = get("sslrootcert", "PGSSLROOTCERT", "")
	portStr := get("port", "PGPORT", "5432")
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("pggo: invalid port %q", portStr)
	}
	c.Port = port
	switch c.SSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return nil, fmt.Errorf("pggo: invalid sslmode %q", c.SSLMode)
	}
	c.Password = get("password", "PGPASSWORD", "")
	if c.Password == "" {
		c.Password = passfileLookup(get("passfile", "PGPASSFILE", ""), c)
	}
	for k, v := range kv {
		switch k {
		case "host", "port", "user", "password", "dbname", "sslmode", "sslrootcert", "passfile":
		default:
			if !ignoredParams[k] {
				c.RuntimeParams[k] = v
			}
		}
	}
	return c, nil
}

// Addr is the dial address: host:port, or the unix socket path.
func (c *Config) Addr() (network, addr string) {
	if strings.HasPrefix(c.Host, "/") {
		return "unix", fmt.Sprintf("%s/.s.PGSQL.%d", c.Host, c.Port)
	}
	return "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func parseConnString(s string) (map[string]string, error) {
	kv := map[string]string{}
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "postgres://"), strings.HasPrefix(s, "postgresql://"):
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("pggo: invalid connection URL: %s", redact(err.Error()))
		}
		if u.User != nil {
			kv["user"] = u.User.Username()
			if p, ok := u.User.Password(); ok {
				kv["password"] = p
			}
		}
		if h := u.Hostname(); h != "" {
			kv["host"] = h
		}
		if p := u.Port(); p != "" {
			kv["port"] = p
		}
		if db := strings.TrimPrefix(u.Path, "/"); db != "" {
			kv["dbname"] = db
		}
		for k, vs := range u.Query() {
			if len(vs) > 0 {
				kv[k] = vs[len(vs)-1]
			}
		}
		return kv, nil
	case s == "":
		return kv, nil
	}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t\n")
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, errors.New("pggo: invalid connection string: expected postgres://... URL or key=value pairs")
		}
		key := strings.TrimSpace(s[:eq])
		s = strings.TrimLeft(s[eq+1:], " \t")
		var val strings.Builder
		if strings.HasPrefix(s, "'") {
			s = s[1:]
			closed := false
			for i := 0; i < len(s); i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					val.WriteByte(s[i])
				} else if s[i] == '\'' {
					s = s[i+1:]
					closed = true
					break
				} else {
					val.WriteByte(s[i])
				}
			}
			if !closed {
				return nil, fmt.Errorf("pggo: invalid connection string: unterminated quoted value for %q", key)
			}
		} else {
			end := strings.IndexAny(s, " \t\n")
			if end < 0 {
				end = len(s)
			}
			val.WriteString(s[:end])
			s = s[end:]
		}
		kv[key] = val.String()
	}
	return kv, nil
}

// readService returns the parameters of [name] from the connection service
// file: PGSERVICEFILE, else ~/.pg_service.conf, else $PGSYSCONFDIR/pg_service.conf.
// An unknown service is an error, as in libpq.
func readService(name string) (map[string]string, error) {
	var files []string
	if f := os.Getenv("PGSERVICEFILE"); f != "" {
		files = []string{f}
	} else {
		if home, err := os.UserHomeDir(); err == nil {
			files = append(files, filepath.Join(home, ".pg_service.conf"))
		}
		if d := os.Getenv("PGSYSCONFDIR"); d != "" {
			files = append(files, filepath.Join(d, "pg_service.conf"))
		}
	}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		params, found, err := scanServiceFile(fh, name)
		fh.Close()
		if err != nil {
			return nil, fmt.Errorf("pggo: %s: %w", f, err)
		}
		if found {
			return params, nil
		}
	}
	return nil, fmt.Errorf("pggo: definition of service %q not found", name)
}

func scanServiceFile(f *os.File, name string) (map[string]string, bool, error) {
	sc := bufio.NewScanner(f)
	params := map[string]string{}
	in, found := false, false
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			if !strings.HasSuffix(line, "]") {
				return nil, false, fmt.Errorf("line %d: malformed section header", n)
			}
			in = line[1:len(line)-1] == name
			found = found || in
			continue
		}
		if !in {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, false, fmt.Errorf("line %d: expected key=value", n)
		}
		params[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return params, found, sc.Err()
}

// passfileLookup returns the first matching password from the password file
// (hostname:port:database:username:password, * wildcards, \ escapes).
func passfileLookup(path string, c *Config) string {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, ".pgpass")
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return "" // libpq ignores a password file readable by group/others
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	host := c.Host
	if strings.HasPrefix(host, "/") {
		host = "localhost" // libpq matches socket connections as localhost
	}
	want := []string{host, strconv.Itoa(c.Port), c.Database, c.User}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := splitPassLine(line)
		if len(fields) != 5 {
			continue
		}
		match := true
		for i, w := range want {
			if fields[i] != "*" && fields[i] != w {
				match = false
				break
			}
		}
		if match {
			return fields[4]
		}
	}
	return ""
}

func splitPassLine(line string) []string {
	var fields []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case c == ':' && len(fields) < 4:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(fields, cur.String())
}

// redact strips anything that looks like a password from URL parse errors.
func redact(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		if at := strings.LastIndex(s, "@"); at > i {
			return s[:i+3] + "***" + s[at:]
		}
	}
	return s
}
