// Package db provides the target-database layer: one concrete Inserter that
// writes batches with per-row failure isolation, configured by a small
// Dialect (placeholder style, batch size, savepoint support) and a
// database/sql driver.Connector supplied per vendor (postgres.go, oracle.go,
// hana.go) — the same split database/sql itself uses between sql.DB and its
// drivers. Columns and table are entirely config-driven (see
// config.TargetConfig and config.MappingField) — there is no hardcoded
// schema anywhere here.
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"syscall"

	"github.com/bluefunda/btp-go/connectivity"
	"github.com/bluefunda/btp-go/destination"

	"github.com/bluefunda/xpro-oss/config"
	"github.com/bluefunda/xpro-oss/model"
)

// DestinationResolver looks up a named SAP BTP destination and returns it
// with a Connectivity dialer for on-premise tunneling. (*btp.Bindings).Resolve
// satisfies it; a nil resolver means "no BTP".
type DestinationResolver func(ctx context.Context, name string) (*destination.Destination, *connectivity.Dialer, error)

// Dialect captures everything that differs between supported databases
// beyond how to connect to them.
type Dialect struct {
	// Name prefixes error messages ("postgres", "oracle", "hana").
	Name string
	// Placeholder returns the i-th (1-based) bind placeholder.
	Placeholder func(i int) string
	// Savepoints makes InsertBatch wrap each row in SAVEPOINT / ROLLBACK TO
	// SAVEPOINT. Leave false for databases that roll a failed statement back
	// on their own and keep the transaction usable (SAP HANA, which has no
	// SAVEPOINT statement at all).
	Savepoints bool
	// BatchSize is the recommended rows per transaction.
	BatchSize int
	// SplitScript, when set, splits a script into the individual statements
	// Exec runs one by one (for databases that accept one statement per
	// request). Nil means the script is sent whole.
	SplitScript func(script string) ([]string, error)
}

// The supported dialects.
var (
	// Postgres: $n placeholders, savepoint isolation.
	Postgres = Dialect{Name: "postgres", Placeholder: func(i int) string { return "$" + strconv.Itoa(i) }, Savepoints: true, BatchSize: 500}
	// Oracle: :n placeholders, savepoint isolation. 150 rows keeps bind
	// counts under Oracle's ~4,000 ceiling for typical column counts (e.g.
	// 150 rows x 24 cols = 3,600); tune target.batchSize for wider mappings.
	Oracle = Dialect{Name: "oracle", Placeholder: func(i int) string { return ":" + strconv.Itoa(i) }, Savepoints: true, BatchSize: 150}
	// HANA: ? placeholders, statement-level rollback (no savepoints), one
	// statement per request.
	HANA = Dialect{Name: "hana", Placeholder: func(int) string { return "?" }, BatchSize: 500, SplitScript: splitStatements}
)

// BatchResult summarizes one InsertBatch call.
type BatchResult struct {
	Success int
	Failed  int
	Errors  []error
}

// ErrCommitUncertain marks a failed transaction commit. The server may or may
// not have applied it, so a blind retry could insert the batch twice.
var ErrCommitUncertain = errors.New("commit outcome unknown")

// Retryable reports whether err from InsertBatch is a transient,
// connection-level failure that is safe to retry once: a dropped or refused
// connection, EOF, or a network timeout. Server-side errors (missing table,
// bad SQL), context cancellation and uncertain commits are not retryable.
func Retryable(err error) bool {
	switch {
	case err == nil,
		errors.Is(err, ErrCommitUncertain),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, driver.ErrBadConn),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.EPIPE):
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// Inserter writes record batches to one table. Create it with Open (or New
// around an existing *sql.DB); it is safe for concurrent use.
type Inserter struct {
	db      *sql.DB
	target  config.TargetConfig
	dialect Dialect
}

// New returns an Inserter writing to sqlDB with the given dialect. It takes
// ownership of sqlDB: Close closes it.
func New(sqlDB *sql.DB, target config.TargetConfig, d Dialect) *Inserter {
	return &Inserter{db: sqlDB, target: target, dialect: d}
}

// Open connects to the database selected by target.DB ("postgres" is the
// default; "oracle" and "hana" are also always available — all drivers are
// pure Go, no CGO required), verifies the connection with a ping, and returns
// a ready Inserter. resolve is used only when target.Destination is set and
// may be nil.
func Open(ctx context.Context, target config.TargetConfig, resolve DestinationResolver) (*Inserter, error) {
	var (
		conn driver.Connector
		d    Dialect
		err  error
	)
	switch target.DB {
	case "", "postgres", "postgresql":
		d = Postgres
		conn, err = postgresConnector(target)
	case "oracle":
		d = Oracle
		conn, err = oracleConnector(ctx, target, resolve)
	case "hana":
		d = HANA
		conn, err = hanaConnector(ctx, target, resolve)
	default:
		return nil, fmt.Errorf("db: unsupported target.db %q", target.DB)
	}
	if err != nil {
		return nil, err
	}

	sqlDB := sql.OpenDB(conn)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("%s: ping: %w", d.Name, err)
	}
	return New(sqlDB, target, d), nil
}

// DefaultBatchSize returns the dialect's recommended batch size.
func (i *Inserter) DefaultBatchSize() int { return i.dialect.BatchSize }

// Ping verifies the database connection is still alive.
func (i *Inserter) Ping(ctx context.Context) error { return i.db.PingContext(ctx) }

// Close releases the connection pool.
func (i *Inserter) Close() error { return i.db.Close() }

// Exec runs a SQL script — used by cmd/migrate and cmd/bench. Dialects with a
// SplitScript get it split into statements, executed in order.
func (i *Inserter) Exec(ctx context.Context, script string) error {
	stmts := []string{script}
	if i.dialect.SplitScript != nil {
		var err error
		if stmts, err = i.dialect.SplitScript(script); err != nil {
			return err
		}
	}
	for _, stmt := range stmts {
		if _, err := i.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: exec: %w", i.dialect.Name, err)
		}
	}
	return nil
}

// InsertBatch inserts records with per-row failure isolation: one
// transaction, one prepared statement, and a failing row is recorded in the
// result without sinking the rest of the batch. Where the dialect has
// savepoints each row is wrapped in SAVEPOINT / ROLLBACK TO SAVEPOINT;
// otherwise the database's own statement-level rollback does the isolating.
// A transaction-level failure (begin, prepare, savepoint, commit) returns an
// error with every record counted as failed; see Retryable.
func (i *Inserter) InsertBatch(ctx context.Context, records []model.Record) (BatchResult, error) {
	d := i.dialect
	if len(records) == 0 {
		return BatchResult{}, nil
	}

	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchResult{Failed: len(records)}, fmt.Errorf("%s: begin: %w", d.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, insertSQL(qualifyTable(i.target), records[0].ColumnNames(), d.Placeholder))
	if err != nil {
		return BatchResult{Failed: len(records)}, fmt.Errorf("%s: prepare: %w", d.Name, err)
	}
	defer func() { _ = stmt.Close() }()

	result := BatchResult{}
	for n, rec := range records {
		sp := "sp_" + strconv.Itoa(n)
		if d.Savepoints {
			if _, err := tx.ExecContext(ctx, "SAVEPOINT "+sp); err != nil {
				return BatchResult{Failed: len(records)}, fmt.Errorf("%s: savepoint: %w", d.Name, err)
			}
		}
		if _, err := stmt.ExecContext(ctx, rec.Values()...); err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Errorf("%s: %w", rec.SourceName, err))
			if d.Savepoints {
				if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+sp); rbErr != nil {
					return BatchResult{Failed: len(records)}, fmt.Errorf("%s: rollback to savepoint: %w", d.Name, rbErr)
				}
			}
			continue
		}
		result.Success++
	}

	if err := tx.Commit(); err != nil {
		return BatchResult{Failed: len(records)}, fmt.Errorf("%s: commit: %w: %w", d.Name, ErrCommitUncertain, err)
	}
	return result, nil
}

// insertSQL builds a generic "INSERT INTO table (cols...) VALUES (phs...)"
// statement from a config-driven column list and a driver-specific
// placeholder style ("$%d" for Postgres, ":%d" for Oracle, "?" for HANA).
func insertSQL(table string, columns []string, placeholder func(i int) string) string {
	phs := make([]string, len(columns))
	for i := range columns {
		phs[i] = placeholder(i + 1)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(columns, ", "), strings.Join(phs, ", "))
}

// qualifyTable returns the fully-qualified table name for an INSERT
// statement: target.Table as-is if it's already schema-qualified (contains
// a dot), "schema.table" when target.Schema is set, otherwise just the bare
// table name. Every dialect shares it.
func qualifyTable(target config.TargetConfig) string {
	if strings.IndexByte(target.Table, '.') >= 0 {
		return target.Table
	}
	if target.Schema == "" {
		return target.Table
	}
	return target.Schema + "." + target.Table
}

// endpointFromDestination extracts host and port from a destination. HTTP-
// typed destinations carry them in URL (e.g. "http://DBHOST:1750") rather
// than in Host/Port directly; an explicit Port always wins over the URL's.
// A zero port means "not specified" — callers apply their driver default.
func endpointFromDestination(dest *destination.Destination) (host string, port int) {
	host = dest.Host
	port, _ = strconv.Atoi(dest.Port)
	if host == "" && dest.URL != "" {
		if u, err := url.Parse(dest.URL); err == nil {
			host = u.Hostname()
			if p := u.Port(); p != "" && port == 0 {
				port, _ = strconv.Atoi(p)
			}
		}
	}
	return host, port
}

// directDestination builds a destination from the direct host/port/user/
// password fields, used when no BTP destination name is configured.
func directDestination(t config.TargetConfig) *destination.Destination {
	return &destination.Destination{
		Host:     t.Host,
		Port:     strconv.Itoa(t.Port),
		User:     t.User,
		Password: t.Password,
	}
}

// resolveDestination returns the destination a driver should connect to:
// the named BTP destination (plus its Connectivity dialer) when
// target.destination is set, otherwise one built from the direct fields.
func resolveDestination(ctx context.Context, t config.TargetConfig, resolve DestinationResolver, driver string) (*destination.Destination, *connectivity.Dialer, error) {
	if t.Destination == "" {
		return directDestination(t), nil, nil
	}
	if resolve == nil {
		return nil, nil, fmt.Errorf("%s: target.destination %q is configured but no BTP Connectivity/Destination binding was detected in this environment", driver, t.Destination)
	}
	d, dl, err := resolve(ctx, t.Destination)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", driver, err)
	}
	return d, dl, nil
}

// firstNonEmpty returns the first non-empty string in vals, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// btpDial dials host:address through the BTP Connectivity SOCKS5 proxy.
// Shared by every driver-specific dialer adapter.
func btpDial(ctx context.Context, d *connectivity.Dialer, locationID, driver, address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%s dialer: parse address %q: %w", driver, address, err)
	}
	portNum, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("%s dialer: parse port %q: %w", driver, portStr, err)
	}
	return d.Dial(ctx, host, uint16(portNum), locationID)
}
