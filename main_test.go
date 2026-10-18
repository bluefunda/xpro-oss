package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluefunda/xpro-oss/btp"
	"github.com/bluefunda/xpro-oss/config"
)

func TestBuildSourceFactory_Local(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.xml"), []byte("<a/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := buildSourceFactory(&config.AppConfig{Source: config.SourceConfig{Type: "local", Path: dir}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	src, closeFn, err := f(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if closeFn != nil {
		t.Error("a local directory needs no close function")
	}
	if data, err := fs.ReadFile(src, "a.xml"); err != nil || string(data) != "<a/>" {
		t.Errorf("read = %q, %v", data, err)
	}

	// Empty type defaults to local.
	if _, err := buildSourceFactory(&config.AppConfig{Source: config.SourceConfig{Path: dir}}, nil); err != nil {
		t.Errorf("empty type: %v", err)
	}
}

func TestBuildSourceFactory_Errors(t *testing.T) {
	tests := []struct {
		name    string
		src     config.SourceConfig
		btp     *btp.Bindings
		wantErr string
	}{
		{"local without path", config.SourceConfig{Type: "local"}, nil, "source.path is required"},
		{"unknown type", config.SourceConfig{Type: "s3"}, nil, `unsupported source.type "s3"`},
		{"sftp without host", config.SourceConfig{Type: "sftp"}, nil, "source.sftp.host is required"},
		{"sftp destination without BTP", config.SourceConfig{Type: "sftp", SFTP: config.SFTPConfig{Destination: "D"}}, nil, "no BTP"},
		{"unreadable key", config.SourceConfig{Type: "sftp", SFTP: config.SFTPConfig{Host: "h", PrivateKeyPath: "/nonexistent/key"}}, nil, "read privateKeyPath"},
		{"missing known_hosts", config.SourceConfig{Type: "sftp", SFTP: config.SFTPConfig{Host: "h", KnownHosts: "/nonexistent/known_hosts"}}, nil, "known_hosts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSourceFactory(&config.AppConfig{Source: tc.src}, tc.btp)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestSFTPBuilder_Direct(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id")
	knownHosts := filepath.Join(dir, "known_hosts")
	for p, content := range map[string]string{key: "KEYDATA", knownHosts: ""} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.AppConfig{Source: config.SourceConfig{
		Type: "sftp", Path: "/in",
		SFTP: config.SFTPConfig{Host: "h", User: "u", Password: "p", PrivateKeyPath: key, KnownHosts: knownHosts},
	}}
	build, err := sftpBuilder(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Path != "/in" || s.Dest.Host != "h" || s.Dest.Port != "22" || s.Dest.User != "u" || s.Dest.Properties["sshKey"] != "KEYDATA" || s.Dialer != nil {
		t.Errorf("sftp = %+v dest = %+v", s, s.Dest)
	}
	if s.HostKeyCallback == nil {
		t.Error("source.sftp.knownHosts must install a host key callback")
	}

	// Without knownHosts, verification stays off (and Connect warns).
	cfg.Source.SFTP.KnownHosts = ""
	build, _ = sftpBuilder(cfg, nil)
	if s, _ := build(context.Background()); s.HostKeyCallback != nil {
		t.Error("no knownHosts configured, yet a host key callback was installed")
	}
}

func TestSFTPFactory_ConnectError(t *testing.T) {
	cfg := &config.AppConfig{Source: config.SourceConfig{
		Type: "sftp", SFTP: config.SFTPConfig{Host: "127.0.0.1", Port: "1", User: "u", Password: "p"},
	}}
	f, err := buildSourceFactory(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f(context.Background()); err == nil {
		t.Error("connecting to a closed port should fail")
	}
}

func TestSFTPBuilder_DestinationResolveError(t *testing.T) {
	// A non-nil but unreachable binding set: Resolve fails, and the error surfaces from the factory.
	cfg := &config.AppConfig{Source: config.SourceConfig{Type: "sftp", SFTP: config.SFTPConfig{Destination: "D"}}}
	f, err := buildSourceFactory(cfg, &btp.Bindings{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f(context.Background()); err == nil {
		t.Error("an unresolvable destination should fail the run, not the process")
	}
}

func TestRun_StartupErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("no config", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := run(ctx); err == nil || !strings.HasPrefix(err.Error(), "config:") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bad mapping", func(t *testing.T) {
		t.Chdir(t.TempDir())
		writeFile(t, "config.yaml", "mapping:\n  - column: a\n    xpath: \"///[\"\n")
		if err := run(ctx); err == nil || !strings.HasPrefix(err.Error(), "mapping:") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("database unreachable", func(t *testing.T) {
		t.Chdir(t.TempDir())
		writeFile(t, "config.yaml", "target:\n  host: 127.0.0.1\n  port: 1\n  user: u\n  database: d\n  sslmode: disable\n")
		if err := run(ctx); err == nil || !strings.HasPrefix(err.Error(), "target db:") {
			t.Errorf("err = %v", err)
		}
	})
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
