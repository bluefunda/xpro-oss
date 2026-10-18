package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/bluefunda/xpro-oss/process"
)

// errSource wraps a SourceFactory failure so a blocking caller can get 503.
type errSource struct{ err error }

func (e errSource) Error() string { return e.err.Error() }
func (e errSource) Unwrap() error { return e.err }

// POST /process — start one source → target pass.
//
// Only one run executes at a time: a second request while one is in flight
// gets 409 (two overlapping runs would insert every file twice). By default
// the run proceeds in the background and the response is 202; poll
// GET /status for the outcome. With ?wait=true the response blocks until the
// run finishes and carries its Result (a client that disconnects early does
// not cancel the run). ?limit=N caps the number of files processed (e.g. for
// a single-record smoke test against a directory with many files).
func (h *Handlers) process(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a non-negative integer"})
			return
		}
		limit = n
	}
	wait := false
	if s := r.URL.Query().Get("wait"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wait must be a boolean"})
			return
		}
		wait = b
	}

	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a run is already in progress"})
		return
	}
	h.running = true
	h.mu.Unlock()

	type outcome struct {
		result process.Result
		err    error
	}
	done := make(chan outcome, 1)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		res, err := h.run(limit)

		h.mu.Lock()
		h.last, h.lastErr, h.lastAt, h.running = &res, err, time.Now(), false
		h.mu.Unlock()
		done <- outcome{res, err}
	}()

	if !wait {
		w.Header().Set("Location", "/status")
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
		return
	}

	select {
	case o := <-done:
		var se errSource
		switch {
		case errors.As(o.err, &se):
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": o.err.Error()})
		case o.err != nil:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": o.err.Error(), "result": o.result})
		default:
			writeJSON(w, http.StatusOK, o.result)
		}
	case <-r.Context().Done():
		// Client went away; the run keeps going and is visible via /status.
	}
}

// run opens the source, runs the pipeline under the handler's base context
// (so shutdown interrupts it, and a disconnected client does not), and closes
// the source.
func (h *Handlers) run(limit int) (process.Result, error) {
	timeout := h.opts.RunTimeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	ctx, cancel := context.WithTimeout(h.base, timeout)
	defer cancel()

	src, closeFn, err := h.newSource(ctx)
	if err != nil {
		return process.Result{}, errSource{err}
	}
	if closeFn != nil {
		defer func() { _ = closeFn() }()
	}

	p := &process.Pipeline{
		Source:     src,
		Mapping:    h.mapping,
		Target:     h.target,
		BatchSize:  h.opts.BatchSize,
		Workers:    h.opts.Workers,
		Limit:      limit,
		RetryDelay: time.Second,
	}
	return p.Run(ctx)
}
