// Package health serves the process liveness and readiness probes. Both
// probes answer with fixed, secret-free JSON bodies: dependency failures are
// logged server-side and never echoed to the wire, so a probe response can
// never leak a DSN, driver diagnostic, or version detail.
package health

import (
	"context"
	"log"
	"net/http"
)

// Checker reports whether one readiness dependency is available. *sql.DB
// satisfies it through PingContext.
type Checker interface {
	PingContext(ctx context.Context) error
}

// The only bodies the probes ever write.
const (
	okBody          = `{"status":"ok"}`
	unavailableBody = `{"status":"unavailable"}`
)

// Handler serves /healthz and /readyz.
type Handler struct {
	ready  Checker
	logger *log.Logger
}

// NewHandler builds the probe handler. ready probes the readiness dependency
// (typically the MySQL pool); a nil Checker reports ready. logger receives
// probe failure notes and defaults to the standard logger.
func NewHandler(ready Checker, logger *log.Logger) *Handler {
	return &Handler{ready: ready, logger: logger}
}

// Live reports process liveness. A live process always answers 200.
func (h *Handler) Live(w http.ResponseWriter, _ *http.Request) {
	writeFixed(w, http.StatusOK, okBody)
}

// Ready reports whether the process can serve traffic: 200 while the
// readiness dependency answers, 503 otherwise. The dependency error reaches
// the server log only — the response body stays the fixed unavailable
// document.
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	if h.ready == nil {
		writeFixed(w, http.StatusOK, okBody)
		return
	}
	if err := h.ready.PingContext(r.Context()); err != nil {
		h.logf("readiness check failed: %v", err)
		writeFixed(w, http.StatusServiceUnavailable, unavailableBody)
		return
	}
	writeFixed(w, http.StatusOK, okBody)
}

func (h *Handler) logf(format string, args ...any) {
	if h.logger != nil {
		h.logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func writeFixed(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
