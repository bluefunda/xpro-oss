package btp

import (
	"context"
	"fmt"

	"github.com/bluefunda/btp-go/connectivity"
	"github.com/bluefunda/btp-go/destination"
	"github.com/bluefunda/btp-go/xsuaa"
)

// Resolve looks up name in the BTP Destination Service and returns the
// resolved destination plus a Connectivity dialer for tunneling on-premise
// traffic through Cloud Connector. Callers should route through the dialer
// only when dest.ProxyType == "OnPremise" (building the dialer itself
// performs no network I/O, so it's always cheap to return one). Shared by
// any source or target that needs to resolve a named BTP destination —
// source.SFTP and the db drivers all call this. A nil *Bindings is valid and
// returns ErrNoBinding, so b.Resolve can be passed around unconditionally.
func (b *Bindings) Resolve(ctx context.Context, name string) (*destination.Destination, *connectivity.Dialer, error) {
	if b == nil || b.Destination == nil || b.Connectivity == nil {
		return nil, nil, ErrNoBinding
	}
	destTokens := xsuaa.NewClientCredentialsSource(xsuaa.Config{
		TokenURL:     b.Destination.TokenServiceURL + "/oauth/token",
		ClientID:     b.Destination.ClientID,
		ClientSecret: b.Destination.ClientSecret,
	})
	destClient := destination.NewClient(b.Destination.URI, destTokens, nil)
	dest, err := destClient.Find(ctx, name)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve destination %q: %w", name, err)
	}

	connTokens := xsuaa.NewClientCredentialsSource(xsuaa.Config{
		TokenURL:     b.Connectivity.TokenServiceURL + "/oauth/token",
		ClientID:     b.Connectivity.ClientID,
		ClientSecret: b.Connectivity.ClientSecret,
	})
	proxyPort := b.Connectivity.OnpremiseProxySocks5Port
	if proxyPort == "" {
		proxyPort = "20004"
	}
	dialer := connectivity.NewDialer(connectivity.Config{
		ProxyHost:   b.Connectivity.OnpremiseProxyHost,
		ProxyPort:   proxyPort,
		TokenSource: connTokens,
	})

	return dest, dialer, nil
}
