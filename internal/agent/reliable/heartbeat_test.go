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
		_, _ = writer.Write([]byte(`{"agent":{"status":"active"},"control":{"pipelines":[{"id":"pipe-1","desired_status":"paused","config_version":2}]}}`))
	}))
	defer server.Close()

	reporter, err := NewHTTPHeartbeat(server.URL, "agent-secret", "0.1.0", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reporter.Report(t.Context(), []HeartbeatPipeline{{ID: "pipe-1", ConfigVersion: 1, Status: "error", LastError: pointerTo("source failed")}})
	if err != nil {
		t.Fatal(err)
	}
	body := <-received
	if body.Version != "0.1.0" || len(body.Pipelines) != 1 || body.Pipelines[0].ID != "pipe-1" || body.Pipelines[0].ConfigVersion != 1 || body.Pipelines[0].Status != "error" || body.Pipelines[0].LastError == nil {
		t.Fatalf("heartbeat body = %+v", body)
	}
	if len(snapshot.Pipelines) != 1 || snapshot.Pipelines[0].DesiredStatus != "paused" || snapshot.Pipelines[0].ConfigVersion != 2 {
		t.Fatalf("heartbeat control = %+v", snapshot)
	}
}

func TestHTTPHeartbeatRejectsNonSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	reporter, err := NewHTTPHeartbeat(server.URL, "agent-secret", "dev", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.Report(t.Context(), nil); err == nil {
		t.Fatal("Report() error = nil, want HTTP failure")
	}
}

func pointerTo(value string) *string { return &value }
