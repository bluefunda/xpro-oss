package btp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bluefunda/btp-go/binding"
)

// btpServer fakes both the XSUAA token endpoint and the Destination Service.
func btpServer(t *testing.T, destStatus int) *Bindings {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	})
	mux.HandleFunc("/destination-configuration/v1/destinations/MY_DEST", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(destStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{"destinationConfiguration": map[string]string{
			"Name": "MY_DEST", "ProxyType": "OnPremise", "host": "db.internal", "port": "30015",
			"user": "u", "password": "p", "CloudConnectorLocationId": "loc1",
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Bindings{
		Destination: &binding.DestinationBinding{URI: srv.URL, TokenServiceURL: srv.URL, ClientID: "id", ClientSecret: "s"},
		Connectivity: &binding.ConnectivityBinding{
			TokenServiceURL: srv.URL, ClientID: "id", ClientSecret: "s", OnpremiseProxyHost: "127.0.0.1",
		},
	}
}

func TestBTPResolve(t *testing.T) {
	b := btpServer(t, http.StatusOK)
	dest, dialer, err := b.Resolve(context.Background(), "MY_DEST")
	if err != nil {
		t.Fatal(err)
	}
	if dest.Host != "db.internal" || dest.Port != "30015" || dest.User != "u" || dest.ProxyType != "OnPremise" || dest.CloudConnectorLocationID != "loc1" {
		t.Errorf("dest = %+v", dest)
	}
	if dialer == nil {
		t.Error("dialer is nil")
	}
}

func TestBTPResolve_Errors(t *testing.T) {
	b := btpServer(t, http.StatusInternalServerError)
	if _, _, err := b.Resolve(context.Background(), "MY_DEST"); err == nil || !strings.Contains(err.Error(), `resolve destination "MY_DEST"`) {
		t.Errorf("destination 500: err = %v", err)
	}
	if _, _, err := b.Resolve(context.Background(), "MISSING"); err == nil {
		t.Error("unknown destination should fail")
	}
}

func TestBindings_NilResolve(t *testing.T) {
	var b *Bindings
	if _, _, err := b.Resolve(context.Background(), "X"); !errors.Is(err, ErrNoBinding) {
		t.Errorf("err = %v, want ErrNoBinding", err)
	}
}

func TestDetect_AbsentIsNil(t *testing.T) {
	t.Setenv("VCAP_SERVICES", "")
	t.Setenv("SERVICE_BINDING_ROOT", t.TempDir())
	if b := Detect(); b != nil {
		t.Errorf("Detect = %+v, want nil without bindings", b)
	}
}
