package reliable

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/agent/spool"
)

func TestHTTPTransportClassifiesResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       ResultClass
	}{
		{"accepted", 200, `{"batch_id":"one","status":"accepted"}`, ResultAccepted},
		{"duplicate", 200, `{"batch_id":"one","status":"duplicate"}`, ResultDuplicate},
		{"bad request", 400, `{"error":{"code":"invalid"}}`, ResultQuarantine},
		{"unauthorized", 401, `{}`, ResultTerminal},
		{"forbidden", 403, `{}`, ResultTerminal},
		{"conflict", 409, `{"error":{"code":"conflict"}}`, ResultQuarantine},
		{"rate limited", 429, `{}`, ResultRetryable},
		{"server failure", 503, `{}`, ResultRetryable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer secret" {
					t.Error("missing bearer token")
				}
				writer.WriteHeader(test.statusCode)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			transport, err := NewHTTPTransport(server.URL, "secret", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := transport.Send(t.Context(), []byte(`{"batch_id":"one"}`))
			if err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			if result.Class != test.want {
				t.Fatalf("Send() class = %v, want %v", result.Class, test.want)
			}
		})
	}
}

func TestHTTPTransportRetriesMismatchedAcknowledgementWithoutDeletingBatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"batch_id":"another-batch","status":"accepted"}`))
	}))
	defer server.Close()
	transport, err := NewHTTPTransport(server.URL, "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := transport.Send(t.Context(), []byte(`{"batch_id":"expected-batch"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Class != ResultRetryable || result.Code != "invalid_ack_batch_id" {
		t.Fatalf("Send() result=%+v", result)
	}
}

func TestHTTPTransportTreatsNetworkFailureAsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := server.Client()
	endpoint := server.URL
	server.Close()
	transport, err := NewHTTPTransport(endpoint, "secret", client)
	if err != nil {
		t.Fatal(err)
	}
	result, err := transport.Send(t.Context(), []byte(`{"batch_id":"network-test"}`))
	if err == nil || result.Class != ResultRetryable {
		t.Fatalf("Send() = %+v, %v; want retryable network error", result, err)
	}
}

func TestHTTPTransportHonorsRetryAfterOnRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "7")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	transport, err := NewHTTPTransport(server.URL, "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := transport.Send(t.Context(), []byte(`{"batch_id":"rate-limited"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Class != ResultRetryable || result.RetryAfter != 7*time.Second {
		t.Fatalf("Send() result = %+v", result)
	}
}

func TestDispatcherRetriesSamePayloadThenAcknowledgesDuplicate(t *testing.T) {
	store := openDispatcherSpool(t)
	payload := []byte(`{"batch_id":"batch-1","entries":[]}`)
	if err := store.Commit(t.Context(), spool.Commit{
		BatchID: "batch-1", Payload: payload,
		Checkpoint: source.Checkpoint{SourceKey: "app", FileIdentity: "file-1", OffsetBytes: 10, ObservedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{results: []SendResult{{Class: ResultRetryable}, {Class: ResultDuplicate}}}
	dispatcher, err := NewDispatcher(store, transport, DispatcherOptions{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for len(store.Pending()) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.payloads) != 2 || string(transport.payloads[0]) != string(payload) || string(transport.payloads[1]) != string(payload) {
		t.Fatalf("transport payloads = %q, want same payload twice", transport.payloads)
	}
}

func TestDispatcherQuarantinesPermanentBatchAndContinues(t *testing.T) {
	store := openDispatcherSpool(t)
	if err := store.Commit(t.Context(), spool.Commit{
		BatchID: "batch-1", Payload: []byte(`{"batch_id":"batch-1"}`),
		Checkpoint: source.Checkpoint{SourceKey: "app", FileIdentity: "file-1", OffsetBytes: 10, ObservedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(store, &recordingTransport{results: []SendResult{{Class: ResultQuarantine, StatusCode: 409, Code: "conflict"}}}, DispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for len(store.Quarantined()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	quarantined := store.Quarantined()
	if len(store.Pending()) != 0 || len(quarantined) != 1 || quarantined[0].Commit.BatchID != "batch-1" || quarantined[0].HTTPCode != 409 {
		t.Fatalf("pending=%+v quarantined=%+v", store.Pending(), quarantined)
	}
}

func TestDispatcherStopsOnSystemicTerminalFailureWithoutRemovingBatch(t *testing.T) {
	store := openDispatcherSpool(t)
	if err := store.Commit(t.Context(), spool.Commit{
		BatchID: "batch-1", Payload: []byte(`{"batch_id":"batch-1"}`),
		Checkpoint: source.Checkpoint{SourceKey: "app", FileIdentity: "file-1", OffsetBytes: 10, ObservedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(store, &recordingTransport{results: []SendResult{{Class: ResultTerminal, StatusCode: 401, Code: "unauthorized"}}}, DispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = dispatcher.Run(t.Context())
	var terminal *TerminalError
	if !errors.As(err, &terminal) || terminal.HTTPCode != 401 || len(store.Pending()) != 1 || len(store.Quarantined()) != 0 {
		t.Fatalf("Run() error=%v pending=%+v quarantined=%+v", err, store.Pending(), store.Quarantined())
	}
}

type recordingTransport struct {
	mu       sync.Mutex
	results  []SendResult
	payloads [][]byte
}

func (t *recordingTransport) Send(_ context.Context, payload []byte) (SendResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.payloads = append(t.payloads, append([]byte(nil), payload...))
	index := len(t.payloads) - 1
	if index >= len(t.results) {
		return SendResult{}, fmt.Errorf("unexpected transport call %d", index)
	}
	return t.results[index], nil
}

func openDispatcherSpool(t *testing.T) *spool.WAL {
	t.Helper()
	store, err := spool.Open(spool.Config{Path: filepath.Join(t.TempDir(), "agent.wal"), MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
