package observability

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestHTTPMetricsUseRouteTemplateInsteadOfResourceID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := prometheus.NewRegistry()
	metrics := NewHTTPMetrics(registry)
	router := gin.New()
	router.Use(metrics.Middleware())
	router.GET("/projects/:projectID", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodGet, "/projects/11111111-1111-1111-1111-111111111111", nil)
	router.ServeHTTP(httptest.NewRecorder(), request)
	family := gatherFamily(t, registry, "gline_server_http_requests_total")
	if len(family.Metric) != 1 {
		t.Fatalf("request metric count = %d, want 1", len(family.Metric))
	}
	labels := family.Metric[0].Label
	for _, label := range labels {
		if label.GetName() == "route" && label.GetValue() != "/projects/:projectID" {
			t.Fatalf("route label = %q", label.GetValue())
		}
		if label.GetValue() == "11111111-1111-1111-1111-111111111111" {
			t.Fatal("resource ID leaked into metric labels")
		}
	}
}

func TestServerMetricContractIncludesBusinessJobAndPoolSignals(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewServerMetrics(registry, func() sql.DBStats { return sql.DBStats{OpenConnections: 3, InUse: 2} })
	metrics.ObserveIngest("accepted", 2, 256, time.Millisecond)
	metrics.ObserveQuery("success", "time_only", 2, time.Millisecond)
	metrics.ObserveBackgroundJob("retention_age", "success", 2, time.Millisecond)
	metrics.ObserveAdmission("rejected", "project_inflight")
	metrics.AddAdmissionInflight(1)
	metrics.AddAdmissionInflight(-1)

	for _, name := range []string{
		"gline_server_ingest_batches_total", "gline_server_query_requests_total",
		"gline_server_background_job_runs_total", "gline_server_db_pool_in_use_connections",
		"gline_server_admission_requests_total", "gline_server_admission_inflight",
	} {
		_ = gatherFamily(t, registry, name)
	}
}

func gatherFamily(t *testing.T, registry *prometheus.Registry, name string) *dto.MetricFamily {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family
		}
	}
	t.Fatalf("metric family %s was not gathered", name)
	return nil
}
