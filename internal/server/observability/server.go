package observability

import (
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type ServerMetrics struct {
	ingestBatches  *prometheus.CounterVec
	ingestEntries  *prometheus.CounterVec
	ingestBytes    *prometheus.CounterVec
	ingestDuration *prometheus.HistogramVec
	queryRequests  *prometheus.CounterVec
	queryRows      *prometheus.HistogramVec
	queryDuration  *prometheus.HistogramVec
	jobRuns        *prometheus.CounterVec
	jobProcessed   *prometheus.CounterVec
	jobDuration    *prometheus.HistogramVec
	jobLastSuccess *prometheus.GaugeVec
}

func NewServerMetrics(registerer prometheus.Registerer, stats func() sql.DBStats) *ServerMetrics {
	metrics := &ServerMetrics{
		ingestBatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "server_ingest", Name: "batches_total",
			Help: "Batches completed by stable result class.",
		}, []string{"result"}),
		ingestEntries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "server_ingest", Name: "entries_total",
			Help: "Entries observed in completed batches by stable result class.",
		}, []string{"result"}),
		ingestBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "server_ingest", Name: "payload_bytes_total",
			Help: "Payload bytes observed in completed batches by stable result class.",
		}, []string{"result"}),
		ingestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gline", Subsystem: "server_ingest", Name: "duration_seconds",
			Help: "Ingest service duration by stable result class.", Buckets: prometheus.DefBuckets,
		}, []string{"result"}),
		queryRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "server_query", Name: "requests_total",
			Help: "Queries completed by bounded filter shape and result.",
		}, []string{"filter_shape", "result"}),
		queryRows: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gline", Subsystem: "server_query", Name: "rows",
			Help: "Rows returned by successful queries.", Buckets: []float64{0, 1, 10, 50, 100, 250, 500},
		}, []string{"filter_shape"}),
		queryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gline", Subsystem: "server_query", Name: "duration_seconds",
			Help: "Query service duration by bounded filter shape and result.", Buckets: prometheus.DefBuckets,
		}, []string{"filter_shape", "result"}),
		jobRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "server_background_job", Name: "runs_total",
			Help: "Background job repository steps by stable job and result.",
		}, []string{"job", "result"}),
		jobProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "server_background_job", Name: "processed_total",
			Help: "Rows processed by successful background job steps.",
		}, []string{"job"}),
		jobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gline", Subsystem: "server_background_job", Name: "duration_seconds",
			Help: "Background job repository step duration.", Buckets: prometheus.DefBuckets,
		}, []string{"job", "result"}),
		jobLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "server_background_job", Name: "last_success_timestamp_seconds",
			Help: "Unix timestamp of the latest successful background job step.",
		}, []string{"job"}),
	}
	registerer.MustRegister(
		metrics.ingestBatches, metrics.ingestEntries, metrics.ingestBytes, metrics.ingestDuration,
		metrics.queryRequests, metrics.queryRows, metrics.queryDuration,
		metrics.jobRuns, metrics.jobProcessed, metrics.jobDuration, metrics.jobLastSuccess,
	)
	if stats != nil {
		registerDBMetrics(registerer, stats)
	}
	return metrics
}

func (m *ServerMetrics) ObserveIngest(result string, entries, payloadBytes int, duration time.Duration) {
	m.ingestBatches.WithLabelValues(result).Inc()
	if entries > 0 {
		m.ingestEntries.WithLabelValues(result).Add(float64(entries))
	}
	if payloadBytes > 0 {
		m.ingestBytes.WithLabelValues(result).Add(float64(payloadBytes))
	}
	m.ingestDuration.WithLabelValues(result).Observe(duration.Seconds())
}

func (m *ServerMetrics) ObserveQuery(result, filterShape string, rows int, duration time.Duration) {
	m.queryRequests.WithLabelValues(filterShape, result).Inc()
	m.queryDuration.WithLabelValues(filterShape, result).Observe(duration.Seconds())
	if result == "success" {
		m.queryRows.WithLabelValues(filterShape).Observe(float64(rows))
	}
}

func (m *ServerMetrics) ObserveBackgroundJob(job, result string, processed int64, duration time.Duration) {
	m.jobRuns.WithLabelValues(job, result).Inc()
	m.jobDuration.WithLabelValues(job, result).Observe(duration.Seconds())
	if result == "success" {
		m.jobProcessed.WithLabelValues(job).Add(float64(processed))
		m.jobLastSuccess.WithLabelValues(job).Set(float64(time.Now().Unix()))
	}
}

func registerDBMetrics(registerer prometheus.Registerer, stats func() sql.DBStats) {
	registerer.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "server_db_pool", Name: "open_connections",
			Help: "Open database connections.",
		}, func() float64 { return float64(stats().OpenConnections) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "server_db_pool", Name: "in_use_connections",
			Help: "Database connections currently in use.",
		}, func() float64 { return float64(stats().InUse) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "server_db_pool", Name: "idle_connections",
			Help: "Idle database connections.",
		}, func() float64 { return float64(stats().Idle) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "server_db_pool", Name: "wait_total",
			Help: "Total waits for a database connection.",
		}, func() float64 { return float64(stats().WaitCount) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "server_db_pool", Name: "wait_duration_seconds_total",
			Help: "Cumulative time spent waiting for a database connection.",
		}, func() float64 { return stats().WaitDuration.Seconds() }),
	)
}
