//go:build integration

package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	agentbuild "github.com/isyuah/gline/internal/agent/build"
	agentconfig "github.com/isyuah/gline/internal/agent/config"
	"github.com/isyuah/gline/internal/agent/reliable"
	"github.com/isyuah/gline/internal/protocol/ingestv1"
	serverconfig "github.com/isyuah/gline/internal/server/config"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/rs/zerolog"
)

const integrationBootstrapToken = "integration-bootstrap-token-at-least-24-bytes"

func TestApplicationHTTPWorkflowAgainstPostgreSQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("GLINE_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("set GLINE_TEST_DATABASE_URL to an expendable PostgreSQL database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	schema := "gline_http_" + integrationRandomHex(t, 8)
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create integration schema: %v", err)
	}

	cfg := integrationConfig(integrationSearchPath(t, dsn, schema))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := New(ctx, cfg, "integration", logger)
	if err != nil {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		t.Fatalf("bootstrap application: %v", err)
	}
	t.Cleanup(func() {
		_ = application.store.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	})
	handler := application.server.Handler

	live := integrationRequest(t, handler, http.MethodGet, "/livez", "", nil)
	integrationExpectStatus(t, live, http.StatusOK)
	ready := integrationRequest(t, handler, http.MethodGet, "/readyz", "", nil)
	integrationExpectStatus(t, ready, http.StatusOK)

	projectResponse := integrationRequest(t, handler, http.MethodPost, "/api/v1/projects", integrationBootstrapToken, map[string]any{
		"slug": "integration-project",
		"name": "Integration Project",
	})
	integrationExpectStatus(t, projectResponse, http.StatusCreated)
	var projectEnvelope struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	integrationDecode(t, projectResponse, &projectEnvelope)
	projectID := projectEnvelope.Project.ID
	integrationRequireUUID(t, "project id", projectID)

	agentResponse := integrationRequest(t, handler, http.MethodPost, "/api/v1/projects/"+projectID+"/agents", integrationBootstrapToken, map[string]any{
		"name": "integration-agent", "hostname": "integration-host", "version": "integration",
	})
	integrationExpectStatus(t, agentResponse, http.StatusCreated)
	var agentEnvelope struct {
		Agent struct {
			ID string `json:"id"`
		} `json:"agent"`
	}
	integrationDecode(t, agentResponse, &agentEnvelope)
	agentID := agentEnvelope.Agent.ID
	integrationRequireUUID(t, "agent id", agentID)

	pipelineResponse := integrationRequest(t, handler, http.MethodPost, "/api/v1/projects/"+projectID+"/pipelines", integrationBootstrapToken, map[string]any{
		"agent_id": agentID,
		"name":     "integration-pipeline",
		"service":  "checkout-api",
		"config":   map[string]any{"source": "integration"},
	})
	integrationExpectStatus(t, pipelineResponse, http.StatusCreated)
	var pipelineEnvelope struct {
		Pipeline struct {
			ID string `json:"id"`
		} `json:"pipeline"`
	}
	integrationDecode(t, pipelineResponse, &pipelineEnvelope)
	pipelineID := pipelineEnvelope.Pipeline.ID
	integrationRequireUUID(t, "pipeline id", pipelineID)

	keyResponse := integrationRequest(t, handler, http.MethodPost, "/api/v1/projects/"+projectID+"/keys", integrationBootstrapToken, map[string]any{
		"name":     "integration-agent-key",
		"agent_id": agentID,
		"scopes":   []string{"ingest", "query", "project:read"},
	})
	integrationExpectStatus(t, keyResponse, http.StatusCreated)
	var keyEnvelope struct {
		Key struct {
			Secret string `json:"secret"`
		} `json:"key"`
	}
	integrationDecode(t, keyResponse, &keyEnvelope)
	apiKey := keyEnvelope.Key.Secret
	if !strings.HasPrefix(apiKey, "glk_") {
		t.Fatalf("created key did not contain the one-time secret")
	}

	now := time.Now().UTC().Truncate(time.Second)
	batchID := "44444444-4444-4444-8444-444444444444"
	batch := ingestv1.BatchRequest{
		ProtocolVersion: ingestv1.Version,
		BatchID:         batchID,
		AgentID:         agentID,
		PipelineID:      pipelineID,
		Sequence:        7,
		SentAt:          now,
		Entries: []ingestv1.Entry{{
			Sequence: 0, ObservedAt: now, Level: "info", Service: "checkout-api",
			Host: "integration-host", Message: "order accepted",
			Attributes: map[string]any{"region": "local", "attempt": 1},
		}},
	}
	accepted := integrationRequest(t, handler, http.MethodPost, "/api/v1/batches", apiKey, batch)
	integrationExpectStatus(t, accepted, http.StatusOK)
	var acceptedEnvelope struct {
		BatchID         string `json:"batch_id"`
		Status          string `json:"status"`
		AcceptedEntries int    `json:"accepted_entries"`
	}
	integrationDecode(t, accepted, &acceptedEnvelope)
	if acceptedEnvelope.BatchID != batchID || acceptedEnvelope.Status != "accepted" || acceptedEnvelope.AcceptedEntries != 1 {
		t.Fatalf("unexpected accepted response: %+v", acceptedEnvelope)
	}

	duplicate := integrationRequest(t, handler, http.MethodPost, "/api/v1/batches", apiKey, batch)
	integrationExpectStatus(t, duplicate, http.StatusOK)
	var duplicateEnvelope struct {
		Status          string `json:"status"`
		AcceptedEntries int    `json:"accepted_entries"`
	}
	integrationDecode(t, duplicate, &duplicateEnvelope)
	if duplicateEnvelope.Status != "duplicate" || duplicateEnvelope.AcceptedEntries != 1 {
		t.Fatalf("unexpected duplicate response: %+v", duplicateEnvelope)
	}

	from := url.QueryEscape(now.Add(-time.Minute).Format(time.RFC3339Nano))
	to := url.QueryEscape(now.Add(time.Minute).Format(time.RFC3339Nano))
	entriesResponse := integrationRequest(t, handler, http.MethodGet, "/api/v1/entries?from="+from+"&to="+to+"&service=checkout-api&level=info&limit=10", apiKey, nil)
	integrationExpectStatus(t, entriesResponse, http.StatusOK)
	var entriesEnvelope struct {
		Entries []struct {
			BatchID string `json:"batch_id"`
			Message string `json:"message"`
			Level   string `json:"level"`
		} `json:"entries"`
	}
	integrationDecode(t, entriesResponse, &entriesEnvelope)
	if len(entriesEnvelope.Entries) != 1 || entriesEnvelope.Entries[0].BatchID != batchID ||
		entriesEnvelope.Entries[0].Message != "order accepted" || entriesEnvelope.Entries[0].Level != "INFO" {
		t.Fatalf("unexpected query result: %+v", entriesEnvelope.Entries)
	}

	usageResponse := integrationRequest(t, handler, http.MethodGet, "/api/v1/projects/"+projectID+"/usage?from="+from+"&to="+to, apiKey, nil)
	integrationExpectStatus(t, usageResponse, http.StatusOK)
	var usageEnvelope struct {
		Buckets []struct {
			Entries int64 `json:"entries"`
		} `json:"buckets"`
	}
	integrationDecode(t, usageResponse, &usageEnvelope)
	var ingestedEntries int64
	for _, bucket := range usageEnvelope.Buckets {
		ingestedEntries += bucket.Entries
	}
	if ingestedEntries != 1 {
		t.Fatalf("usage counted %d entries, want 1 after idempotent replay", ingestedEntries)
	}

	metrics := integrationRequest(t, handler, http.MethodGet, "/metrics", "", nil)
	integrationExpectStatus(t, metrics, http.StatusOK)
	metricsBody := metrics.Body.String()
	for _, family := range []string{
		"gline_server_ingest_batches_total", "gline_server_query_requests_total",
		"gline_server_db_pool_open_connections", "gline_server_http_requests_total",
		"gline_server_admission_requests_total",
	} {
		if !strings.Contains(metricsBody, family) {
			t.Fatalf("metrics output is missing %s", family)
		}
	}
	if strings.Contains(metricsBody, projectID) || strings.Contains(metricsBody, batchID) {
		t.Fatal("metrics output contains a project or batch identifier")
	}
}

func TestReliableAgentRecoversFileBacklogAfterServerOutage(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("GLINE_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("set GLINE_TEST_DATABASE_URL to an expendable PostgreSQL database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	schema := "gline_agent_" + integrationRandomHex(t, 8)
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create integration schema: %v", err)
	}
	application, err := New(ctx, integrationConfig(integrationSearchPath(t, dsn, schema)), "integration", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		t.Fatalf("bootstrap application: %v", err)
	}
	t.Cleanup(func() {
		_ = application.store.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	})

	projectResponse := integrationRequest(t, application.server.Handler, http.MethodPost, "/api/v1/projects", integrationBootstrapToken, map[string]any{
		"slug": "agent-recovery", "name": "Agent Recovery",
	})
	integrationExpectStatus(t, projectResponse, http.StatusCreated)
	var projectEnvelope struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	integrationDecode(t, projectResponse, &projectEnvelope)
	projectID := projectEnvelope.Project.ID

	agentResponse := integrationRequest(t, application.server.Handler, http.MethodPost, "/api/v1/projects/"+projectID+"/agents", integrationBootstrapToken, map[string]any{
		"name": "recovery-agent", "hostname": "integration-host", "version": "integration",
	})
	integrationExpectStatus(t, agentResponse, http.StatusCreated)
	var agentEnvelope struct {
		Agent struct {
			ID string `json:"id"`
		} `json:"agent"`
	}
	integrationDecode(t, agentResponse, &agentEnvelope)
	agentID := agentEnvelope.Agent.ID

	pipelineResponse := integrationRequest(t, application.server.Handler, http.MethodPost, "/api/v1/projects/"+projectID+"/pipelines", integrationBootstrapToken, map[string]any{
		"agent_id": agentID, "name": "recovery-file", "service": "recovery-api", "config": map[string]any{"source": "file"},
	})
	integrationExpectStatus(t, pipelineResponse, http.StatusCreated)
	var pipelineEnvelope struct {
		Pipeline struct {
			ID string `json:"id"`
		} `json:"pipeline"`
	}
	integrationDecode(t, pipelineResponse, &pipelineEnvelope)
	pipelineID := pipelineEnvelope.Pipeline.ID

	keyResponse := integrationRequest(t, application.server.Handler, http.MethodPost, "/api/v1/projects/"+projectID+"/keys", integrationBootstrapToken, map[string]any{
		"name": "recovery-agent-key", "agent_id": agentID, "scopes": []string{"ingest", "query", "agent:write"},
	})
	integrationExpectStatus(t, keyResponse, http.StatusCreated)
	var keyEnvelope struct {
		Key struct {
			Secret string `json:"secret"`
		} `json:"key"`
	}
	integrationDecode(t, keyResponse, &keyEnvelope)

	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if unavailable.Load() && strings.HasPrefix(request.URL.Path, "/api/v1/") {
			http.Error(writer, "temporary outage", http.StatusServiceUnavailable)
			return
		}
		application.server.Handler.ServeHTTP(writer, request)
	}))
	defer server.Close()

	directory := t.TempDir()
	logPath := filepath.Join(directory, "recovery.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceParams, _ := yaml.Marshal(agentconfig.FileSourceParams{Path: logPath, SourceKey: "recovery-log"})
	senderParams, _ := yaml.Marshal(agentconfig.ReliableSenderParams{
		SpoolPath: filepath.Join(directory, "agent.wal"), MaxSpoolBytes: 8 << 20, MaxRecordBytes: 1 << 20,
		BatchSize: 1, FlushInterval: 20 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond,
		Retry: agentconfig.ReliableRetryParams{BaseDelay: 20 * time.Millisecond, MaxDelay: 100 * time.Millisecond},
	})
	destinationParams, _ := yaml.Marshal(agentconfig.GlineDestinationParams{
		URL: server.URL + "/api/v1/batches", Token: keyEnvelope.Key.Secret,
	})
	runtime, err := agentbuild.ReliableAgent(agentconfig.GlineAgentConfig{
		Version: agentconfig.CurrentVersion,
		Agent:   agentconfig.AgentConfig{ID: agentID, Version: "integration"},
		Pipelines: []agentconfig.PipelineConfig{{
			ID: pipelineID, ConfigVersion: 1, Service: "recovery-api", Host: "integration-host",
			Source: agentconfig.PipelineSourceConfig{Type: "file", Params: sourceParams},
			Parser: agentconfig.PipelineParserConfig{Type: "string_line", Params: yaml.RawMessage(`{}`)},
		}},
		Sender: agentconfig.SenderConfig{
			Type: "reliable", Params: senderParams,
			Destination: agentconfig.SenderDestinationConfig{Type: "gline", Params: destinationParams},
		},
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	reliableAgent, ok := runtime.(*reliable.Agent)
	if !ok {
		t.Fatalf("runtime type = %T, want *reliable.Agent", runtime)
	}
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() { agentDone <- reliableAgent.Run(agentCtx) }()

	integrationAppendLine(t, logPath, "INFO before outage\n")
	integrationWaitForMessages(t, application.server.Handler, keyEnvelope.Key.Secret, "recovery-api", []string{"before outage"})

	unavailable.Store(true)
	integrationAppendLine(t, logPath, "ERROR during outage\n")
	integrationEventually(t, 10*time.Second, func() bool {
		return reliableAgent.Spool.Stats().PendingBatches > 0
	}, "Agent WAL did not retain a batch during the outage")
	unavailable.Store(false)
	integrationWaitForMessages(t, application.server.Handler, keyEnvelope.Key.Secret, "recovery-api", []string{"before outage", "during outage"})
	integrationEventually(t, 10*time.Second, func() bool {
		return reliableAgent.Spool.Stats().PendingBatches == 0
	}, "Agent WAL did not clear after Server recovery")

	stopAgent()
	if err := <-agentDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Agent.Run() error = %v, want context.Canceled", err)
	}
}

func integrationConfig(databaseURL string) serverconfig.Config {
	return serverconfig.Config{
		HTTPAddr:                "127.0.0.1:0",
		DatabaseURL:             databaseURL,
		BootstrapToken:          integrationBootstrapToken,
		APIKeyPepper:            "integration-api-key-pepper-at-least-24-bytes",
		ShutdownTimeout:         5 * time.Second,
		DatabaseTimeout:         5 * time.Second,
		MaxRequestBytes:         8 << 20,
		IngestRequestsPerMinute: 600,
		IngestEntriesPerMinute:  120_000,
		IngestBytesPerMinute:    256 << 20,
		IngestMaxInflight:       16,
		QueryMaxRange:           7 * 24 * time.Hour,
		QueryTimeout:            5 * time.Second,
		QueryMaxPageSize:        500,
		QueryConcurrency:        8,
		MaintenanceEvery:        time.Minute,
		AgentStaleAfter:         2 * time.Minute,
		RetentionBatch:          100,
	}
}

func integrationAppendLine(t *testing.T, path, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(line); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func integrationWaitForMessages(t *testing.T, handler http.Handler, token, service string, expected []string) {
	t.Helper()
	integrationEventually(t, 10*time.Second, func() bool {
		now := time.Now().UTC()
		from := url.QueryEscape(now.Add(-time.Minute).Format(time.RFC3339Nano))
		to := url.QueryEscape(now.Add(time.Minute).Format(time.RFC3339Nano))
		response := integrationRequest(t, handler, http.MethodGet, "/api/v1/entries?from="+from+"&to="+to+"&service="+url.QueryEscape(service)+"&limit=100", token, nil)
		if response.Code != http.StatusOK {
			return false
		}
		var envelope struct {
			Entries []struct {
				Message string `json:"message"`
			} `json:"entries"`
		}
		integrationDecode(t, response, &envelope)
		seen := make(map[string]bool, len(envelope.Entries))
		for _, entry := range envelope.Entries {
			seen[entry.Message] = true
		}
		for _, message := range expected {
			if !seen[message] {
				return false
			}
		}
		return true
	}, "query API did not expose all expected Agent messages")
}

func integrationEventually(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(message)
}

func integrationRequest(t *testing.T, handler http.Handler, method, target, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s request: %v", method, target, err)
		}
		payload = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, payload)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("X-Request-ID", "integration-request")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func integrationExpectStatus(t *testing.T, response *httptest.ResponseRecorder, expected int) {
	t.Helper()
	if response.Code != expected {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, expected, response.Body.String())
	}
}

func integrationDecode(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
}

func integrationRequireUUID(t *testing.T, name, value string) {
	t.Helper()
	if len(value) != 36 || strings.Count(value, "-") != 4 {
		t.Fatalf("%s = %q, want UUID", name, value)
	}
}

func integrationRandomHex(t *testing.T, byteCount int) string {
	t.Helper()
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buffer)
}

func integrationSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatalf("GLINE_TEST_DATABASE_URL must be a PostgreSQL URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
