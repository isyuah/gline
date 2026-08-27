package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type HTTPMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

func NewHTTPMetrics(registerer prometheus.Registerer) *HTTPMetrics {
	metrics := &HTTPMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "http", Name: "requests_total",
			Help: "Total HTTP requests by method and status class.",
		}, []string{"method", "status_class"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gline", Subsystem: "http", Name: "request_duration_seconds",
			Help: "HTTP request duration by method.", Buckets: prometheus.DefBuckets,
		}, []string{"method"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "http", Name: "in_flight_requests",
			Help: "Current number of in-flight HTTP requests.",
		}),
	}
	registerer.MustRegister(metrics.requests, metrics.duration, metrics.inFlight)
	return metrics
}

func (m *HTTPMetrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		m.inFlight.Inc()
		defer m.inFlight.Dec()
		started := time.Now()
		wrapped := &statusWriter{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(wrapped, request)
		statusClass := strconv.Itoa(wrapped.status/100) + "xx"
		m.requests.WithLabelValues(request.Method, statusClass).Inc()
		m.duration.WithLabelValues(request.Method).Observe(time.Since(started).Seconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
