package handler

import "net/http"

// GET /status — whether a run is in progress, and the result of the most
// recent finished /process run.
func (h *Handlers) status(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	resp := map[string]any{"running": h.running}
	if h.last == nil {
		if !h.running {
			resp["status"] = "no run yet"
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["result"] = h.last
	resp["at"] = h.lastAt
	if h.lastErr != nil {
		resp["error"] = h.lastErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}
