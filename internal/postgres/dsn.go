package postgres

import (
	"net/url"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	perr "github.com/pgrundev/pggo/internal/errors"
)

// Config is a parsed connection string. Only what pgGo needs.
type Config struct {
	Host        string // hostname, IP, or unix socket directory (starts with "/")
	Port        int
	User        string
	Password    string
	Database    string
	SSLMode     string // disable | allow | prefer | require | verify-ca | verify-full
	SSLRootCert string
	Params      map[string]string // extra startup parameters (runtime settings)
	Timeout     time.Duration     // the user's --timeout, used in error messages
}

// libpq parameters that pgGo accepts but does not act on; never sent to the server.
var ignoredParams = map[string]bool{
	"channel_binding": true, "connect_timeout": true, "sslcert": true, "sslkey": true,
	"sslcrl": true, "sslnegotiation": true, "target_session_attrs": true, "gssencmode": true,
	"krbsrvname": true, "passfile": true, "sslcompression": true, "keepalives": true,
	"keepalives_idle": true, "keepalives_interval": true, "keepalives_count": true,
}

// ParseURL parses postgres:// / postgresql:// URLs and libpq keyword=value strings,
// filling unset fields from the standard PG* environment variables.
func ParseURL(s string) (*Config, error) {
	c := &Config{Params: map[string]string{}}
	kv := map[string]string{}
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "postgres://"), strings.HasPrefix(s, "postgresql://"):
		u, err := url.Parse(s)
		if err != nil {
			return nil, perr.New(perr.InvalidInput, "invalid connection URL: %v", redact(err.Error()))
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
	case s == "":
	default:
		var err error
		if kv, err = parseKeywordValue(s); err != nil {
			return nil, err
		}
	}

	env := func(key, envName, def string) string {
		if v, ok := kv[key]; ok && v != "" {
			return v
		}
		if v := os.Getenv(envName); v != "" {
			return v
		}
		return def
	}
	c.Host = env("host", "PGHOST", "localhost")
	c.User = env("user", "PGUSER", "")
	if c.User == "" {
		if u, err := user.Current(); err == nil {
			c.User = u.Username
		}
	}
	c.Password = env("password", "PGPASSWORD", "")
	c.Database = env("dbname", "PGDATABASE", c.User)
	c.SSLMode = env("sslmode", "PGSSLMODE", "prefer")
	c.SSLRootCert = env("sslrootcert", "PGSSLROOTCERT", "")
	port, err := strconv.Atoi(env("port", "PGPORT", "5432"))
	if err != nil || port <= 0 || port > 65535 {
		return nil, perr.New(perr.InvalidInput, "invalid port %q", env("port", "PGPORT", "5432"))
	}
	c.Port = port
	switch c.SSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return nil, perr.New(perr.InvalidInput, "invalid sslmode %q", c.SSLMode)
	}
	for k, v := range kv {
		switch k {
		case "host", "port", "user", "password", "dbname", "sslmode", "sslrootcert":
		default:
			if !ignoredParams[k] {
				c.Params[k] = v
			}
		}
	}
	return c, nil
}

func parseKeywordValue(s string) (map[string]string, error) {
	kv := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t\n")
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, perr.New(perr.InvalidInput, "invalid connection string: expected postgres://... URL or key=value pairs")
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
				return nil, perr.New(perr.InvalidInput, "invalid connection string: unterminated quoted value for %q", key)
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

// redact strips anything that looks like a password from URL parse errors.
func redact(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		if at := strings.LastIndex(s, "@"); at > i {
			return s[:i+3] + "***" + s[at:]
		}
	}
	return s
}
