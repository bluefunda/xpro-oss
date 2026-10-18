// Command xpro-oss is a generic, config-driven XML ingestion daemon: it
// reads files from a source (a local directory, or SFTP — optionally
// tunneled through SAP BTP Connectivity), maps configured XPath expressions
// onto target columns, and batch-inserts the result into a target database
// (PostgreSQL, Oracle or SAP HANA — all pure Go, no CGO required). Oracle and
// HANA targets can also be resolved via a named SAP BTP destination instead
// of direct host/port/user/password config.
//
// xpro-oss has no hardcoded knowledge of any XML schema, source location, or
// SAP concept — everything is driven by config.yaml. BTP integration is
// entirely optional: without it, xpro-oss runs unmodified on AWS, GCP, or
// bare metal.
//
// Endpoints:
//
//	GET  /healthz  — liveness probe
//	POST /process  — run one source → target pass
//	GET  /status   — result of the most recent /process run
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bluefunda/btp-go/destination"
	"golang.org/x/crypto/ssh"

	"github.com/bluefunda/xpro-oss/btp"
	"github.com/bluefunda/xpro-oss/config"
	xdb "github.com/bluefunda/xpro-oss/db"
	"github.com/bluefunda/xpro-oss/handler"
	"github.com/bluefunda/xpro-oss/parser"
	"github.com/bluefunda/xpro-oss/source"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("xpro-oss", "err", err)
		stop()
		os.Exit(1)
	}
}

// run wires everything up and serves until ctx is cancelled (SIGINT/SIGTERM),
// then shuts down gracefully: stop accepting requests, interrupt any in-flight
// /process run, and wait for it to wind down. It returns instead of calling
// os.Exit so deferred cleanup (closing the database) always runs.
func run(ctx context.Context) error {
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	slog.Info("config loaded", "sourceType", cfg.Source.Type, "targetDB", cfg.Target.DB, "mappings", len(cfg.Mapping))

	mapping, err := parser.Compile(cfg.Mapping)
	if err != nil {
		return fmt.Errorf("mapping: %w", err)
	}

	bindings := btp.Detect() // nil outside Cloud Foundry/Kyma; Resolve then reports ErrNoBinding
	resolve := xdb.DestinationResolver(nil)
	if bindings != nil {
		resolve = bindings.Resolve
	}

	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	target, err := xdb.Open(dbCtx, cfg.Target, resolve)
	cancel()
	if err != nil {
		return fmt.Errorf("target db: %w", err)
	}
	defer func() { _ = target.Close() }()
	slog.Info("target db connected", "db", cfg.Target.DB, "table", cfg.Target.Table)

	newSource, err := buildSourceFactory(cfg, bindings)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}

	runCtx, cancelRuns := context.WithCancel(context.Background())
	defer cancelRuns()
	h := handler.New(runCtx, handler.Options{BatchSize: cfg.Target.BatchSize, Workers: cfg.Workers}, mapping, target, newSource)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// No WriteTimeout: POST /process?wait=true legitimately holds a response
	// open for the length of a run.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http shutdown", "err", err)
	}
	cancelRuns() // interrupt an in-flight run; it reports the partial result
	if err := h.Wait(shutdownCtx); err != nil {
		slog.Error("waiting for in-flight run", "err", err)
	}
	return nil
}

// buildSourceFactory returns a function that opens a fresh source for each
// /process run, based on cfg.Source.Type. bindings may be nil.
func buildSourceFactory(cfg *config.AppConfig, bindings *btp.Bindings) (handler.SourceFactory, error) {
	switch cfg.Source.Type {
	case "", "local":
		if cfg.Source.Path == "" {
			return nil, fmt.Errorf("source.path is required when source.type is local")
		}
		return func(context.Context) (fs.FS, func() error, error) {
			return os.DirFS(cfg.Source.Path), nil, nil
		}, nil

	case "sftp":
		return buildSFTPSourceFactory(cfg, bindings)

	default:
		return nil, fmt.Errorf("unsupported source.type %q", cfg.Source.Type)
	}
}

// buildSFTPSourceFactory wires either the BTP-tunneled path (destination
// name resolved via the BTP Destination Service, dialed through the
// Connectivity SOCKS5 proxy) or the direct path (plain host/port/user
// credentials, no BTP involved at all). Each run connects its own session and
// closes it when the run ends.
func buildSFTPSourceFactory(cfg *config.AppConfig, bindings *btp.Bindings) (handler.SourceFactory, error) {
	build, err := sftpBuilder(cfg, bindings)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (fs.FS, func() error, error) {
		s, err := build(ctx)
		if err != nil {
			return nil, nil, err
		}
		if err := s.Connect(ctx); err != nil {
			return nil, nil, err
		}
		return s, s.Close, nil
	}, nil
}

// sftpBuilder validates the SFTP configuration up front and returns a
// function that assembles an (unconnected) source.SFTP for one run.
func sftpBuilder(cfg *config.AppConfig, bindings *btp.Bindings) (func(context.Context) (*source.SFTP, error), error) {
	sc := cfg.Source.SFTP

	var hostKey ssh.HostKeyCallback
	if sc.KnownHosts != "" {
		cb, err := source.KnownHostsCallback(sc.KnownHosts)
		if err != nil {
			return nil, err
		}
		hostKey = cb
	}

	if sc.Destination != "" {
		if bindings == nil {
			return nil, fmt.Errorf("source.sftp.destination %q is configured but no BTP Connectivity/Destination binding was detected in this environment", sc.Destination)
		}
		return func(ctx context.Context) (*source.SFTP, error) {
			dest, dialer, err := bindings.Resolve(ctx, sc.Destination)
			if err != nil {
				return nil, err
			}
			path := cfg.Source.Path
			if path == "" {
				path = dest.Path
			}
			return &source.SFTP{Dest: dest, Dialer: dialer, Path: path, HostKeyCallback: hostKey}, nil
		}, nil
	}

	if sc.Host == "" {
		return nil, fmt.Errorf("source.sftp.host is required when source.sftp.destination is not set")
	}
	dest := &destination.Destination{
		Host:     sc.Host,
		Port:     sc.Port,
		User:     sc.User,
		Password: sc.Password,
	}
	if dest.Port == "" {
		dest.Port = "22"
	}
	if sc.PrivateKeyPath != "" {
		key, err := os.ReadFile(sc.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read privateKeyPath: %w", err)
		}
		dest.Properties = map[string]string{"sshKey": string(key)}
	}
	return func(context.Context) (*source.SFTP, error) {
		return &source.SFTP{Dest: dest, Path: cfg.Source.Path, HostKeyCallback: hostKey}, nil
	}, nil
}
