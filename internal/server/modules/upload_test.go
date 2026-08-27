package modules

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/isyuah/gline/internal/logentry"
)

type recordingSink struct {
	entries []logentry.LogEntry
	err     error
	calls   int
}

func (s *recordingSink) Accept(_ context.Context, entries []logentry.LogEntry) error {
	s.calls++
	s.entries = append([]logentry.LogEntry(nil), entries...)
	return s.err
}

func newUploadRouter(entrySink *recordingSink) *gin.Engine {
	router := gin.New()
	handler := &EntriesUploadHandler{Sink: entrySink}
	router.POST("/entries/upload", handler.HandleUploadEntries)
	return router
}

func TestEntriesUploadHandlerAcceptsEntries(t *testing.T) {
	entrySink := &recordingSink{}
	router := newUploadRouter(entrySink)
	body := `{
		"entries": [{
			"timestamp": "2026-08-13T12:00:00Z",
			"level": "INFO",
			"host": "host-1",
			"message": "hello",
			"service": "orders"
		}]
	}`

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/entries/upload", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if entrySink.calls != 1 {
		t.Fatalf("Accept() calls = %d, want 1", entrySink.calls)
	}
	if len(entrySink.entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entrySink.entries))
	}
	got := entrySink.entries[0]
	if got.Level != logentry.LevelInfo || got.Host != "host-1" || got.Message != "hello" || got.Service != "orders" {
		t.Fatalf("entry = %+v, want uploaded entry", got)
	}
}

func TestEntriesUploadHandlerRejectsInvalidJSON(t *testing.T) {
	entrySink := &recordingSink{}
	router := newUploadRouter(entrySink)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/entries/upload", strings.NewReader(`{"entries": [`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if entrySink.calls != 0 {
		t.Fatalf("Accept() calls = %d, want 0", entrySink.calls)
	}
}

func TestEntriesUploadHandlerReturnsErrorWhenSinkFails(t *testing.T) {
	entrySink := &recordingSink{err: errors.New("sink unavailable")}
	router := newUploadRouter(entrySink)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/entries/upload", strings.NewReader(`{"entries": []}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if entrySink.calls != 1 {
		t.Fatalf("Accept() calls = %d, want 1", entrySink.calls)
	}
}
