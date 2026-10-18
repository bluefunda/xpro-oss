package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bluefunda/xpro-oss/config"
	"github.com/bluefunda/xpro-oss/db"
	"github.com/bluefunda/xpro-oss/model"
	"github.com/bluefunda/xpro-oss/parser"
)

// fakeTarget counts inserted records. block, when non-nil, holds every
// InsertBatch until it is closed, so tests can observe an in-flight run.
type fakeTarget struct {
	inserted atomic.Int64
	started  chan struct{}
	block    chan struct{}
}

func (f *fakeTarget) DefaultBatchSize() int { return 10 }
func (f *fakeTarget) InsertBatch(ctx context.Context, recs []model.Record) (db.BatchResult, error) {
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return db.BatchResult{Failed: len(recs)}, ctx.Err()
		}
	}
	f.inserted.Add(int64(len(recs)))
	return db.BatchResult{Success: len(recs)}, nil
}

type pingTarget struct {
	fakeTarget
	err error
}

func (p *pingTarget) Ping(context.Context) error { return p.err }

func newMapping(t *testing.T) []parser.Mapping {
	t.Helper()
	m, err := parser.Compile([]config.MappingField{{Column: "n", XPath: "/Root/N"}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func memSource(n int) SourceFactory {
	return func(context.Context) (fs.FS, func() error, error) {
		fsys := fstest.MapFS{}
		for i := 0; i < n; i++ {
			fsys["f"+string(rune('a'+i))+".xml"] = &fstest.MapFile{Data: []byte("<Root><N>1</N></Root>")}
		}
		return fsys, nil, nil
	}
}

func newTestHandlers(t *testing.T, target Target, src SourceFactory) (*Handlers, *http.ServeMux) {
	t.Helper()
	h := New(context.Background(), Options{Workers: 2}, newMapping(t), target, src)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, mux
}

// Target is the handler's view of the database, for readability in tests.
type Target = interface {
	InsertBatch(ctx context.Context, records []model.Record) (db.BatchResult, error)
	DefaultBatchSize() int
}

func do(mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return m
}

// waitIdle blocks until the handler has no run in flight.
func waitIdle(t *testing.T, h *Handlers) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("run did not finish: %v", err)
	}
}

func TestHealthz(t *testing.T) {
	_, mux := newTestHandlers(t, &fakeTarget{}, memSource(0))
	rec := do(mux, "GET", "/healthz")
	if rec.Code != 200 || decode(t, rec)["status"] != "ok" {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestReadyz(t *testing.T) {
	t.Run("no pinger means ready", func(t *testing.T) {
		_, mux := newTestHandlers(t, &fakeTarget{}, memSource(0))
		if rec := do(mux, "GET", "/readyz"); rec.Code != 200 {
			t.Errorf("code = %d", rec.Code)
		}
	})
	t.Run("healthy database", func(t *testing.T) {
		_, mux := newTestHandlers(t, &pingTarget{}, memSource(0))
		if rec := do(mux, "GET", "/readyz"); rec.Code != 200 || decode(t, rec)["status"] != "ready" {
			t.Errorf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	t.Run("database down", func(t *testing.T) {
		_, mux := newTestHandlers(t, &pingTarget{err: errors.New("connection refused")}, memSource(0))
		rec := do(mux, "GET", "/readyz")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "connection refused") {
			t.Errorf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
}

func TestProcess_WaitReturnsResult(t *testing.T) {
	h, mux := newTestHandlers(t, &fakeTarget{}, memSource(3))
	rec := do(mux, "POST", "/process?wait=true")
	if rec.Code != 200 || decode(t, rec)["inserted"] != float64(3) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	waitIdle(t, h)

	st := decode(t, do(mux, "GET", "/status"))
	res, ok := st["result"].(map[string]any)
	if !ok || res["inserted"] != float64(3) || st["running"] != false {
		t.Errorf("status after run = %v", st)
	}
	if _, has := st["error"]; has {
		t.Error("successful run must not report an error")
	}
}

func TestProcess_AsyncByDefault(t *testing.T) {
	target := &fakeTarget{started: make(chan struct{}, 1), block: make(chan struct{})}
	h, mux := newTestHandlers(t, target, memSource(2))

	if got := decode(t, do(mux, "GET", "/status")); got["status"] != "no run yet" || got["running"] != false {
		t.Errorf("status before run = %v", got)
	}

	rec := do(mux, "POST", "/process")
	if rec.Code != http.StatusAccepted || rec.Header().Get("Location") != "/status" {
		t.Fatalf("code=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	<-target.started
	if got := decode(t, do(mux, "GET", "/status")); got["running"] != true {
		t.Errorf("status during run = %v", got)
	}

	close(target.block)
	waitIdle(t, h)
	st := decode(t, do(mux, "GET", "/status"))
	if st["running"] != false || st["result"].(map[string]any)["inserted"] != float64(2) {
		t.Errorf("status after run = %v", st)
	}
}

// TestProcess_RejectsOverlappingRuns: two concurrent runs would insert every
// file twice, so the second request must be refused.
func TestProcess_RejectsOverlappingRuns(t *testing.T) {
	target := &fakeTarget{started: make(chan struct{}, 1), block: make(chan struct{})}
	h, mux := newTestHandlers(t, target, memSource(1))

	if rec := do(mux, "POST", "/process"); rec.Code != http.StatusAccepted {
		t.Fatalf("first run: %d", rec.Code)
	}
	<-target.started

	for _, path := range []string{"/process", "/process?wait=true"} {
		if rec := do(mux, "POST", path); rec.Code != http.StatusConflict {
			t.Errorf("POST %s during a run = %d, want 409", path, rec.Code)
		}
	}

	close(target.block)
	waitIdle(t, h)
	if got := target.inserted.Load(); got != 1 {
		t.Errorf("inserted = %d, want exactly 1 (no duplicate run)", got)
	}
	// Once the run is over a new one is accepted again.
	if rec := do(mux, "POST", "/process?wait=true"); rec.Code != 200 {
		t.Errorf("run after completion = %d", rec.Code)
	}
}

func TestProcess_Limit(t *testing.T) {
	target := &fakeTarget{}
	_, mux := newTestHandlers(t, target, memSource(5))
	rec := do(mux, "POST", "/process?limit=2&wait=true")
	if rec.Code != 200 || target.inserted.Load() != 2 {
		t.Errorf("code=%d inserted=%d body=%s", rec.Code, target.inserted.Load(), rec.Body)
	}
}

func TestProcess_BadQuery(t *testing.T) {
	_, mux := newTestHandlers(t, &fakeTarget{}, memSource(1))
	for _, q := range []string{"limit=abc", "limit=-1", "wait=maybe"} {
		if rec := do(mux, "POST", "/process?"+q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", q, rec.Code)
		}
	}
}

func TestProcess_SourceFactoryError(t *testing.T) {
	h, mux := newTestHandlers(t, &fakeTarget{}, func(context.Context) (fs.FS, func() error, error) {
		return nil, nil, errors.New("no source")
	})
	rec := do(mux, "POST", "/process?wait=true")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no source") {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	waitIdle(t, h)
	if st := decode(t, do(mux, "GET", "/status")); !strings.Contains(st["error"].(string), "no source") {
		t.Errorf("status = %v", st)
	}
}

func TestProcess_ClosesSourceAfterRun(t *testing.T) {
	var closed atomic.Bool
	h, mux := newTestHandlers(t, &fakeTarget{}, func(ctx context.Context) (fs.FS, func() error, error) {
		fsys, _, _ := memSource(1)(ctx)
		return fsys, func() error { closed.Store(true); return nil }, nil
	})
	do(mux, "POST", "/process?wait=true")
	waitIdle(t, h)
	if !closed.Load() {
		t.Error("the source's close function was not called")
	}
}

// brokenFS fails at listing, so the pipeline run returns an error.
type brokenFS struct{ fstest.MapFS }

func (brokenFS) ReadDir(string) ([]fs.DirEntry, error) { return nil, errors.New("list boom") }

func TestProcess_PipelineErrorRecordedInStatus(t *testing.T) {
	h, mux := newTestHandlers(t, &fakeTarget{}, func(context.Context) (fs.FS, func() error, error) { return brokenFS{}, nil, nil })
	rec := do(mux, "POST", "/process?wait=true")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "list boom") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	waitIdle(t, h)
	if st := decode(t, do(mux, "GET", "/status")); !strings.Contains(st["error"].(string), "list boom") {
		t.Errorf("status = %v", st)
	}
}

// TestProcess_ShutdownInterruptsRun: cancelling the handler's base context
// (what main does on SIGTERM) stops an in-flight run, and Wait returns once
// it has wound down — a client disconnect, by contrast, must not.
func TestProcess_ShutdownInterruptsRun(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	target := &fakeTarget{started: make(chan struct{}, 1), block: make(chan struct{})}
	h := New(base, Options{}, newMapping(t), target, memSource(3))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	do(mux, "POST", "/process")
	<-target.started
	cancel()
	waitIdle(t, h)

	st := decode(t, do(mux, "GET", "/status"))
	if st["running"] != false || st["error"] == nil {
		t.Errorf("status = %v, want a finished, interrupted run", st)
	}
}

func TestProcess_ClientDisconnectDoesNotCancelRun(t *testing.T) {
	target := &fakeTarget{started: make(chan struct{}, 1), block: make(chan struct{})}
	h, mux := newTestHandlers(t, target, memSource(1))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/process?wait=true", nil).WithContext(ctx)
	returned := make(chan struct{})
	go func() { mux.ServeHTTP(httptest.NewRecorder(), req); close(returned) }()

	<-target.started
	cancel() // client goes away
	<-returned

	close(target.block)
	waitIdle(t, h)
	if got := target.inserted.Load(); got != 1 {
		t.Errorf("inserted = %d, want the run to complete despite the disconnect", got)
	}
}

func TestWait_HonorsContext(t *testing.T) {
	target := &fakeTarget{started: make(chan struct{}, 1), block: make(chan struct{})}
	h, mux := newTestHandlers(t, target, memSource(1))
	do(mux, "POST", "/process")
	<-target.started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait = %v, want deadline exceeded while a run is in flight", err)
	}
	close(target.block)
	waitIdle(t, h)
}

func TestNew_NilBaseContext(t *testing.T) {
	h := New(nil, Options{}, newMapping(t), &fakeTarget{}, memSource(1)) //nolint:staticcheck // nil is documented as Background
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	if rec := do(mux, "POST", "/process?wait=true"); rec.Code != 200 {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestMethodRouting(t *testing.T) {
	_, mux := newTestHandlers(t, &fakeTarget{}, memSource(0))
	if rec := do(mux, "GET", "/process"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /process = %d, want 405", rec.Code)
	}
}
