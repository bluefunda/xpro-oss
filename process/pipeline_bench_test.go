package process

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/bluefunda/xpro-oss/config"
	"github.com/bluefunda/xpro-oss/db"
	"github.com/bluefunda/xpro-oss/model"
	"github.com/bluefunda/xpro-oss/parser"
)

// newFakeSource builds an in-memory fs.FS via the standard library's
// testing/fstest.MapFS — Source is a plain fs.FS, so no hand-rolled fake is
// needed. A benchmark measures xpro-oss's own orchestration overhead
// (batching, worker pool, parsing), not disk or network I/O.
func newFakeSource(n int) fstest.MapFS {
	const xml = `<Root><Account><Number>0000%06d</Number></Account><Billing><Amount>%d.00</Amount></Billing></Root>`
	fsys := make(fstest.MapFS, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("file-%d.xml", i)
		fsys[name] = &fstest.MapFile{Data: []byte(fmt.Sprintf(xml, i, i%1000))}
	}
	return fsys
}

// fakeTarget is an in-memory process.Inserter — InsertBatch just counts
// records, so the benchmark isolates pipeline overhead from any real
// database's latency.
type fakeTarget struct {
	batchSize int
	mu        sync.Mutex // InsertBatch runs from concurrent batch goroutines
	inserted  int
}

func (f *fakeTarget) DefaultBatchSize() int { return f.batchSize }
func (f *fakeTarget) InsertBatch(ctx context.Context, records []model.Record) (db.BatchResult, error) {
	f.mu.Lock()
	f.inserted += len(records)
	f.mu.Unlock()
	return db.BatchResult{Success: len(records)}, nil
}

// BenchmarkPipeline_Run measures files/sec through the full read → parse →
// batch → insert orchestration path, DB and disk I/O replaced with in-memory
// fakes. It answers one narrow question: is xpro-oss's own batching/worker-
// pool logic a bottleneck? It does not — and cannot — predict real-world
// throughput against an actual Oracle/PostgreSQL target, which depends on
// network latency, connection pool limits, and target hardware.
func BenchmarkPipeline_Run(b *testing.B) {
	const numFiles = 50_000
	src := newFakeSource(numFiles)

	mapping, err := parser.Compile([]config.MappingField{
		{Column: "account_number", XPath: "/Root/Account/Number"},
		{Column: "amount", XPath: "/Root/Billing/Amount"},
		{Column: "raw_xml", XPath: ""},
	})
	if err != nil {
		b.Fatal(err)
	}

	for _, workers := range []int{8, 32, 128} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				target := &fakeTarget{batchSize: 500}
				p := &Pipeline{
					Source:  src,
					Mapping: mapping,
					Target:  target,
					Workers: workers,
				}
				res, err := p.Run(context.Background())
				if err != nil {
					b.Fatal(err)
				}
				if res.Inserted != numFiles {
					b.Fatalf("inserted %d, want %d", res.Inserted, numFiles)
				}
			}
			b.ReportMetric(float64(numFiles), "files/op")
		})
	}
}
