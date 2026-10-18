package db

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"

	"github.com/SAP/go-hdb/driver/dial"
	"github.com/bluefunda/btp-go/connectivity"
	"github.com/bluefunda/btp-go/destination"

	"github.com/bluefunda/xpro-oss/config"
)

func TestHANAConnectorConfig(t *testing.T) {
	direct := func() *destination.Destination {
		return &destination.Destination{Host: "hana.example", Port: "30015", User: "u", Password: "p"}
	}

	t.Run("direct defaults to verified TLS", func(t *testing.T) {
		cc, err := hanaConnectorConfig(config.TargetConfig{Database: "TENANT"}, direct(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if cc.Host != "hana.example:30015" || cc.Username != "u" || cc.Password != "p" || cc.DatabaseName != "TENANT" {
			t.Errorf("cfg = %+v", cc)
		}
		if cc.TLSConfig == nil || cc.TLSConfig.InsecureSkipVerify || cc.TLSConfig.ServerName != "hana.example" || cc.TLSConfig.MinVersion != tls.VersionTLS12 {
			t.Errorf("TLSConfig = %+v", cc.TLSConfig)
		}
	})

	t.Run("default port is HANA Cloud 443", func(t *testing.T) {
		d := direct()
		d.Port = "0"
		cc, err := hanaConnectorConfig(config.TargetConfig{}, d, nil)
		if err != nil || cc.Host != "hana.example:443" {
			t.Fatalf("host=%q err=%v", cc.Host, err)
		}
	})

	t.Run("sslmode", func(t *testing.T) {
		cc, _ := hanaConnectorConfig(config.TargetConfig{SSLMode: "disable"}, direct(), nil)
		if cc.TLSConfig != nil {
			t.Error("disable should leave TLS off")
		}
		cc, _ = hanaConnectorConfig(config.TargetConfig{SSLMode: "skip-verify"}, direct(), nil)
		if cc.TLSConfig == nil || !cc.TLSConfig.InsecureSkipVerify {
			t.Error("skip-verify should enable TLS without verification")
		}
		if _, err := hanaConnectorConfig(config.TargetConfig{SSLMode: "bogus"}, direct(), nil); err == nil || !strings.Contains(err.Error(), "sslmode") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("destination properties and URL host", func(t *testing.T) {
		d := &destination.Destination{
			URL:        "https://hc.example:443",
			Properties: map[string]string{"User": "pu", "Password": "pp", "DatabaseName": "DB1"},
		}
		cc, err := hanaConnectorConfig(config.TargetConfig{Database: "ignored"}, d, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cc.Host != "hc.example:443" || cc.Username != "pu" || cc.Password != "pp" || cc.DatabaseName != "DB1" {
			t.Errorf("cfg = %+v", cc)
		}
	})

	t.Run("missing host and user", func(t *testing.T) {
		if _, err := hanaConnectorConfig(config.TargetConfig{}, &destination.Destination{User: "u"}, nil); err == nil || !strings.Contains(err.Error(), "no host") {
			t.Errorf("err = %v", err)
		}
		if _, err := hanaConnectorConfig(config.TargetConfig{}, &destination.Destination{Host: "h"}, nil); err == nil || !strings.Contains(err.Error(), "no user") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("on-premise destination installs BTP dialer", func(t *testing.T) {
		d := direct()
		d.ProxyType = "onpremise"
		d.CloudConnectorLocationID = "loc"
		dialer := connectivity.NewDialer(connectivity.Config{ProxyHost: "127.0.0.1", ProxyPort: "1"})
		cc, err := hanaConnectorConfig(config.TargetConfig{}, d, dialer)
		if err != nil {
			t.Fatal(err)
		}
		hd, ok := cc.Dialer.(*hanaDialer)
		if !ok || hd.locationID != "loc" || hd.dialer != dialer {
			t.Errorf("Dialer = %#v", cc.Dialer)
		}

		// Internet-proxied destinations connect directly.
		d.ProxyType = "Internet"
		cc, _ = hanaConnectorConfig(config.TargetConfig{}, d, dialer)
		if _, ok := cc.Dialer.(*hanaDialer); ok {
			t.Error("Internet destination must not use the BTP dialer")
		}
	})
}

func TestHanaDialer_BadAddress(t *testing.T) {
	d := &hanaDialer{}
	if _, err := d.DialContext(context.Background(), "no-port", dial.DialerOptions{}); err == nil || !strings.HasPrefix(err.Error(), "hana dialer:") {
		t.Errorf("err = %v", err)
	}
}

func TestSplitStatements(t *testing.T) {
	got, err := splitStatements("-- header\nCREATE TABLE t (s NVARCHAR(5) DEFAULT 'a;b');\n\n;  \nDROP TABLE x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.Contains(got[0], "'a;b'") || got[1] != "DROP TABLE x" {
		t.Errorf("got %q", got)
	}
	if got, _ := splitStatements("  \n"); len(got) != 0 {
		t.Errorf("blank script yielded %q", got)
	}
}

func TestHANAConnector(t *testing.T) {
	ctx := context.Background()
	c, err := hanaConnector(ctx, config.TargetConfig{Host: "h", User: "u", Password: "p", SSLMode: "disable"}, nil)
	if err != nil || c == nil {
		t.Fatalf("connector=%v err=%v", c, err)
	}

	tests := []struct {
		name    string
		cfg     config.TargetConfig
		resolve DestinationResolver
		wantErr string
	}{
		{"destination without resolver", config.TargetConfig{Destination: "D"}, nil, "no BTP"},
		{"destination lookup fails", config.TargetConfig{Destination: "D"}, failingResolver, "hana: resolver boom"},
		{"no host", config.TargetConfig{User: "u"}, nil, "no host"},
		{"bad sslmode", config.TargetConfig{Host: "h", User: "u", SSLMode: "bogus"}, nil, "sslmode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := hanaConnector(ctx, tc.cfg, tc.resolve); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}

	// Open reports an unreachable server as a ping failure.
	_, err = Open(ctx, config.TargetConfig{DB: "hana", Host: "127.0.0.1", Port: 1, User: "u", SSLMode: "disable"}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "hana: ping") {
		t.Fatalf("err = %v", err)
	}
}
