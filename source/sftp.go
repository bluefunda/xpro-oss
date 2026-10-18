package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"time"

	"github.com/bluefunda/btp-go/connectivity"
	"github.com/bluefunda/btp-go/destination"
	"github.com/bluefunda/btp-go/sshclient"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SFTP is an fs.FS backed by one SFTP session, rooted at Path. When Dialer
// is non-nil, connections tunnel through the SAP BTP Connectivity SOCKS5
// proxy via btp-go/sshclient — the same on-prem pattern used by BlueFunda's BTP
// services for SAP access. When Dialer is nil, xpro-oss dials directly, reusing btp-go's
// own auth-config logic (sshclient.SSHConfigFromDestination is
// dialer-independent), so the exact same code path works unmodified on AWS,
// GCP, or bare metal — no BTP dependency required.
//
// The creator of an SFTP calls Connect once before use and Close when done;
// readers (such as process.Pipeline) just use it as an fs.FS.
// *sftp.Client is safe for concurrent use, so one session is shared across
// all concurrent batch goroutines within a run.
type SFTP struct {
	Dest      *destination.Destination
	Dialer    *connectivity.Dialer // nil = direct connection, no BTP
	Path      string
	RetryOpts sshclient.RetryOpts

	// HostKeyCallback verifies the server's host key. When nil, host keys are
	// not verified (a warning is logged on Connect) — build one with
	// KnownHostsCallback for any untrusted network.
	HostKeyCallback ssh.HostKeyCallback

	client  *sftp.Client
	sshConn *ssh.Client
}

// Connect establishes the SSH+SFTP session. Must be called before Open/ReadDir.
func (s *SFTP) Connect(ctx context.Context) error {
	sshClient, err := s.dial(ctx)
	if err != nil {
		return err
	}
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return fmt.Errorf("sftp: %w", err)
	}
	s.sshConn, s.client = sshClient, client
	return nil
}

// Close tears down the SFTP and SSH connections, returning any close errors.
func (s *SFTP) Close() error {
	var errs []error
	if s.client != nil {
		errs = append(errs, s.client.Close())
	}
	if s.sshConn != nil {
		// Closing the sftp client already closes the SSH connection, so an
		// already-closed error here is expected, not a failure.
		if err := s.sshConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// KnownHostsCallback returns a host-key callback that verifies servers
// against an OpenSSH known_hosts file.
func KnownHostsCallback(path string) (ssh.HostKeyCallback, error) {
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("sftp: known_hosts %s: %w", path, err)
	}
	return cb, nil
}

func (s *SFTP) dial(ctx context.Context) (*ssh.Client, error) {
	if s.HostKeyCallback == nil {
		slog.Warn("sftp: host key verification is disabled; set source.sftp.knownHosts", "host", s.Dest.Host)
	}
	if s.Dialer != nil {
		return sshclient.Dial(ctx, sshclient.Config{Dialer: s.Dialer, RetryOpts: s.RetryOpts, HostKeyCallback: s.HostKeyCallback}, s.Dest)
	}

	sshCfg, err := sshclient.SSHConfigFromDestination(s.Dest)
	if err != nil {
		return nil, err
	}
	if s.HostKeyCallback != nil {
		sshCfg.HostKeyCallback = s.HostKeyCallback
	}
	hostPort := net.JoinHostPort(s.Dest.Host, s.Dest.Port)
	d := &net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", hostPort, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, hostPort, sshCfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

func (s *SFTP) resolve(name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if s.Path == "" || name == "." {
		if s.Path == "" {
			return name, nil
		}
		return s.Path, nil
	}
	return s.Path + "/" + name, nil
}

// Open implements fs.FS, opening the named file under the configured Path.
func (s *SFTP) Open(name string) (fs.File, error) {
	if s.client == nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("sftp: source not connected")}
	}
	path, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	return s.client.Open(path)
}

// ReadDir implements fs.ReadDirFS for the configured Path — a fast path so
// fs.ReadDir doesn't fall back to Open+ReadDir(-1)+Close.
func (s *SFTP) ReadDir(name string) ([]fs.DirEntry, error) {
	if s.client == nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("sftp: source not connected")}
	}
	path, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	infos, err := s.client.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]fs.DirEntry, len(infos))
	for i, info := range infos {
		entries[i] = fs.FileInfoToDirEntry(info)
	}
	return entries, nil
}
