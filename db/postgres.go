package db

import (
	"database/sql/driver"
	"net"
	"net/url"
	"strconv"

	"github.com/lib/pq"

	"github.com/bluefunda/xpro-oss/config"
)

// postgresConnector builds a lib/pq connector (a pure-Go driver, so
// PostgreSQL is always available with no CGO). The DSN is assembled as a URL
// so that credentials containing spaces, quotes or '=' are escaped properly
// instead of being spliced into a key=value string. sslmode defaults to
// "require".
func postgresConnector(t config.TargetConfig) (driver.Connector, error) {
	return pq.NewConnector(postgresDSN(t))
}

// postgresDSN returns the postgres:// URL postgresConnector connects with.
func postgresDSN(t config.TargetConfig) string {
	host := t.Host
	if t.Port > 0 {
		host = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(t.User, t.Password),
		Host:     host,
		Path:     "/" + t.Database,
		RawQuery: url.Values{"sslmode": {firstNonEmpty(t.SSLMode, "require")}}.Encode(),
	}
	return u.String()
}
