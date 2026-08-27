package reliable

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPHeartbeatReportsPipelineStateAndAuthorization(t *testing.T) {
	type heartbeatBody struct {
		Version   string              `json:"version"`
		Pipelines []HeartbeatPipeline `json:"pipelines"`
	}
	received := make(chan heartbeatBody, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer agent-secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body heartbeatBody
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid json", http.StatusBadRequest)
			return
		}
		received <- body
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"agent":{"status":"active"}}`))
	}))
	defer server.Close()

	reporter, err := NewHTTPHeartbeat(server.URL, "agent-secret", "0.1.0", []string{"pipe-1"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	body := <-received
	if body.Version != "0.1.0" || len(body.Pipelines) != 1 || body.Pipelines[0].ID != "pipe-1" || body.Pipelines[0].Status != "running" {
		t.Fatalf("heartbeat body = %+v", body)
	}
}

func TestHTTPHeartbeatRejectsNonSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	reporter, err := NewHTTPHeartbeat(server.URL, "agent-secret", "dev", nil, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Report(t.Context()); err == nil {
		t.Fatal("Report() error = nil, want HTTP failure")
	}
}
