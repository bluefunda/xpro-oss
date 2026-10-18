package db

import (
	"context"
	"database/sql/driver"
	"fmt"
	"net"
	"strings"

	"github.com/bluefunda/btp-go/connectivity"
	go_ora "github.com/sijms/go-ora/v2"

	"github.com/bluefunda/xpro-oss/config"
)

// oracleConnector builds a go-ora connector. github.com/sijms/go-ora/v2 is a
// pure-Go wire-protocol driver — no CGO and no Oracle Instant Client needed.
//
// When t.Destination is set (and resolve is non-nil), the connection is
// resolved via the BTP Destination Service and, for OnPremise-proxied
// destinations, tunneled through the BTP Connectivity SOCKS5 proxy.
// Otherwise it connects directly using the Host/Port/User/Password/Database
// fields.
func oracleConnector(ctx context.Context, t config.TargetConfig, resolve DestinationResolver) (driver.Connector, error) {
	dest, dialer, err := resolveDestination(ctx, t, resolve, "oracle")
	if err != nil {
		return nil, err
	}

	user := firstNonEmpty(dest.User, dest.Properties["User"])
	pass := firstNonEmpty(dest.Password, dest.Properties["Password"])
	svc := firstNonEmpty(dest.Properties["ServiceName"], dest.Properties["service_name"], dest.Properties["SID"], t.Database)
	host, port := endpointFromDestination(dest)

	if host == "" {
		return nil, fmt.Errorf("oracle: no host in target destination %q", t.Destination)
	}
	if user == "" {
		return nil, fmt.Errorf("oracle: no user in target destination %q", t.Destination)
	}
	if svc == "" {
		return nil, fmt.Errorf("oracle: no service_name/SID/database configured for target destination %q", t.Destination)
	}
	if port == 0 {
		port = 1521
	}

	connector := go_ora.NewConnector(go_ora.BuildUrl(host, port, svc, user, pass, nil))
	if strings.EqualFold(dest.ProxyType, "OnPremise") && dialer != nil {
		if oc, ok := connector.(*go_ora.OracleConnector); ok {
			oc.Dialer(&btpDialer{dialer: dialer, locationID: dest.CloudConnectorLocationID})
		}
	}
	return connector, nil
}

// btpDialer adapts btp-go's connectivity.Dialer to go-ora's DialerContext
// interface, so on-premise Oracle traffic tunnels through the BTP
// Connectivity SOCKS5 proxy — the same adapter used by BlueFunda's BTP services.
type btpDialer struct {
	dialer     *connectivity.Dialer
	locationID string
}

func (d *btpDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return btpDial(ctx, d.dialer, d.locationID, "oracle", address)
}
