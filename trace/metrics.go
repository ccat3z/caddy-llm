package trace

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metric names exposed at /metrics.
const (
	metricRequestsTotal = "caddy_llm_trace_requests_total"
	metricInputTokens   = "caddy_llm_trace_input_tokens_total"
	metricCacheTokens   = "caddy_llm_trace_cache_tokens_total"
	metricOutputTokens  = "caddy_llm_trace_output_tokens_total"
)

var (
	requestsDesc = prometheus.NewDesc(metricRequestsTotal,
		"Traced exchanges recorded per trace name, cumulative over the SQLite index (survives restarts and raw-file cleanup).",
		[]string{"trace_name"}, nil)
	inputDesc = prometheus.NewDesc(metricInputTokens,
		"Input tokens per trace name (cache excluded), cumulative over the SQLite index.",
		[]string{"trace_name"}, nil)
	cacheDesc = prometheus.NewDesc(metricCacheTokens,
		"Cache-read tokens per trace name, cumulative over the SQLite index.",
		[]string{"trace_name"}, nil)
	outputDesc = prometheus.NewDesc(metricOutputTokens,
		"Output tokens per trace name, cumulative over the SQLite index.",
		[]string{"trace_name"}, nil)
)

// totalsCollector aggregates llm_requests at scrape time instead of
// maintaining counters in memory, so /metrics reflects the full indexed
// history across restarts. Exchanges without usage contribute to the
// request count but zero tokens.
type totalsCollector struct {
	app *Store
}

// Describe is intentionally empty: an unchecked collector, so missing
// descriptors don't turn a temporarily empty store into a scrape error.
func (c *totalsCollector) Describe(_ chan<- *prometheus.Desc) {}

func (c *totalsCollector) Collect(ch chan<- prometheus.Metric) {
	totals, err := c.app.Storage().UsageTotals(context.Background())
	if err != nil {
		ch <- prometheus.NewInvalidMetric(requestsDesc, err)
		return
	}
	for _, tt := range totals {
		ch <- prometheus.MustNewConstMetric(requestsDesc, prometheus.CounterValue, float64(tt.Requests), tt.TraceName)
		ch <- prometheus.MustNewConstMetric(inputDesc, prometheus.CounterValue, float64(tt.Input), tt.TraceName)
		ch <- prometheus.MustNewConstMetric(cacheDesc, prometheus.CounterValue, float64(tt.Cache), tt.TraceName)
		ch <- prometheus.MustNewConstMetric(outputDesc, prometheus.CounterValue, float64(tt.Output), tt.TraceName)
	}
}

// metricsHandler lazily builds the /metrics HTTP handler (own registry:
// just the trace metrics, not the process defaults). Lazy so tests can
// construct TraceAPI without Provision.
func (t *TraceAPI) metricsHandler() http.Handler {
	t.metricsOnce.Do(func() {
		reg := prometheus.NewRegistry()
		reg.MustRegister(&totalsCollector{app: t.app})
		t.metrics = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	})
	return t.metrics
}
