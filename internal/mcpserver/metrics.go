package mcpserver

import "github.com/prometheus/client_golang/prometheus"

const (
	outcomeOK            = "ok"
	outcomeToolError     = "tool_error"
	outcomeProtocolError = "protocol_error"
	outcomeRateLimited   = "rate_limited"
)

// metrics counts tool calls by principal, tool, and outcome. It is a plain
// Collector so the API can register it on its own per-router registry.
type metrics struct {
	calls *prometheus.CounterVec
}

func newMetrics() *metrics {
	return &metrics{
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "blob_indexer_mcp_tool_calls_total",
			Help: "MCP tool calls by API key name, tool, and outcome.",
		}, []string{"principal", "tool", "outcome"}),
	}
}

func (m *metrics) record(principalName, tool, outcome string) {
	m.calls.WithLabelValues(principalName, tool, outcome).Inc()
}

func (m *metrics) Describe(ch chan<- *prometheus.Desc) { m.calls.Describe(ch) }
func (m *metrics) Collect(ch chan<- prometheus.Metric) { m.calls.Collect(ch) }
