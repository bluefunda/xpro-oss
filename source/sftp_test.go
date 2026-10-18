package source

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluefunda/btp-go/destination"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// startSFTPServer runs an in-process SSH+SFTP server rooted at the real
// filesystem (tests use absolute t.TempDir paths) and returns a destination
// pointing at it.
func startSFTPServer(t *testing.T) *destination.Destination {
	dest, _ := startSFTPServerWithKey(t)
	return dest
}

// startSFTPServerWithKey is startSFTPServer, also returning the server's host key.
func startSFTPServerWithKey(t *testing.T) (*destination.Destination, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == "u" && string(pw) == "p" {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSFTPConn(conn, cfg)
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	return &destination.Destination{Host: host, Port: port, User: "u", Password: "p"}, signer.PublicKey()
}

func serveSFTPConn(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for r := range chReqs {
				_ = r.Reply(r.Type == "subsystem", nil)
			}
		}()
		go func() {
			srv, err := sftp.NewServer(ch)
			if err != nil {
				return
			}
			_ = srv.Serve()
			_ = srv.Close()
		}()
	}
}

func TestSFTP_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.xml"), []byte("<a/>"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "b.xml"), []byte("<b/>"), 0o600)
	_ = os.Mkdir(filepath.Join(dir, "sub"), 0o700)

	s := &SFTP{Dest: startSFTPServer(t), Path: dir}
	if err := s.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	entries, err := fs.ReadDir(s, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 3 {
		t.Errorf("entries = %v, want a.xml b.xml sub", names)
	}

	got, err := fs.ReadFile(s, "a.xml")
	if err != nil || string(got) != "<a/>" {
		t.Errorf("ReadFile = %q, %v", got, err)
	}
	if _, err := fs.ReadFile(s, "missing.xml"); err == nil {
		t.Error("missing file should error")
	}
	if _, err := s.ReadDir("nope"); err == nil {
		t.Error("missing dir should error")
	}
	if _, err := s.Open("../escape"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("path traversal err = %v, want ErrInvalid", err)
	}
}

func TestSFTP_ConnectFailures(t *testing.T) {
	ctx := context.Background()

	bad := startSFTPServer(t)
	bad.Password = "wrong"
	if err := (&SFTP{Dest: bad}).Connect(ctx); err == nil {
		t.Error("wrong password should fail the handshake")
	}

	closed := &destination.Destination{Host: "127.0.0.1", Port: "1", User: "u", Password: "p"}
	if err := (&SFTP{Dest: closed}).Connect(ctx); err == nil {
		t.Error("closed port should fail to dial")
	}

	if err := (&SFTP{Dest: &destination.Destination{Host: "h", Port: "22"}}).Connect(ctx); err == nil {
		t.Error("destination without auth should fail")
	}
}

func TestSFTP_NotConnected(t *testing.T) {
	s := &SFTP{Path: "/x"}
	if _, err := s.Open("a"); err == nil {
		t.Error("Open before Connect should fail")
	}
	if _, err := s.ReadDir("."); err == nil {
		t.Error("ReadDir before Connect should fail")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close on unconnected = %v", err)
	}
}

func TestSFTP_Resolve(t *testing.T) {
	tests := []struct {
		path, name, want string
		wantErr          bool
	}{
		{"", "a.xml", "a.xml", false},
		{"", ".", ".", false},
		{"/in", ".", "/in", false},
		{"/in", "a.xml", "/in/a.xml", false},
		{"/in", "../a", "", true},
		{"/in", "/abs", "", true},
	}
	for _, tc := range tests {
		got, err := (&SFTP{Path: tc.path}).resolve(tc.name)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("resolve(%q,%q) = %q, %v; want %q (err=%v)", tc.path, tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

// writeKnownHosts writes a known_hosts file trusting key for host:port.
func writeKnownHosts(t *testing.T, d *destination.Destination, key ssh.PublicKey) string {
	t.Helper()
	line := fmt.Sprintf("[%s]:%s %s\n", d.Host, d.Port, ssh.MarshalAuthorizedKey(key))
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSFTP_HostKeyVerification(t *testing.T) {
	ctx := context.Background()
	dest, key := startSFTPServerWithKey(t)

	// A known_hosts entry for the server's real key lets the connection through.
	good, err := KnownHostsCallback(writeKnownHosts(t, dest, key))
	if err != nil {
		t.Fatal(err)
	}
	s := &SFTP{Dest: dest, HostKeyCallback: good}
	if err := s.Connect(ctx); err != nil {
		t.Fatalf("trusted host key rejected: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}

	// A known_hosts entry for a different key must refuse the connection.
	_, otherKey := startSFTPServerWithKey(t)
	bad, err := KnownHostsCallback(writeKnownHosts(t, dest, otherKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := (&SFTP{Dest: dest, HostKeyCallback: bad}).Connect(ctx); err == nil {
		t.Error("mismatched host key must fail the handshake")
	}
}

func TestKnownHostsCallback_MissingFile(t *testing.T) {
	if _, err := KnownHostsCallback(filepath.Join(t.TempDir(), "nope")); err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("err = %v", err)
	}
}

func TestSFTP_CloseTwice(t *testing.T) {
	s := &SFTP{Dest: startSFTPServer(t)}
	if err := s.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("first Close = %v", err)
	}
	// Tearing down an already-closed session is not an error worth reporting.
	if err := s.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Logf("second Close = %v (tolerated)", err)
	}
}
