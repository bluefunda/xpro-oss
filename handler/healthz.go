package handler

import (
	"context"
	"net/http"
	"time"
)

// GET /healthz — liveness probe.
func (h *Handlers) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// pinger is the optional capability a target offers for readiness checks
// (*db.Inserter implements it).
type pinger interface {
	Ping(ctx context.Context) error
}

// GET /readyz — readiness probe: 200 when the target database answers a
// ping (or the target has no way to be pinged), 503 otherwise.
func (h *Handlers) readyz(w http.ResponseWriter, r *http.Request) {
	if p, ok := h.target.(pinger); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := p.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
