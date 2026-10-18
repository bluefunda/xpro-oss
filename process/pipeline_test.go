package process

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bluefunda/xpro-oss/config"
	"github.com/bluefunda/xpro-oss/db"
	"github.com/bluefunda/xpro-oss/model"
	"github.com/bluefunda/xpro-oss/parser"
)

// TestPipeline_Limit confirms Limit caps how many files a run processes —
// this is the safety valve for a single-record smoke test against a
// directory that may contain many files; without it, /process would insert
// every file it finds.
func TestPipeline_Limit(t *testing.T) {
	src := newFakeSource(10)
	mapping, err := parser.Compile([]config.MappingField{
		{Column: "account_number", XPath: "/Root/Account/Number"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	target := &fakeTarget{batchSize: 500}
	p := &Pipeline{Source: src, Mapping: mapping, Target: target, Limit: 1}

	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1", res.Files)
	}
	if res.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1", res.Inserted)
	}
}

// TestPipeline_NoLimit confirms Limit=0 (the zero value) processes every
// file — the documented default, unchanged by adding Limit.
func TestPipeline_NoLimit(t *testing.T) {
	src := newFakeSource(10)
	mapping, err := parser.Compile([]config.MappingField{
		{Column: "account_number", XPath: "/Root/Account/Number"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	target := &fakeTarget{batchSize: 500}
	p := &Pipeline{Source: src, Mapping: mapping, Target: target}

	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Files != 10 {
		t.Errorf("Files = %d, want 10", res.Files)
	}
}

func compileMapping(t *testing.T) []parser.Mapping {
	t.Helper()
	m, err := parser.Compile([]config.MappingField{{Column: "n", XPath: "/Root/Account/Number"}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSplitInGroups(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e"}
	if got := splitInGroups(nil, 2); got != nil {
		t.Errorf("empty = %v", got)
	}
	if got := splitInGroups(names, 10); len(got) != 1 || len(got[0]) != 5 {
		t.Errorf("single group = %v", got)
	}
	got := splitInGroups(names, 2)
	if len(got) != 3 || len(got[0]) != 2 || len(got[2]) != 1 || got[2][0] != "e" {
		t.Errorf("groups = %v", got)
	}
}

func TestPipeline_BatchesAndWorkers(t *testing.T) {
	target := &fakeTarget{batchSize: 3}
	p := &Pipeline{Source: newFakeSource(10), Mapping: compileMapping(t), Target: target, Workers: 2}
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Batches != 4 || res.Inserted != 10 || res.Failed != 0 {
		t.Errorf("res = %+v, want 4 batches / 10 inserted", res)
	}
}

func TestPipeline_BatchSizeOverride(t *testing.T) {
	p := &Pipeline{Source: newFakeSource(10), Mapping: compileMapping(t), Target: &fakeTarget{batchSize: 500}, BatchSize: 5}
	if res, _ := p.Run(context.Background()); res.Batches != 2 {
		t.Errorf("Batches = %d, want 2", res.Batches)
	}
}

func TestPipeline_SkipsDirsAndUnparseable(t *testing.T) {
	src := fstest.MapFS{
		"dir/inner.xml": &fstest.MapFile{Data: []byte("<Root/>")},
		"good.xml":      &fstest.MapFile{Data: []byte("<Root><Account><Number>1</Number></Account></Root>")},
		"bad.xml":       &fstest.MapFile{Data: []byte("not xml <<<")},
	}
	target := &fakeTarget{batchSize: 10}
	res, err := (&Pipeline{Source: src, Mapping: compileMapping(t), Target: target}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || target.inserted != 1 {
		t.Errorf("files=%d inserted=%d, want 2 files / 1 inserted (dir ignored)", res.Files, target.inserted)
	}
	// The unparseable file must be accounted for, not silently dropped.
	if res.Failed != 1 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "bad.xml") {
		t.Errorf("res = %+v, want bad.xml reported as the one failure", res)
	}
	if res.Inserted+res.Failed != res.Files {
		t.Errorf("Inserted+Failed = %d, want Files = %d", res.Inserted+res.Failed, res.Files)
	}
}

// scriptedTarget returns canned InsertBatch results in order.
type scriptedTarget struct {
	mu      sync.Mutex
	results []db.BatchResult
	errs    []error
	calls   int
}

func (s *scriptedTarget) DefaultBatchSize() int { return 100 }
func (s *scriptedTarget) InsertBatch(_ context.Context, recs []model.Record) (db.BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	return s.results[i], s.errs[i]
}

func TestPipeline_RetriesWholeBatchFailureOnce(t *testing.T) {
	boom := driver.ErrBadConn
	tg := &scriptedTarget{
		results: []db.BatchResult{{Failed: 2}, {Success: 2}},
		errs:    []error{boom, nil},
	}
	res, _ := (&Pipeline{Source: newFakeSource(2), Mapping: compileMapping(t), Target: tg}).Run(context.Background())
	if tg.calls != 2 || res.Inserted != 2 || res.Failed != 0 {
		t.Errorf("calls=%d res=%+v, want retry to recover both rows", tg.calls, res)
	}
}

func TestPipeline_NoRetryOnPartialFailure(t *testing.T) {
	tg := &scriptedTarget{
		results: []db.BatchResult{{Success: 1, Failed: 1, Errors: []error{errors.New("dup key")}}},
		errs:    []error{nil},
	}
	res, _ := (&Pipeline{Source: newFakeSource(2), Mapping: compileMapping(t), Target: tg}).Run(context.Background())
	if tg.calls != 1 {
		t.Errorf("calls = %d, retrying a partially-committed batch risks duplicates", tg.calls)
	}
	if res.Inserted != 1 || res.Failed != 1 || len(res.Errors) != 1 || res.Errors[0] != "dup key" {
		t.Errorf("res = %+v", res)
	}
}

func TestPipeline_RetryAlsoFails(t *testing.T) {
	boom := driver.ErrBadConn
	tg := &scriptedTarget{results: []db.BatchResult{{Failed: 1}, {Failed: 1}}, errs: []error{boom, boom}}
	res, _ := (&Pipeline{Source: newFakeSource(1), Mapping: compileMapping(t), Target: tg}).Run(context.Background())
	if tg.calls != 2 || res.Failed != 1 || res.Inserted != 0 {
		t.Errorf("calls=%d res=%+v", tg.calls, res)
	}
}

type panicTarget struct{}

func (panicTarget) DefaultBatchSize() int { return 10 }
func (panicTarget) InsertBatch(context.Context, []model.Record) (db.BatchResult, error) {
	panic("kaboom")
}

func TestPipeline_RecoversFromPanic(t *testing.T) {
	res, err := (&Pipeline{Source: newFakeSource(3), Mapping: compileMapping(t), Target: panicTarget{}}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 3 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "kaboom") {
		t.Errorf("res = %+v", res)
	}
}

// closableSource is an fs.FS that also implements io.Closer. A pipeline only
// reads from its source; closing it is the creator's job.
type closableSource struct {
	fstest.MapFS
	closed bool
}

func (c *closableSource) Close() error { c.closed = true; return nil }

func TestPipeline_DoesNotCloseSource(t *testing.T) {
	src := &closableSource{MapFS: newFakeSource(1)}
	if _, err := (&Pipeline{Source: src, Mapping: compileMapping(t), Target: &fakeTarget{batchSize: 5}}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.closed {
		t.Error("Run closed a source it did not create")
	}
}

type unlistableFS struct{ fstest.MapFS }

func (unlistableFS) ReadDir(string) ([]fs.DirEntry, error) { return nil, errors.New("denied") }

func TestPipeline_ListError(t *testing.T) {
	_, err := (&Pipeline{Source: unlistableFS{}, Mapping: compileMapping(t), Target: &fakeTarget{}}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "list source") {
		t.Errorf("err = %v", err)
	}
}

func TestPipeline_ReadErrorCountedAsFailure(t *testing.T) {
	src := unreadableFS{newFakeSource(3), "file-1.xml"}
	target := &fakeTarget{batchSize: 10}
	res, _ := (&Pipeline{Source: src, Mapping: compileMapping(t), Target: target}).Run(context.Background())
	if res.Inserted != 2 || res.Failed != 1 || !strings.Contains(res.Errors[0], "file-1.xml: read") {
		t.Errorf("res = %+v", res)
	}
}

// unreadableFS fails to open one named file.
type unreadableFS struct {
	fstest.MapFS
	bad string
}

func (u unreadableFS) ReadFile(name string) ([]byte, error) {
	if name == u.bad {
		return nil, errors.New("permission denied")
	}
	return u.MapFS.ReadFile(name)
}

func (u unreadableFS) Open(name string) (fs.File, error) {
	if name == u.bad {
		return nil, errors.New("permission denied")
	}
	return u.MapFS.Open(name)
}

func TestPipeline_NoRetryForPermanentOrAmbiguousErrors(t *testing.T) {
	for name, err := range map[string]error{
		"server error":     errors.New(`relation "t" does not exist`),
		"uncertain commit": fmt.Errorf("pg: commit: %w: %w", db.ErrCommitUncertain, driver.ErrBadConn),
	} {
		t.Run(name, func(t *testing.T) {
			tg := &scriptedTarget{results: []db.BatchResult{{Failed: 2}, {Success: 2}}, errs: []error{err, nil}}
			res, _ := (&Pipeline{Source: newFakeSource(2), Mapping: compileMapping(t), Target: tg}).Run(context.Background())
			if tg.calls != 1 {
				t.Errorf("calls = %d, want 1 (retrying could duplicate or is pointless)", tg.calls)
			}
			if res.Failed != 2 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "batch 0") {
				t.Errorf("res = %+v; the batch error must be reported", res)
			}
		})
	}
}

func TestPipeline_RetryDelayHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tg := &cancelOnCallTarget{cancel: cancel}
	start := time.Now()
	(&Pipeline{Source: newFakeSource(1), Mapping: compileMapping(t), Target: tg, RetryDelay: time.Hour}).Run(ctx) //nolint:errcheck // outcome asserted via tg
	if time.Since(start) > 5*time.Second || tg.calls != 1 {
		t.Errorf("calls=%d after %v; a cancelled context must cut the retry wait short and skip the retry", tg.calls, time.Since(start))
	}
}

// cancelOnCallTarget cancels the run's context during its first (transiently
// failing) InsertBatch.
type cancelOnCallTarget struct {
	cancel context.CancelFunc
	calls  int
}

func (c *cancelOnCallTarget) DefaultBatchSize() int { return 10 }
func (c *cancelOnCallTarget) InsertBatch(context.Context, []model.Record) (db.BatchResult, error) {
	c.calls++
	c.cancel()
	return db.BatchResult{Failed: 1}, driver.ErrBadConn
}

func TestPipeline_ErrorsAreCapped(t *testing.T) {
	src := fstest.MapFS{}
	for i := 0; i < 10; i++ {
		src[fmt.Sprintf("bad-%d.xml", i)] = &fstest.MapFile{Data: []byte("<<<")}
	}
	res, _ := (&Pipeline{Source: src, Mapping: compileMapping(t), Target: &fakeTarget{batchSize: 100}, MaxErrors: 3}).Run(context.Background())
	if res.Failed != 10 || len(res.Errors) != 3 || res.ErrorsOmitted != 7 {
		t.Errorf("failed=%d kept=%d omitted=%d, want 10/3/7", res.Failed, len(res.Errors), res.ErrorsOmitted)
	}
}

// gateTarget blocks every InsertBatch until released, so a test can cancel
// the run while batches are queued behind the worker limit.
type gateTarget struct {
	started chan struct{}
	release chan struct{}
}

func (g *gateTarget) DefaultBatchSize() int { return 1 }
func (g *gateTarget) InsertBatch(_ context.Context, recs []model.Record) (db.BatchResult, error) {
	g.started <- struct{}{}
	<-g.release
	return db.BatchResult{Success: len(recs)}, nil
}

func TestPipeline_CancelStopsDispatching(t *testing.T) {
	g := &gateTarget{started: make(chan struct{}, 10), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pipeline{Source: newFakeSource(6), Mapping: compileMapping(t), Target: g, Workers: 1}

	type out struct {
		res Result
		err error
	}
	done := make(chan out, 1)
	go func() { r, e := p.Run(ctx); done <- out{r, e} }()

	<-g.started // batch 0 is in flight; the other 5 wait for the single worker slot
	cancel()
	close(g.release)

	o := <-done
	if !errors.Is(o.err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", o.err)
	}
	if o.res.Inserted+o.res.Failed != o.res.Files {
		t.Errorf("res = %+v; every file must be accounted for", o.res)
	}
	if o.res.Failed < 1 || !strings.Contains(strings.Join(o.res.Errors, "|"), "not processed") {
		t.Errorf("res = %+v; unstarted batches should be reported as not processed", o.res)
	}
}

func TestPipeline_UsesInjectedLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	(&Pipeline{Source: newFakeSource(1), Mapping: compileMapping(t), Target: &fakeTarget{batchSize: 5}, Logger: logger}).Run(context.Background()) //nolint:errcheck // only the log output matters
	if !strings.Contains(buf.String(), "pipeline run finished") {
		t.Errorf("injected logger got %q", buf.String())
	}
}
