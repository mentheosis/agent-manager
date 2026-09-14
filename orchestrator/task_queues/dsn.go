package taskqueues

import (
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// An omitted engine means MySQL. Explicit schemes are reserved for engine selection.
// Keep errors credential-free: url.Parse errors can contain the original URL.
func parseDatabaseDSN(raw string) (*mysql.Config, error) {
	invalid := errors.New("invalid database DSN")
	raw = strings.TrimSpace(raw)
	marker := strings.Index(raw, "://")
	if marker < 0 {
		cfg, err := mysql.ParseDSN(raw)
		if err != nil {
			return nil, invalid
		}
		return cfg, nil
	}
	// A native DSN password may itself contain ://.
	if strings.Contains(raw[:marker], "@") || strings.Contains(raw[:marker], ":") {
		cfg, err := mysql.ParseDSN(raw)
		if err != nil {
			return nil, invalid
		}
		return cfg, nil
	}
	if !strings.EqualFold(raw[:marker], "mysql") {
		return nil, errors.New("unsupported database engine; currently only mysql is supported")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || u.Fragment != "" || u.Path == "" || u.Path == "/" {
		return nil, invalid
	}
	cfg := mysql.NewConfig()
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(u.Hostname(), port)
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	// Delegate driver options and validation to the same parser used for native DSNs.
	native := cfg.FormatDSN()
	if u.RawQuery != "" {
		separator := "?"
		if strings.Contains(native, "?") {
			separator = "&"
		}
		native += separator + u.RawQuery
	}
	parsed, err := mysql.ParseDSN(native)
	if err != nil {
		return nil, invalid
	}
	return parsed, nil
}
