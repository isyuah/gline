package observability

import (
	"time"

	"github.com/isyuah/gline/internal/agent/spool"
	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	recordsRead    *prometheus.CounterVec
	batchesSpooled prometheus.Counter
	sendAttempts   *prometheus.CounterVec
	uploadDuration *prometheus.HistogramVec
	pipelineUp     *prometheus.GaugeVec
}

func NewMetrics(registerer prometheus.Registerer, store *spool.WAL) *Metrics {
	metrics := &Metrics{
		recordsRead: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "agent", Name: "records_read_total",
			Help: "Records emitted by a source, classified by parse result.",
		}, []string{"pipeline", "result"}),
		batchesSpooled: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "agent", Name: "batches_spooled_total",
			Help: "Batches durably committed to the local WAL.",
		}),
		sendAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "agent", Name: "send_attempts_total",
			Help: "Batch delivery attempts by stable result class.",
		}, []string{"result"}),
		uploadDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gline", Subsystem: "agent", Name: "upload_duration_seconds",
			Help: "Batch delivery attempt duration by stable result class.", Buckets: prometheus.DefBuckets,
		}, []string{"result"}),
		pipelineUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "agent", Name: "pipeline_up",
			Help: "Whether a configured pipeline is currently running.",
		}, []string{"pipeline"}),
	}
	registerer.MustRegister(metrics.recordsRead, metrics.batchesSpooled, metrics.sendAttempts, metrics.uploadDuration, metrics.pipelineUp)
	if store != nil {
		registerSpoolMetrics(registerer, store)
	}
	return metrics
}

func (m *Metrics) ObserveRecord(pipeline, result string) {
	m.recordsRead.WithLabelValues(pipeline, result).Inc()
}

func (m *Metrics) ObserveBatchSpooled() { m.batchesSpooled.Inc() }

func (m *Metrics) ObserveDelivery(result string, duration time.Duration) {
	m.sendAttempts.WithLabelValues(result).Inc()
	m.uploadDuration.WithLabelValues(result).Observe(duration.Seconds())
}

func (m *Metrics) SetPipelineUp(pipeline string, up bool) {
	value := 0.0
	if up {
		value = 1
	}
	m.pipelineUp.WithLabelValues(pipeline).Set(value)
}

func registerSpoolMetrics(registerer prometheus.Registerer, store *spool.WAL) {
	registerer.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "agent", Name: "spool_bytes",
			Help: "Logical bytes retained by pending and quarantined WAL batches.",
		}, func() float64 { return float64(store.Stats().UsedBytes) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "agent", Name: "spool_batches",
			Help: "Batches pending delivery in the WAL.",
		}, func() float64 { return float64(store.Stats().PendingBatches) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "agent", Name: "quarantined_batches",
			Help: "Batches retained in durable local quarantine.",
		}, func() float64 { return float64(store.Stats().QuarantinedBatches) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "agent", Name: "oldest_pending_seconds",
			Help: "Age of the oldest pending batch checkpoint, or zero without pending batches.",
		}, func() float64 {
			oldest := store.Stats().OldestPendingAt
			if oldest.IsZero() {
				return 0
			}
			age := time.Since(oldest).Seconds()
			if age < 0 {
				return 0
			}
			return age
		}),
	)
}
