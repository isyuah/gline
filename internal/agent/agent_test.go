package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/logentry"
	"github.com/rs/zerolog"
)

type SourceResult struct {
	Record source.RawRecord
	Err    error
}

type MockSource struct {
	Results chan SourceResult
}

func (m *MockSource) NextRecord(
	ctx context.Context,
) (source.RawRecord, error) {
	select {
	case <-ctx.Done():
		return source.RawRecord{}, ctx.Err()

	case result := <-m.Results:
		return result.Record, result.Err
	}
}

type MockParser struct{}

func (m *MockParser) Parse(
	raw source.RawRecord,
) (logentry.LogEntry, error) {
	return logentry.LogEntry{
		Message: raw.Content,
	}, nil
}

type panicParser struct {
	Value any
}

func (p panicParser) Parse(source.RawRecord) (logentry.LogEntry, error) {
	panic(p.Value)
}

type DrainSender struct {
	Entries []logentry.LogEntry
}

func (s *DrainSender) Run(
	ctx context.Context,
	source <-chan logentry.LogEntry,
) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case entry, ok := <-source:
			if !ok {
				return nil
			}

			s.Entries = append(s.Entries, entry)
		}
	}
}

func TestAgent_PipelineErrorDoesNotStopOtherPipelines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		pipelineErr := errors.New("pipeline A failed")
		srcA := &MockSource{Results: make(chan SourceResult)}
		srcB := &MockSource{Results: make(chan SourceResult)}
		sender := &DrainSender{}

		var logs bytes.Buffer
		a := Agent{
			Logger: zerolog.New(&logs),
			Pipelines: []SourcePipeline{
				{
					Source: srcA, Parser: &MockParser{},
					Service: "service-a", Host: "host-a",
				},
				{
					Source: srcB, Parser: &MockParser{},
					Service: "service-b", Host: "host-b",
				},
			},
			Sender: sender,
		}

		done := make(chan error, 1)
		go func() {
			done <- a.Run(ctx)
		}()

		// 等两个 Pipeline 都阻塞在 NextRecord。
		synctest.Wait()

		srcA.Results <- SourceResult{
			Record: source.RawRecord{Content: "before-failure"},
		}
		synctest.Wait()

		srcA.Results <- SourceResult{
			Err: source.FromErrorFatal(pipelineErr),
		}
		// 返回后 A 已退出，其余 goroutine 再次稳定阻塞。
		synctest.Wait()

		// 这条记录明确发生在 A 退出之后。
		srcB.Results <- SourceResult{
			Record: source.RawRecord{Content: "after-a-failed"},
		}
		synctest.Wait()

		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Agent error: %v", err)
		}

		wantEntries := []logentry.LogEntry{
			{
				Message: "before-failure",
				Service: "service-a",
				Host:    "host-a",
			},
			{
				Message: "after-a-failed",
				Service: "service-b",
				Host:    "host-b",
			},
		}
		if diff := cmp.Diff(wantEntries, sender.Entries); diff != "" {
			t.Fatalf("sender entries mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestAgent_PipelinePanicDoesNotStopOtherPipelines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		srcA := &MockSource{Results: make(chan SourceResult)}
		srcB := &MockSource{Results: make(chan SourceResult)}
		sender := &DrainSender{}

		var logs bytes.Buffer
		a := Agent{
			Logger: zerolog.New(&logs),
			Pipelines: []SourcePipeline{
				{
					Source: srcA, Parser: panicParser{Value: "parser exploded"},
					Service: "service-a", Host: "host-a",
				},
				{
					Source: srcB, Parser: &MockParser{},
					Service: "service-b", Host: "host-b",
				},
			},
			Sender: sender,
		}

		done := make(chan error, 1)
		go func() {
			done <- a.Run(ctx)
		}()

		synctest.Wait()

		srcA.Results <- SourceResult{
			Record: source.RawRecord{Content: "triggers panic"},
		}
		synctest.Wait()

		srcB.Results <- SourceResult{
			Record: source.RawRecord{Content: "still running"},
		}
		synctest.Wait()

		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Agent error: %v", err)
		}

		wantEntries := []logentry.LogEntry{
			{
				Message: "still running",
				Service: "service-b",
				Host:    "host-b",
			},
		}
		if diff := cmp.Diff(wantEntries, sender.Entries); diff != "" {
			t.Fatalf("sender entries mismatch (-want +got):\n%s", diff)
		}

		var event map[string]any
		if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event["level"] != "fatal" {
			t.Fatalf("level = %v, want fatal", event["level"])
		}
		if event["panic"] != "parser exploded" {
			t.Fatalf("panic = %v, want parser exploded", event["panic"])
		}
		if event["service"] != "service-a" {
			t.Fatalf("service = %v, want service-a", event["service"])
		}

		stack, ok := event["stack"].(string)
		if !ok {
			t.Fatalf("stack is not a string: %T", event["stack"])
		}
		if !strings.Contains(stack, "panicParser.Parse") {
			t.Fatalf("stack does not contain panic origin: %s", stack)
		}
		if event["component"] != "pipeline" {
			t.Fatalf("component = %v, want pipeline", event["component"])
		}
		if event["host"] != "host-a" {
			t.Fatalf("host = %v, want host-a", event["host"])
		}
	})
}

type ErrorAfterFirstSender struct {
	Entries []logentry.LogEntry
	Err     error
}

func (s *ErrorAfterFirstSender) Run(
	ctx context.Context,
	source <-chan logentry.LogEntry,
) error {
	select {
	case <-ctx.Done():
		return ctx.Err()

	case entry, ok := <-source:
		if !ok {
			return nil
		}

		s.Entries = append(s.Entries, entry)

		return s.Err
	}
}

func TestAgent_SenderErrorStopsPipelines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		senderErr := errors.New("sender error")

		src := &MockSource{
			Results: make(chan SourceResult, 1),
		}

		src.Results <- SourceResult{
			Record: source.RawRecord{
				Content: "hello",
			},
		}

		sender := &ErrorAfterFirstSender{
			Err: senderErr,
		}

		var logs bytes.Buffer
		a := Agent{
			Logger: zerolog.New(&logs),
			Pipelines: []SourcePipeline{
				{
					Source:  src,
					Parser:  &MockParser{},
					Service: "test-service",
					Host:    "test-host",
				},
			},
			Sender: sender,
		}

		err := a.Run(t.Context())

		// Agent 最终应该返回 Sender 的错误
		if !errors.Is(err, senderErr) {
			t.Fatalf("Agent.Run() error = %v, want %v", err, senderErr)
		}

		// Sender 确实收到过第一条日志
		wantEntries := []logentry.LogEntry{
			{
				Message: "hello",
				Service: "test-service",
				Host:    "test-host",
			},
		}
		if diff := cmp.Diff(wantEntries, sender.Entries); diff != "" {
			t.Fatalf("sender entries mismatch (-want +got):\n%s", diff)
		}

		var event map[string]any
		if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event["level"] != "error" {
			t.Fatalf("level = %v, want error", event["level"])
		}
		if event["component"] != "sender" {
			t.Fatalf("component = %v, want sender", event["component"])
		}
		if event["error"] != "sender error" {
			t.Fatalf("error = %v, want sender error", event["error"])
		}
	})
}

func TestAgent_ContextCancelDrainsEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		src := &MockSource{
			Results: make(chan SourceResult, 3),
		}

		src.Results <- SourceResult{
			Record: source.RawRecord{
				Content: "one",
			},
		}
		src.Results <- SourceResult{
			Record: source.RawRecord{
				Content: "two",
			},
		}
		src.Results <- SourceResult{
			Record: source.RawRecord{
				Content: "three",
			},
		}

		sender := &DrainSender{}

		a := Agent{
			Pipelines: []SourcePipeline{
				{
					Source:  src,
					Parser:  &MockParser{},
					Service: "test-service",
					Host:    "test-host",
				},
			},
			Sender: sender,
		}

		done := make(chan error, 1)

		go func() {
			done <- a.Run(ctx)
		}()

		// 等前三条日志全部流过 Pipeline/Sender，
		// 此时 Pipeline 会阻塞在第四次 NextRecord。
		synctest.Wait()

		cancel()

		err := <-done

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Agent.Run() error = %v, want %v", err, context.Canceled)
		}

		wantEntries := []logentry.LogEntry{
			{
				Message: "one",
				Service: "test-service",
				Host:    "test-host",
			},
			{
				Message: "two",
				Service: "test-service",
				Host:    "test-host",
			},
			{
				Message: "three",
				Service: "test-service",
				Host:    "test-host",
			},
		}
		if diff := cmp.Diff(wantEntries, sender.Entries); diff != "" {
			t.Fatalf("sender entries mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestAgent_MultiplePipelines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		srcA := &MockSource{
			Results: make(chan SourceResult, 1),
		}

		srcB := &MockSource{
			Results: make(chan SourceResult, 1),
		}

		srcA.Results <- SourceResult{
			Record: source.RawRecord{
				Content: "from-a",
			},
		}

		srcB.Results <- SourceResult{
			Record: source.RawRecord{
				Content: "from-b",
			},
		}

		sender := &DrainSender{}

		a := Agent{
			Pipelines: []SourcePipeline{
				{
					Source:  srcA,
					Parser:  &MockParser{},
					Service: "service-a",
					Host:    "host-a",
				},
				{
					Source:  srcB,
					Parser:  &MockParser{},
					Service: "service-b",
					Host:    "host-b",
				},
			},
			Sender: sender,
		}

		done := make(chan error, 1)

		go func() {
			done <- a.Run(ctx)
		}()

		synctest.Wait()

		cancel()

		err := <-done

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Agent.Run() error = %v, want %v", err, context.Canceled)
		}
		wantEntries := []logentry.LogEntry{
			{
				Message: "from-a",
				Host:    "host-a",
				Service: "service-a",
			},
			{
				Message: "from-b",
				Host:    "host-b",
				Service: "service-b",
			},
		}
		if diff := cmp.Diff(wantEntries, sender.Entries,
			cmpopts.SortSlices(
				func(a, b logentry.LogEntry) bool {
					return a.Message < b.Message
				},
			),
		); diff != "" {
			t.Fatalf("sender entries mismatch (-want +got):\n%s", diff)
		}
	})
}
