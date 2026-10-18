package db

import (
	"context"
	"strings"
	"testing"

	"github.com/bluefunda/xpro-oss/config"
)

func TestOracleConnector_Errors(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		cfg     config.TargetConfig
		resolve DestinationResolver
		wantErr string
	}{
		{"destination without resolver", config.TargetConfig{Destination: "D"}, nil, "no BTP"},
		{"destination lookup fails", config.TargetConfig{Destination: "D"}, failingResolver, "oracle: resolver boom"},
		{"no host", config.TargetConfig{User: "u", Database: "svc"}, nil, "no host"},
		{"no user", config.TargetConfig{Host: "h", Database: "svc"}, nil, "no user"},
		{"no service", config.TargetConfig{Host: "h", User: "u"}, nil, "no service_name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := oracleConnector(ctx, tc.cfg, tc.resolve)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestOracleConnector_Direct(t *testing.T) {
	c, err := oracleConnector(context.Background(), config.TargetConfig{Host: "h", User: "u", Password: "p", Database: "svc"}, nil)
	if err != nil || c == nil {
		t.Fatalf("connector=%v err=%v", c, err)
	}
	// And Open reports an unreachable server as a ping failure.
	_, err = Open(context.Background(), config.TargetConfig{DB: "oracle", Host: "127.0.0.1", Port: 1, User: "u", Database: "svc"}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "oracle: ping") {
		t.Fatalf("err = %v", err)
	}
}

func TestBtpDialer_BadAddress(t *testing.T) {
	if _, err := (&btpDialer{}).DialContext(context.Background(), "tcp", "no-port"); err == nil || !strings.HasPrefix(err.Error(), "oracle dialer:") {
		t.Errorf("err = %v", err)
	}
}
