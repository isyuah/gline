package reliable

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/agent/parser"
	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/agent/spool"
	"github.com/isyuah/gline/internal/protocol/ingestv1"
	"github.com/rs/zerolog"
)

func TestAgentCommitsCheckpointBeforeDeliveryAndPersistsAck(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "app.log")
	walPath := filepath.Join(directory, "agent.wal")
	line := "INFO durable line\n"
	if err := os.WriteFile(logPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := spool.Open(spool.Config{Path: walPath, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	fileSource, err := source.NewDurableFileSource(source.DurableFileOptions{
		Path: logPath, SourceKey: "app", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		checkpoint, ok := store.Checkpoint("app")
		if !ok || checkpoint.OffsetBytes != int64(len(line)) {
			t.Errorf("checkpoint at HTTP boundary = %+v, %t", checkpoint, ok)
		}
		var batch ingestv1.BatchRequest
		if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"batch_id": batch.BatchID, "status": "accepted"})
		select {
		case delivered <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	transport, err := NewHTTPTransport(server.URL, "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(store, transport, DispatcherOptions{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	agent := &Agent{
		Logger: zerolog.Nop(), AgentID: "22222222-2222-4222-8222-222222222222",
		Pipelines: []Pipeline{{
			ID: "33333333-3333-4333-8333-333333333333", ConfigVersion: 1, Source: fileSource,
			Parser: parser.NewStringLineLogParser(), Service: "api", Host: "node-1",
		}},
		Spool: store, Dispatcher: dispatcher,
		Options: AgentOptions{BatchSize: 1, FlushInterval: time.Second},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("agent did not deliver batch")
	}
	deadline := time.Now().Add(time.Second)
	for len(store.Pending()) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Agent.Run() error = %v, want context.Canceled", err)
	}

	recovered, err := spool.Open(spool.Config{Path: walPath, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if len(recovered.Pending()) != 0 {
		t.Fatal("ACKed batch remained pending after restart")
	}
	checkpoint, ok := recovered.Checkpoint("app")
	if !ok || checkpoint.OffsetBytes != int64(len(line)) {
		t.Fatalf("recovered checkpoint = %+v, %t", checkpoint, ok)
	}
}

func TestAgentIsolatesPipelineFailure(t *testing.T) {
	directory := t.TempDir()
	goodPath := filepath.Join(directory, "good.log")
	badPath := filepath.Join(directory, "bad.log")
	if err := os.WriteFile(goodPath, []byte("INFO still running\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	goodSource, err := source.NewDurableFileSource(source.DurableFileOptions{Path: goodPath, SourceKey: "good", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	badSource, err := source.NewDurableFileSource(source.DurableFileOptions{Path: badPath, SourceKey: "bad", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(badPath); err != nil {
		t.Fatal(err)
	}
	store, err := spool.Open(spool.Config{Path: filepath.Join(directory, "agent.wal"), MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var batch ingestv1.BatchRequest
		if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"batch_id": batch.BatchID, "status": "accepted"})
		select {
		case delivered <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	transport, _ := NewHTTPTransport(server.URL, "secret", server.Client())
	dispatcher, _ := NewDispatcher(store, transport, DispatcherOptions{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	agent := &Agent{
		Logger: zerolog.Nop(), AgentID: "22222222-2222-4222-8222-222222222222",
		Pipelines: []Pipeline{
			{ID: "33333333-3333-4333-8333-333333333333", ConfigVersion: 1, Source: badSource, Parser: parser.NewStringLineLogParser(), Service: "bad", Host: "node-1"},
			{ID: "44444444-4444-4444-8444-444444444444", ConfigVersion: 1, Source: goodSource, Parser: parser.NewStringLineLogParser(), Service: "good", Host: "node-1"},
		},
		Spool: store, Dispatcher: dispatcher, Options: AgentOptions{BatchSize: 1, FlushInterval: time.Second},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Agent.Run() returned after one pipeline failed: %v", err)
	case <-delivered:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Agent.Run() error = %v, want context.Canceled", err)
	}
	reports := agent.State.Reports()
	if len(reports) != 2 || reports[0].Status != "error" {
		t.Fatalf("pipeline reports = %+v", reports)
	}
}
