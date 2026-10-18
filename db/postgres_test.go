package db

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/bluefunda/xpro-oss/config"
)

func TestPostgresDSN(t *testing.T) {
	// Credentials full of characters that break key=value DSNs.
	const pw = `p w'd=\"x" @/?#`
	dsn := postgresDSN(config.TargetConfig{Host: "db.example", Port: 5433, User: "u ser", Password: pw, Database: "x", SSLMode: ""})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN %q does not parse: %v", dsn, err)
	}
	gotPW, _ := u.User.Password()
	if u.Scheme != "postgres" || u.Hostname() != "db.example" || u.Port() != "5433" || u.User.Username() != "u ser" || gotPW != pw || u.Path != "/x" {
		t.Errorf("round trip lost data: %+v", u)
	}
	if u.Query().Get("sslmode") != "require" {
		t.Errorf("sslmode defaults to require, got %q", u.Query().Get("sslmode"))
	}
	if got := postgresDSN(config.TargetConfig{Host: "h", SSLMode: "disable"}); !strings.Contains(got, "sslmode=disable") || strings.Contains(got, "h:") {
		t.Errorf("explicit sslmode / no port: %q", got)
	}
}

func TestPostgresConnector_Unreachable(t *testing.T) {
	if _, err := postgresConnector(config.TargetConfig{Host: "127.0.0.1", Port: 1, User: "u", Password: "p w", Database: "d"}); err != nil {
		t.Fatalf("connector should build without connecting: %v", err)
	}
	_, err := Open(context.Background(), config.TargetConfig{Host: "127.0.0.1", Port: 1, User: "u", Database: "d"}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "postgres: ping") {
		t.Fatalf("err = %v", err)
	}
}
