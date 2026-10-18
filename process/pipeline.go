// Package process orchestrates the read → parse → batch → insert pipeline:
// split file names into DB-aware batches, run each batch in its own
// goroutine bounded by a worker pool (not one goroutine per file — see
// xpro's AGENTS.md notes on why unbounded fan-out doesn't scale safely),
// and retry a batch once inline when the whole transaction fails with a
// transient, connection-level error. This inline retry replaces the separate
// retry-processor daemon xpro used.
package process

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/bluefunda/xpro-oss/db"
	"github.com/bluefunda/xpro-oss/model"
	"github.com/bluefunda/xpro-oss/parser"
)

// defaultMaxErrors caps Result.Errors so a run with a million bad rows can't
// balloon memory or the /status response.
const defaultMaxErrors = 100

// Inserter is the subset of db.Inserter that Pipeline actually uses — a
// small, consumer-defined interface.
type Inserter interface {
	InsertBatch(ctx context.Context, records []model.Record) (db.BatchResult, error)
	DefaultBatchSize() int
}

// Pipeline wires one source through the mapping-driven parser into one
// target for a single run. Source is only read: whoever created it (and
// connected it, if it needs a session) owns closing it.
type Pipeline struct {
	Source     fs.FS
	Mapping    []parser.Mapping
	Target     Inserter
	BatchSize  int           // 0 = Target.DefaultBatchSize()
	Workers    int           // 0 = 8
	Limit      int           // 0 = no limit; otherwise cap the number of files processed this run
	MaxErrors  int           // 0 = 100; cap on Result.Errors entries kept
	RetryDelay time.Duration // pause before the single transient-failure retry; 0 = none
	Logger     *slog.Logger  // nil = slog.Default()
}

// Result summarizes one pipeline run. Every file is accounted for:
// Inserted + Failed == Files unless the run was interrupted (see Run).
type Result struct {
	Files    int `json:"files"`
	Inserted int `json:"inserted"`
	// Failed counts files that were not inserted: unreadable, unparseable,
	// rejected by the database, or never started because the run was cancelled.
	Failed  int      `json:"failed"`
	Batches int      `json:"batches"`
	Errors  []string `json:"errors,omitempty"`
	// ErrorsOmitted counts failures beyond the MaxErrors kept in Errors.
	ErrorsOmitted int           `json:"errorsOmitted,omitempty"`
	Elapsed       time.Duration `json:"elapsedNs"`
}

func (r *Result) addError(max int, msg string) {
	if len(r.Errors) < max {
		r.Errors = append(r.Errors, msg)
		return
	}
	r.ErrorsOmitted++
}

// Run lists every file the Source has, splits it into batches, and drains
// them through a bounded worker pool. If ctx is cancelled mid-run, batches
// not yet started are counted as failed, in-flight batches finish or abort
// with their database calls, and Run returns the partial Result together
// with an error wrapping ctx.Err() (unless everything had already succeeded).
func (p *Pipeline) Run(ctx context.Context) (Result, error) {
	start := time.Now()
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}
	maxErrors := p.MaxErrors
	if maxErrors <= 0 {
		maxErrors = defaultMaxErrors
	}

	entries, err := fs.ReadDir(p.Source, ".")
	if err != nil {
		return Result{}, fmt.Errorf("list source: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if p.Limit > 0 && len(names) > p.Limit {
		names = names[:p.Limit]
	}

	batchSize := p.BatchSize
	if batchSize <= 0 {
		batchSize = p.Target.DefaultBatchSize()
	}
	workers := p.Workers
	if workers <= 0 {
		workers = 8
	}
	batches := splitInGroups(names, batchSize)

	log.Info("pipeline run starting", "files", len(names), "batches", len(batches), "batchSize", batchSize, "workers", workers)

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	res := Result{Files: len(names), Batches: len(batches)}

	skipped := 0
dispatch:
	for i, batch := range batches {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			for _, rest := range batches[i:] {
				skipped += len(rest)
			}
			break dispatch
		}
		wg.Add(1)
		go func(idx int, names []string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Error("batch panicked", "batch", idx, "panic", r)
					mu.Lock()
					res.Failed += len(names)
					res.addError(maxErrors, fmt.Sprintf("batch %d panicked: %v", idx, r))
					mu.Unlock()
				}
			}()

			br := p.runBatch(ctx, log, idx, names)

			mu.Lock()
			res.Inserted += br.Success
			res.Failed += br.Failed
			for _, e := range br.Errors {
				res.addError(maxErrors, e.Error())
			}
			mu.Unlock()
		}(i, batch)
	}
	wg.Wait()

	if skipped > 0 {
		res.Failed += skipped
		res.addError(maxErrors, fmt.Sprintf("%d files not processed: %v", skipped, ctx.Err()))
	}
	var runErr error
	if ctx.Err() != nil && res.Failed > 0 {
		runErr = fmt.Errorf("run interrupted: %w", ctx.Err())
	}

	res.Elapsed = time.Since(start)
	log.Info("pipeline run finished", "inserted", res.Inserted, "failed", res.Failed, "elapsed", res.Elapsed)
	return res, runErr
}

// runBatch reads + parses one batch of files and inserts the result. Files
// that can't be read or parsed are counted as failed (with their error) —
// never silently dropped. When the whole transaction fails with a transient
// error (db.Retryable: dropped connection, timeout — not a server-side error
// or an uncertain commit) and no row got through, it retries once. Per-row
// failures are already isolated by the Inserter and are not retried, since
// re-running a partially-successful batch risks duplicate-key errors on the
// rows that already committed.
func (p *Pipeline) runBatch(ctx context.Context, log *slog.Logger, idx int, names []string) db.BatchResult {
	var out db.BatchResult
	records := make([]model.Record, 0, len(names))
	for _, name := range names {
		raw, err := fs.ReadFile(p.Source, name)
		if err != nil {
			log.Error("read", "batch", idx, "file", name, "err", err)
			out.Failed++
			out.Errors = append(out.Errors, fmt.Errorf("%s: read: %w", name, err))
			continue
		}
		rec, err := parser.Parse(name, raw, p.Mapping)
		if err != nil {
			log.Error("parse", "batch", idx, "file", name, "err", err)
			out.Failed++
			out.Errors = append(out.Errors, fmt.Errorf("%s: %w", name, err))
			continue
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		return out
	}

	result, err := p.Target.InsertBatch(ctx, records)
	if err != nil && result.Success == 0 && result.Failed == len(records) && db.Retryable(err) && ctx.Err() == nil {
		log.Warn("batch failed entirely, retrying once", "batch", idx, "err", err)
		if p.RetryDelay > 0 {
			select {
			case <-time.After(p.RetryDelay):
			case <-ctx.Done():
			}
		}
		if ctx.Err() == nil {
			if retryResult, retryErr := p.Target.InsertBatch(ctx, records); retryErr == nil || retryResult.Success > 0 {
				result, err = retryResult, retryErr
			}
		}
	}

	if err != nil {
		// Transaction-level failure: report why, not just how many.
		out.Errors = append(out.Errors, fmt.Errorf("batch %d: %w", idx, err))
	}
	out.Success += result.Success
	out.Failed += result.Failed
	out.Errors = append(out.Errors, result.Errors...)
	return out
}

// splitInGroups splits names into batches of at most groupSize.
func splitInGroups(names []string, groupSize int) [][]string {
	if len(names) == 0 {
		return nil
	}
	if len(names) <= groupSize {
		return [][]string{names}
	}
	var groups [][]string
	for i := 0; i < len(names); i += groupSize {
		end := i + groupSize
		if end > len(names) {
			end = len(names)
		}
		groups = append(groups, names[i:end])
	}
	return groups
}
