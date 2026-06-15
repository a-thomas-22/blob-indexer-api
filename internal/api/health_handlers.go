package api

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// readyzTimeout bounds the database ping performed by the readiness probe so a
// stalled connection cannot hang the probe.
const readyzTimeout = 2 * time.Second

// HealthResponse is the minimal body returned by the liveness/readiness probes.
// Kubernetes only inspects the HTTP status code; the body is for humans/curl.
type HealthResponse struct {
	Status string `json:"status"`
}

// Healthz godoc
// @Summary Liveness probe
// @Description Liveness check that does NOT touch the database, so a transient
// @Description database problem never restarts a running pod. Returns 200 while
// @Description the process is serving.
// @Tags health
// @Produce json
// @Success 200 {object} HealthResponse "Service is alive"
// @Router /healthz [get]
func (a *API) Healthz(w http.ResponseWriter, _ *http.Request) {
	a.respondJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
}

// Readyz godoc
// @Summary Readiness probe
// @Description Readiness check that pings the database. Returns 503 when the
// @Description database is unreachable so traffic is only routed to pods that
// @Description can serve queries.
// @Tags health
// @Produce json
// @Success 200 {object} HealthResponse "Service is ready"
// @Failure 503 {object} HealthResponse "Database unreachable"
// @Router /readyz [get]
func (a *API) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()

	var ok int
	if err := a.db.GetContext(ctx, &ok, "SELECT 1"); err != nil {
		// Debug level: the kubelet hits /readyz repeatedly while the DB is
		// unreachable, and the 503 already surfaces the state — logging at warn
		// would flood logs and trigger noisy alerts during an outage.
		logger.Debug("Readiness probe failed: database unreachable", zap.Error(err))
		a.respondJSON(w, http.StatusServiceUnavailable, HealthResponse{Status: "unavailable"})
		return
	}
	a.respondJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
}
