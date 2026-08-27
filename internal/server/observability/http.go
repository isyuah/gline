package observability

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
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
			Namespace: "gline", Subsystem: "server_http", Name: "requests_total",
			Help: "Total HTTP requests by route template, method and status class.",
		}, []string{"route", "method", "status_class"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gline", Subsystem: "server_http", Name: "request_duration_seconds",
			Help: "HTTP request duration by route template and method.", Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "gline", Subsystem: "server_http", Name: "in_flight_requests",
			Help: "Current number of in-flight HTTP requests.",
		}),
	}
	registerer.MustRegister(metrics.requests, metrics.duration, metrics.inFlight)
	return metrics
}

func (m *HTTPMetrics) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		m.inFlight.Inc()
		defer m.inFlight.Dec()
		started := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		statusClass := strconv.Itoa(c.Writer.Status()/100) + "xx"
		m.requests.WithLabelValues(route, c.Request.Method, statusClass).Inc()
		m.duration.WithLabelValues(route, c.Request.Method).Observe(time.Since(started).Seconds())
	}
}
