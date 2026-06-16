package api

import (
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// rateLimitRejections counts requests rejected by the per-IP rate limiter,
// surfaced via /metrics. Package-level because the limiter middleware is built
// per-router while the collector reads a single process-wide total.
var rateLimitRejections int64

func incRateLimitRejections() { atomic.AddInt64(&rateLimitRejections, 1) }

const metricsNamespace = "blob_indexer"

// apiCollector exposes operational metrics read on scrape: HTTP request counts,
// rate-limit rejections, live WebSocket connections, and database pool stats.
// Reading on scrape keeps the request path free of per-request instrumentation.
type apiCollector struct {
	api            *API
	totalRequests  *prometheus.Desc
	activeRequests *prometheus.Desc
	wsConnections  *prometheus.Desc
	rlRejections   *prometheus.Desc
	dbConnections  *prometheus.Desc
}

func newAPICollector(a *API) *apiCollector {
	return &apiCollector{
		api:            a,
		totalRequests:  prometheus.NewDesc(metricsNamespace+"_http_requests_total", "Total HTTP requests served.", nil, nil),
		activeRequests: prometheus.NewDesc(metricsNamespace+"_http_active_requests", "In-flight HTTP requests.", nil, nil),
		wsConnections:  prometheus.NewDesc(metricsNamespace+"_websocket_connections", "Live WebSocket client connections.", nil, nil),
		rlRejections:   prometheus.NewDesc(metricsNamespace+"_rate_limit_rejections_total", "Requests rejected by the per-IP rate limiter.", nil, nil),
		dbConnections:  prometheus.NewDesc(metricsNamespace+"_db_connections", "Database connection pool stats by state.", []string{"state"}, nil),
	}
}

func (c *apiCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalRequests
	ch <- c.activeRequests
	ch <- c.wsConnections
	ch <- c.rlRejections
	ch <- c.dbConnections
}

func (c *apiCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.totalRequests, prometheus.CounterValue, float64(atomic.LoadInt64(&c.api.totalRequests)))
	ch <- prometheus.MustNewConstMetric(c.activeRequests, prometheus.GaugeValue, float64(atomic.LoadInt64(&c.api.activeRequests)))
	ch <- prometheus.MustNewConstMetric(c.rlRejections, prometheus.CounterValue, float64(atomic.LoadInt64(&rateLimitRejections)))

	wsCount := 0.0
	if c.api.hub != nil {
		wsCount = float64(c.api.hub.ClientCount())
	}
	ch <- prometheus.MustNewConstMetric(c.wsConnections, prometheus.GaugeValue, wsCount)

	if c.api.db != nil {
		s := c.api.db.Stats()
		ch <- prometheus.MustNewConstMetric(c.dbConnections, prometheus.GaugeValue, float64(s.OpenConnections), "open")
		ch <- prometheus.MustNewConstMetric(c.dbConnections, prometheus.GaugeValue, float64(s.InUse), "in_use")
		ch <- prometheus.MustNewConstMetric(c.dbConnections, prometheus.GaugeValue, float64(s.Idle), "idle")
		ch <- prometheus.MustNewConstMetric(c.dbConnections, prometheus.GaugeValue, float64(s.MaxOpenConnections), "max_open")
	}
}

// metricsHandler serves Prometheus metrics from a dedicated registry, so the
// process-global default registry is never touched (safe to build more than one
// router in tests/dev).
func (a *API) metricsHandler() http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newAPICollector(a))
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
