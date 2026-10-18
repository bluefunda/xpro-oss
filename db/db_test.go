package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/bluefunda/btp-go/connectivity"
	"github.com/bluefunda/btp-go/destination"

	"github.com/bluefunda/xpro-oss/config"
	"github.com/bluefunda/xpro-oss/model"
)

func rec(name string, vals ...string) model.Record {
	cols := make([]model.Column, len(vals))
	for i, v := range vals {
		cols[i] = model.Column{Name: string(rune('a' + i)), Value: v}
	}
	return model.Record{SourceName: name, Columns: cols}
}

func TestInsertSQL(t *testing.T) {
	got := insertSQL("t", []string{"a", "b"}, Postgres.Placeholder)
	if want := "INSERT INTO t (a, b) VALUES ($1, $2)"; got != want {
		t.Errorf("insertSQL = %q, want %q", got, want)
	}
	if got := insertSQL("t", []string{"a", "b"}, HANA.Placeholder); got != "INSERT INTO t (a, b) VALUES (?, ?)" {
		t.Errorf("hana insertSQL = %q", got)
	}
	if got := insertSQL("t", []string{"a"}, Oracle.Placeholder); got != "INSERT INTO t (a) VALUES (:1)" {
		t.Errorf("oracle insertSQL = %q", got)
	}
}

func TestDialects(t *testing.T) {
	tests := []struct {
		d          Dialect
		batch      int
		savepoints bool
		split      bool
	}{
		{Postgres, 500, true, false},
		{Oracle, 150, true, false},
		{HANA, 500, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.d.Name, func(t *testing.T) {
			in := New(nil, config.TargetConfig{}, tc.d)
			if in.DefaultBatchSize() != tc.batch || tc.d.Savepoints != tc.savepoints || (tc.d.SplitScript != nil) != tc.split {
				t.Errorf("dialect = %+v", tc.d)
			}
		})
	}
}

func TestQualifyTable(t *testing.T) {
	tests := []struct {
		name string
		in   config.TargetConfig
		want string
	}{
		{"bare", config.TargetConfig{Table: "t"}, "t"},
		{"schema", config.TargetConfig{Table: "t", Schema: "s"}, "s.t"},
		{"already qualified wins", config.TargetConfig{Table: "x.t", Schema: "s"}, "x.t"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := qualifyTable(tc.in); got != tc.want {
				t.Errorf("qualifyTable = %q, want %q", got, tc.want)
			}
		})
	}
}

func newInserter(t *testing.T, f *fakeDB, d Dialect) *Inserter {
	t.Helper()
	return New(f.open(t), config.TargetConfig{Table: "t"}, d)
}

func TestInsertBatch_Empty(t *testing.T) {
	f := &fakeDB{}
	res, err := newInserter(t, f, Postgres).InsertBatch(context.Background(), nil)
	if err != nil || res.Success != 0 || res.Failed != 0 || res.Errors != nil {
		t.Fatalf("got %+v, %v; want zero result, nil", res, err)
	}
	if len(f.lines()) != 0 {
		t.Errorf("empty batch touched the database: %v", f.lines())
	}
}

func TestInsertBatch_IsolatesBadRow_Savepoints(t *testing.T) {
	f := &fakeDB{badValue: "bad"}
	recs := []model.Record{rec("one.xml", "1"), rec("two.xml", "bad"), rec("three.xml", "3")}

	res, err := newInserter(t, f, Postgres).InsertBatch(context.Background(), recs)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Success != 2 || res.Failed != 1 || len(res.Errors) != 1 {
		t.Fatalf("result = %+v, want 2 ok / 1 failed", res)
	}
	if !strings.Contains(res.Errors[0].Error(), "two.xml") {
		t.Errorf("error %q should name the failing source file", res.Errors[0])
	}

	log := strings.Join(f.lines(), "\n")
	for _, want := range []string{"SAVEPOINT sp_0", "SAVEPOINT sp_1", "ROLLBACK TO SAVEPOINT sp_1", "SAVEPOINT sp_2", "COMMIT", "INSERT INTO t (a) VALUES ($1)"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "ROLLBACK TO SAVEPOINT sp_0") {
		t.Error("rolled back a row that succeeded")
	}
}

func TestInsertBatch_IsolatesBadRow_NoSavepoints(t *testing.T) {
	f := &fakeDB{badValue: "bad"}
	recs := []model.Record{rec("one.xml", "1"), rec("two.xml", "bad"), rec("three.xml", "3")}

	res, err := newInserter(t, f, HANA).InsertBatch(context.Background(), recs)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Success != 2 || res.Failed != 1 {
		t.Fatalf("result = %+v, want 2 ok / 1 failed", res)
	}
	for _, l := range f.lines() {
		if strings.Contains(l, "SAVEPOINT") {
			t.Fatalf("dialect without savepoints but saw %q", l)
		}
	}
	if got := f.lines()[len(f.lines())-1]; got != "COMMIT" {
		t.Errorf("last statement = %q, want COMMIT", got)
	}
}

func TestInsertBatch_TransactionFailures(t *testing.T) {
	tests := []struct {
		name    string
		f       *fakeDB
		wantErr string
	}{
		{"begin", &fakeDB{failBegin: true}, "begin"},
		{"prepare", &fakeDB{failPrepare: true}, "prepare"},
		{"savepoint", &fakeDB{failSavepoint: true}, "savepoint"},
		{"commit", &fakeDB{failCommit: true}, "commit"},
		{"rollback to savepoint", &fakeDB{badValue: "bad", failRollback: true}, "rollback to savepoint"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recs := []model.Record{rec("one.xml", "bad"), rec("two.xml", "2")}
			res, err := newInserter(t, tc.f, Postgres).InsertBatch(context.Background(), recs)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			// Whole-batch failure must report every record failed so the
			// pipeline can decide whether to retry.
			if res.Success != 0 || res.Failed != len(recs) {
				t.Errorf("result = %+v, want 0 ok / %d failed", res, len(recs))
			}
			// Only a failed commit leaves the outcome unknown.
			if got := errors.Is(err, ErrCommitUncertain); got != (tc.name == "commit") {
				t.Errorf("ErrCommitUncertain = %v for %s failure", got, tc.name)
			}
		})
	}
}

func TestExec(t *testing.T) {
	ctx := context.Background()

	f := &fakeDB{}
	if err := newInserter(t, f, Postgres).Exec(ctx, "CREATE TABLE a (x int); CREATE TABLE b (y int)"); err != nil {
		t.Fatal(err)
	}
	if got := f.lines(); len(got) != 1 || !strings.Contains(got[0], "CREATE TABLE b") {
		t.Errorf("a dialect without SplitScript sends the script whole, got %q", got)
	}

	f = &fakeDB{}
	if err := newInserter(t, f, HANA).Exec(ctx, "CREATE TABLE a (x INT);\nCREATE TABLE b (y INT);"); err != nil {
		t.Fatal(err)
	}
	if got := f.lines(); len(got) != 2 || got[0] != "CREATE TABLE a (x INT)" || got[1] != "CREATE TABLE b (y INT)" {
		t.Errorf("HANA script should be split, got %q", got)
	}

	f = &fakeDB{failExec: true}
	if err := newInserter(t, f, HANA).Exec(ctx, "SELECT 1"); err == nil || !strings.HasPrefix(err.Error(), "hana: exec") {
		t.Errorf("err = %v", err)
	}

	bad := Dialect{Name: "x", SplitScript: func(string) ([]string, error) { return nil, errors.New("split boom") }}
	if err := New(nil, config.TargetConfig{}, bad).Exec(ctx, "x"); err == nil || !strings.Contains(err.Error(), "split boom") {
		t.Errorf("err = %v", err)
	}
}

func TestPingAndClose(t *testing.T) {
	in := newInserter(t, &fakeDB{}, Postgres)
	if err := in.Ping(context.Background()); err != nil {
		t.Errorf("Ping = %v", err)
	}
	if err := in.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bad conn", fmt.Errorf("hana: begin: %w", driver.ErrBadConn), true},
		{"eof", fmt.Errorf("x: %w", io.EOF), true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"conn reset", fmt.Errorf("x: %w", syscall.ECONNRESET), true},
		{"conn refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"net timeout", &net.DNSError{IsTimeout: true}, true},
		{"commit uncertain", fmt.Errorf("pg: commit: %w: %w", ErrCommitUncertain, driver.ErrBadConn), false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"server error", errors.New(`relation "t" does not exist`), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Retryable(tc.err); got != tc.want {
				t.Errorf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestOpen_UnsupportedDriver(t *testing.T) {
	_, err := Open(context.Background(), config.TargetConfig{DB: "mysql"}, nil)
	if err == nil || !strings.Contains(err.Error(), `unsupported target.db "mysql"`) {
		t.Fatalf("err = %v", err)
	}
}

// TestOpen_SelectsDriver confirms each supported name picks its driver; every
// connection attempt fails fast (nothing is listening, or the destination
// cannot be resolved) and the error carries the driver's prefix.
func TestOpen_SelectsDriver(t *testing.T) {
	for db, prefix := range map[string]string{
		"postgres": "postgres:", "postgresql": "postgres:", "": "postgres:",
		"oracle": "oracle:", "hana": "hana:",
	} {
		t.Run(db, func(t *testing.T) {
			target := config.TargetConfig{DB: db, Destination: "NOPE"}
			if db == "" || strings.HasPrefix(db, "postgres") {
				// Postgres has no destination support: point it at a closed port.
				target = config.TargetConfig{DB: db, Host: "127.0.0.1", Port: 1, SSLMode: "disable"}
			}
			_, err := Open(context.Background(), target, nil)
			if err == nil || !strings.HasPrefix(err.Error(), prefix) {
				t.Fatalf("err = %v, want prefix %q", err, prefix)
			}
		})
	}
}

func TestEndpointFromDestination(t *testing.T) {
	tests := []struct {
		name     string
		dest     destination.Destination
		wantHost string
		wantPort int
	}{
		{"host and port", destination.Destination{Host: "h", Port: "1234"}, "h", 1234},
		{"no port", destination.Destination{Host: "h"}, "h", 0},
		{"url fallback", destination.Destination{URL: "http://dbhost:1750"}, "dbhost", 1750},
		{"explicit port beats url", destination.Destination{URL: "http://dbhost:1750", Port: "99"}, "dbhost", 99},
		{"host beats url", destination.Destination{Host: "h", URL: "http://other:1"}, "h", 0},
		{"bad url", destination.Destination{URL: "://"}, "", 0},
		{"empty", destination.Destination{}, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, port := endpointFromDestination(&tc.dest)
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("got %q:%d, want %q:%d", host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestResolveDestination(t *testing.T) {
	ctx := context.Background()

	t.Run("direct", func(t *testing.T) {
		target := config.TargetConfig{Host: "h", Port: 5, User: "u", Password: "p"}
		d, dialer, err := resolveDestination(ctx, target, nil, "x")
		if err != nil || dialer != nil {
			t.Fatalf("err=%v dialer=%v", err, dialer)
		}
		if d.Host != "h" || d.Port != "5" || d.User != "u" || d.Password != "p" {
			t.Errorf("dest = %+v", d)
		}
	})

	t.Run("destination without a resolver", func(t *testing.T) {
		_, _, err := resolveDestination(ctx, config.TargetConfig{Destination: "DEST"}, nil, "x")
		if err == nil || !strings.Contains(err.Error(), "no BTP") || !strings.HasPrefix(err.Error(), "x:") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("resolver success", func(t *testing.T) {
		want := &destination.Destination{Host: "from-btp"}
		dialer := &connectivity.Dialer{}
		resolve := func(_ context.Context, name string) (*destination.Destination, *connectivity.Dialer, error) {
			if name != "DEST" {
				t.Errorf("resolved %q", name)
			}
			return want, dialer, nil
		}
		d, dl, err := resolveDestination(ctx, config.TargetConfig{Destination: "DEST"}, resolve, "x")
		if err != nil || d != want || dl != dialer {
			t.Fatalf("d=%v dl=%v err=%v", d, dl, err)
		}
	})

	t.Run("resolver failure", func(t *testing.T) {
		boom := errors.New("lookup failed")
		resolve := func(context.Context, string) (*destination.Destination, *connectivity.Dialer, error) {
			return nil, nil, boom
		}
		_, _, err := resolveDestination(ctx, config.TargetConfig{Destination: "DEST"}, resolve, "x")
		if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "x:") {
			t.Fatalf("err = %v", err)
		}
	})
}

// failingResolver is a DestinationResolver that always fails, for driver
// tests that must not touch the network.
func failingResolver(context.Context, string) (*destination.Destination, *connectivity.Dialer, error) {
	return nil, nil, errors.New("resolver boom")
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "c", "d"); got != "c" {
		t.Errorf("got %q, want c", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestBtpDial_BadAddress(t *testing.T) {
	for _, addr := range []string{"no-port", "host:notaport", "host:70000"} {
		if _, err := btpDial(context.Background(), nil, "", "x", addr); err == nil || !strings.HasPrefix(err.Error(), "x dialer:") {
			t.Errorf("btpDial(%q) err = %v, want a parse error", addr, err)
		}
	}
}
