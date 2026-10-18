// Package handler exposes xpro-oss's HTTP surface: /healthz, /readyz,
// /process, /status.
package handler

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bluefunda/xpro-oss/parser"
	"github.com/bluefunda/xpro-oss/process"
)

// defaultRunTimeout bounds one /process run when Options.RunTimeout is unset.
const defaultRunTimeout = 30 * time.Minute

// SourceFactory opens a fresh source for one /process run. closeFn, if
// non-nil, is called when the run ends (an SFTP source connects a new session
// per run; os.DirFS needs neither connecting nor closing). The factory — not
// the pipeline — owns the source's lifecycle.
type SourceFactory func(ctx context.Context) (fsys fs.FS, closeFn func() error, err error)

// Options tunes how /process runs the pipeline. The zero value is usable.
type Options struct {
	BatchSize  int           // 0 = the target's default
	Workers    int           // 0 = pipeline default
	RunTimeout time.Duration // 0 = 30 minutes
}

// Handlers holds the dependencies every HTTP handler needs.
type Handlers struct {
	opts      Options
	mapping   []parser.Mapping
	target    process.Inserter
	newSource SourceFactory
	base      context.Context

	wg sync.WaitGroup // in-flight runs, awaited by Wait

	mu      sync.Mutex
	running bool
	last    *process.Result
	lastErr error
	lastAt  time.Time
}

// New constructs Handlers. base is the parent context of every run: cancel it
// on shutdown to interrupt an in-flight run (and use Wait to let it wind
// down). A nil base means context.Background().
func New(base context.Context, opts Options, mapping []parser.Mapping, target process.Inserter, newSource SourceFactory) *Handlers {
	if base == nil {
		base = context.Background()
	}
	return &Handlers{opts: opts, mapping: mapping, target: target, newSource: newSource, base: base}
}

// Wait blocks until any in-flight run has finished or ctx is done.
func (h *Handlers) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RegisterRoutes wires every endpoint onto mux. /healthz and /readyz are
// always unauthenticated (they're probes); mws wraps /process and /status,
// applied outermost-first — see Middleware. Called with no mws, xpro-oss's
// own endpoints are fully open, matching its "no auth baked into the
// generic core" scope; a closed-source layer built on xpro-oss supplies its
// own auth/audit/observability middleware here without forking this package.
func (h *Handlers) RegisterRoutes(mux *http.ServeMux, mws ...Middleware) {
	chain := Chain(mws...)
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.Handle("POST /process", chain(http.HandlerFunc(h.process)))
	mux.Handle("GET /status", chain(http.HandlerFunc(h.status)))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "err", err)
	}
}
