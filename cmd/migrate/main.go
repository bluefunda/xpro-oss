// Command migrate applies a raw SQL file (e.g. db/migrations/001_init.sql)
// against the target database configured in config.yaml.
//
// Usage:
//
//	go run ./cmd/migrate db/migrations/001_init.sql
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/bluefunda/xpro-oss/btp"
	"github.com/bluefunda/xpro-oss/config"
	xdb "github.com/bluefunda/xpro-oss/db"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable body: it returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		_, _ = fmt.Fprintln(stderr, "usage: migrate <path/to/schema.sql>")
		return 1
	}

	sqlBytes, err := os.ReadFile(args[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "read:", err)
		return 1
	}

	cfg, err := config.Load("")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "config:", err)
		return 1
	}

	var resolve xdb.DestinationResolver
	if b := btp.Detect(); b != nil {
		resolve = b.Resolve
	}
	target, err := xdb.Open(ctx, cfg.Target, resolve)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "db:", err)
		return 1
	}
	defer func() { _ = target.Close() }()

	if err := target.Exec(ctx, string(sqlBytes)); err != nil {
		_, _ = fmt.Fprintln(stderr, "exec:", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "migration applied")
	return 0
}
