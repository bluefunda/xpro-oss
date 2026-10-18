package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeDB is an in-memory database/sql driver used to exercise the shared
// insert path without a real database. It logs every statement it sees and
// can be told to fail at specific points.
type fakeDB struct {
	mu  sync.Mutex
	log []string

	failBegin     bool
	failPrepare   bool
	failCommit    bool
	failSavepoint bool
	failRollback  bool   // fail "ROLLBACK TO SAVEPOINT"
	badValue      string // an INSERT whose args contain this value fails
	failExec      bool   // fail every direct Exec (used for Exec tests)
}

func (f *fakeDB) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, s)
}

func (f *fakeDB) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// open returns a *sql.DB backed by f.
func (f *fakeDB) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(&fakeConnector{f})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fakeConnector struct{ f *fakeDB }

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{c.f}, nil }
func (c *fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use OpenDB") }

type fakeConn struct{ f *fakeDB }

func (c *fakeConn) Prepare(q string) (driver.Stmt, error) {
	if c.f.failPrepare {
		return nil, errors.New("prepare boom")
	}
	return &fakeStmt{c.f, q}, nil
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	if c.f.failBegin {
		return nil, errors.New("begin boom")
	}
	c.f.record("BEGIN")
	return &fakeTx{c.f}, nil
}

// ExecContext handles direct (unprepared) statements: SAVEPOINTs and Exec.
func (c *fakeConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.f.record(q)
	switch {
	case strings.HasPrefix(q, "SAVEPOINT") && c.f.failSavepoint:
		return nil, errors.New("savepoint boom")
	case strings.HasPrefix(q, "ROLLBACK TO SAVEPOINT") && c.f.failRollback:
		return nil, errors.New("rollback boom")
	case c.f.failExec:
		return nil, errors.New("exec boom")
	}
	return driver.RowsAffected(1), nil
}

type fakeStmt struct {
	f *fakeDB
	q string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.f.record(fmt.Sprintf("%s %v", s.q, args))
	for _, a := range args {
		if s.f.badValue != "" && a == s.f.badValue {
			return nil, errors.New("constraint violated")
		}
	}
	return driver.RowsAffected(1), nil
}
func (s *fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("not supported")
}

type fakeTx struct{ f *fakeDB }

func (t *fakeTx) Commit() error {
	if t.f.failCommit {
		return errors.New("commit boom")
	}
	t.f.record("COMMIT")
	return nil
}
func (t *fakeTx) Rollback() error { t.f.record("ROLLBACK"); return nil }
