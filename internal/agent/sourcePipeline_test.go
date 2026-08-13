package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/logentry"
	"github.com/isyuah/testx"
	"github.com/rs/zerolog"
)

type funcParser func(record source.RawRecord) (logentry.LogEntry, error)

func (f funcParser) Parse(raw source.RawRecord) (logentry.LogEntry, error) {
	return f(raw)
}

func TestSourcePipeline_ParserErrorProducesUnknownAndContinues(t *testing.T) {
	parseErr := errors.New("invalid record")
	stopErr := errors.New("source stopped")
	observedAt := time.Unix(123, 0)

	src := &MockSource{Results: make(chan SourceResult, 3)}
	src.Results <- SourceResult{Record: source.RawRecord{
		ObservedAt: observedAt, Content: "unparseable",
	}}
	src.Results <- SourceResult{Record: source.RawRecord{
		ObservedAt: observedAt, Content: "valid",
	}}
	src.Results <- SourceResult{Err: source.FromErrorFatal(stopErr)}

	var logs bytes.Buffer
	pipeline := SourcePipeline{
		Logger: zerolog.New(&logs),
		Source: src,
		Parser: funcParser(func(raw source.RawRecord) (logentry.LogEntry, error) {
			if raw.Content == "unparseable" {
				return logentry.LogEntry{}, parseErr
			}
			return logentry.LogEntry{
				Timestamp: raw.ObservedAt,
				Level:     logentry.LevelInfo,
				Message:   raw.Content,
			}, nil
		}),
		Service: "orders",
		Host:    "host-1",
	}

	entries := make(chan logentry.LogEntry, 2)
	err := pipeline.Start(t.Context(), entries)
	if !errors.Is(err, stopErr) {
		t.Fatalf("unexpected error: %v", err)
	}
	close(entries)
	results := make([]logentry.LogEntry, 0)
	for entry := range entries {
		results = append(results, entry)
	}
	testx.Assert(t, len(results)).Equal(2)
	testx.Assert(t, results).Equal([]logentry.LogEntry{
		{
			Timestamp: observedAt,
			Level:     logentry.LevelUnknown,
			Message:   "unparseable",
			Service:   "orders",
			Host:      "host-1",
		},
		{
			Timestamp: observedAt,
			Level:     logentry.LevelInfo,
			Message:   "valid",
			Service:   "orders",
			Host:      "host-1",
		},
	})
	decoder := json.NewDecoder(&logs)

	var event map[string]any
	if err := decoder.Decode(&event); err != nil {
		t.Fatal(err)
	}
	testx.Assert(t, event["level"]).Equal("warn")
	testx.Assert(t, event["error"]).Equal("invalid record")
}

func TestSourcePipeline_TemporaryError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tempErr := errors.New("temporarily unavailable")

		src := &MockSource{Results: make(chan SourceResult, 3)}
		src.Results <- SourceResult{Err: source.FromErrorTemp(tempErr)}
		src.Results <- SourceResult{Record: source.RawRecord{Content: "recovered"}}
		src.Results <- SourceResult{Err: context.Canceled}

		var logs bytes.Buffer
		pipeline := SourcePipeline{
			Logger: zerolog.New(&logs),
			Source: src,
			Parser: funcParser(func(raw source.RawRecord) (logentry.LogEntry, error) {
				return logentry.LogEntry{
					Timestamp: raw.ObservedAt,
					Level:     logentry.LevelInfo,
					Message:   raw.Content,
				}, nil
			}),
			Service: "orders",
			Host:    "host-1",
		}

		entries := make(chan logentry.LogEntry, 1)
		err := pipeline.Start(t.Context(), entries)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}

		got := <-entries
		testx.Assert(t, got.Message).Equal("recovered")
	})
}

func TestSourcePipeline_FatalErrorStopsPipeline(t *testing.T) {
	fatalCause := errors.New("source is unusable")

	src := &MockSource{Results: make(chan SourceResult, 3)}
	src.Results <- SourceResult{Err: source.FromErrorFatal(fatalCause)}
	src.Results <- SourceResult{
		Record: source.RawRecord{Content: "must not be processed"},
	}
	src.Results <- SourceResult{Err: context.Canceled}

	var logs bytes.Buffer
	parseCalls := 0

	pipeline := SourcePipeline{
		Logger: zerolog.New(&logs),
		Source: src,
		Parser: funcParser(func(raw source.RawRecord) (logentry.LogEntry, error) {
			parseCalls++
			return logentry.LogEntry{
				Level:   logentry.LevelInfo,
				Message: raw.Content,
			}, nil
		}),
		Service: "orders",
		Host:    "host-1",
	}

	entries := make(chan logentry.LogEntry, 1)
	err := pipeline.Start(t.Context(), entries)

	if !errors.Is(err, fatalCause) {
		t.Fatalf("want fatal cause, got %v", err)
	}
	if parseCalls != 0 {
		t.Fatalf("parser called %d times after fatal source error", parseCalls)
	}

	select {
	case entry := <-entries:
		t.Fatalf("unexpected entry after fatal source error: %+v", entry)
	default:
	}

	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	testx.Assert(t, event["level"]).Equal("error")
	testx.Assert(t, event["error"]).Equal("source is unusable")
}
