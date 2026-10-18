// Package btp detects SAP BTP service bindings and resolves named BTP
// Destination Service entries (plus a Connectivity dialer for on-premise
// tunneling). Everything is optional: Detect returns nil outside Cloud
// Foundry/Kyma, and a nil *Bindings is safe to use — Resolve then reports
// ErrNoBinding.
package btp

import (
	"errors"
	"log/slog"

	"github.com/bluefunda/btp-go/binding"
	"github.com/bluefunda/btp-go/binding/auto"
)

// ErrNoBinding is returned by Resolve when no BTP Connectivity + Destination
// binding was detected in this environment.
var ErrNoBinding = errors.New("no BTP Connectivity/Destination binding detected in this environment")

// Bindings holds resolved SAP BTP service bindings.
type Bindings struct {
	Connectivity *binding.ConnectivityBinding
	Destination  *binding.DestinationBinding
}

// Detect resolves BTP Connectivity and Destination service bindings via
// btp-go's binding/auto (VCAP_SERVICES on Cloud Foundry, SERVICE_BINDING_ROOT
// or /bindings on Kyma). It returns nil (not an error) when no BTP
// environment is present — the expected, common case outside Cloud
// Foundry/Kyma.
func Detect() *Bindings {
	provider, err := auto.NewProvider()
	if err != nil {
		slog.Debug("btp binding not detected", "err", err)
		return nil
	}
	conn, err := binding.Connectivity(provider, "")
	if err != nil {
		slog.Debug("btp connectivity binding not detected", "err", err)
		return nil
	}
	dest, err := binding.Destination(provider, "")
	if err != nil {
		slog.Debug("btp destination binding not detected", "err", err)
		return nil
	}
	return &Bindings{Connectivity: conn, Destination: dest}
}
