package db

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql/driver"
	"fmt"
	"net"
	"strconv"
	"strings"

	hdb "github.com/SAP/go-hdb/driver"
	"github.com/SAP/go-hdb/driver/dial"
	"github.com/SAP/go-hdb/sqlscript"
	"github.com/bluefunda/btp-go/connectivity"
	"github.com/bluefunda/btp-go/destination"

	"github.com/bluefunda/xpro-oss/config"
)

// hanaDefaultPort is the HANA Cloud SQL port; on-premise systems use
// 3<instance>15 (e.g. 30015) and must set target.port explicitly.
const hanaDefaultPort = 443

// hanaConnector builds a go-hdb connector. github.com/SAP/go-hdb is SAP's own
// pure-Go driver — no CGO and no HANA client install needed.
//
// HANA has no SAVEPOINT statement; it doesn't need one here (see HANA
// dialect): a failing INSERT is rolled back at statement level and the
// surrounding transaction stays usable.
//
// When t.Destination is set (and resolve is non-nil), the connection is
// resolved via the BTP Destination Service and, for OnPremise-proxied
// destinations, tunneled through the BTP Connectivity SOCKS5 proxy.
// Otherwise it connects directly using the Host/Port/User/Password/Database
// fields.
func hanaConnector(ctx context.Context, t config.TargetConfig, resolve DestinationResolver) (driver.Connector, error) {
	dest, dialer, err := resolveDestination(ctx, t, resolve, "hana")
	if err != nil {
		return nil, err
	}
	cc, err := hanaConnectorConfig(t, dest, dialer)
	if err != nil {
		return nil, err
	}
	conn, err := hdb.NewConfigConnector(cc)
	if err != nil {
		return nil, fmt.Errorf("hana: connector: %w", err)
	}
	return conn, nil
}

// hanaConnectorConfig builds the go-hdb connector configuration from the
// target config and the resolved destination. Pure (no I/O) so it can be
// tested without a database.
//
// target.sslmode: "" or "require" = TLS, verifying the server certificate
// against the system roots (HANA Cloud mandates TLS); "skip-verify" = TLS
// without certificate verification; "disable" = plain TCP (on-premise dev
// systems only).
func hanaConnectorConfig(t config.TargetConfig, dest *destination.Destination, dialer *connectivity.Dialer) (*hdb.ConnectorConfig, error) {
	host, port := endpointFromDestination(dest)
	user := firstNonEmpty(dest.User, dest.Properties["User"])
	pass := firstNonEmpty(dest.Password, dest.Properties["Password"])

	if host == "" {
		return nil, fmt.Errorf("hana: no host in target destination %q", t.Destination)
	}
	if user == "" {
		return nil, fmt.Errorf("hana: no user in target destination %q", t.Destination)
	}
	if port == 0 {
		port = hanaDefaultPort
	}

	cc := hdb.NewConnectorConfig()
	cc.Host = net.JoinHostPort(host, strconv.Itoa(port))
	cc.Username = user
	cc.Password = pass
	cc.DatabaseName = firstNonEmpty(dest.Properties["DatabaseName"], t.Database)

	switch t.SSLMode {
	case "", "require":
		cc.TLSConfig = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	case "skip-verify":
		cc.TLSConfig = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec // explicit opt-in
	case "disable":
	default:
		return nil, fmt.Errorf("hana: unsupported sslmode %q (want require, skip-verify or disable)", t.SSLMode)
	}

	if strings.EqualFold(dest.ProxyType, "OnPremise") && dialer != nil {
		cc.Dialer = &hanaDialer{dialer: dialer, locationID: dest.CloudConnectorLocationID}
	}
	return cc, nil
}

// hanaDialer adapts btp-go's connectivity.Dialer to go-hdb's dial.Dialer, so
// on-premise HANA traffic tunnels through the BTP Connectivity SOCKS5 proxy.
type hanaDialer struct {
	dialer     *connectivity.Dialer
	locationID string
}

func (d *hanaDialer) DialContext(ctx context.Context, address string, _ dial.DialerOptions) (net.Conn, error) {
	return btpDial(ctx, d.dialer, d.locationID, "hana", address)
}

// splitStatements splits a HANA SQL script into individual statements using
// go-hdb's sqlscript scanner, dropping empty ones.
func splitStatements(script string) ([]string, error) {
	sc := bufio.NewScanner(strings.NewReader(script))
	sc.Split(sqlscript.ScanFunc(';', false))
	var out []string
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			out = append(out, s)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("hana: parse script: %w", err)
	}
	return out, nil
}
