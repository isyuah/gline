package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/isyuah/gline/internal/agent/config"
	"github.com/isyuah/gline/internal/agent/spool"
)

func TestParserBuildsStringLineParser(t *testing.T) {
	got, err := Parser(config.PipelineParserConfig{Type: "string_line"})
	if err != nil {
		t.Fatalf("Parser() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("Parser() = nil, want parser")
	}
}

func TestAgentBuildsAndRunsReliableMode(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "app.log")
	if err := os.WriteFile(sourcePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := func(value any) yaml.RawMessage {
		encoded, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	cfg := config.GlineAgentConfig{
		Version: config.CurrentVersion,
		Agent: config.AgentConfig{
			ID:  "22222222-2222-4222-8222-222222222222",
			Log: config.AgentLogConfig{Level: "info", File: filepath.Join(directory, "agent.log")},
		},
		Pipelines: []config.PipelineConfig{{
			ID: "33333333-3333-4333-8333-333333333333", Service: "api", Host: "node-1",
			Source: config.PipelineSourceConfig{Type: "file", Params: raw(config.FileSourceParams{Path: sourcePath, SourceKey: "app"})},
			Parser: config.PipelineParserConfig{Type: "string_line", Params: raw(map[string]any{})},
		}},
		Sender: config.SenderConfig{
			Type: "reliable", Params: raw(config.ReliableSenderParams{SpoolPath: filepath.Join(directory, "agent.wal")}),
			Destination: config.SenderDestinationConfig{
				Type: "gline", Params: raw(config.GlineDestinationParams{URL: "http://127.0.0.1:1/api/v1/batches", Token: "test-token"}),
			},
		},
	}
	runtime, err := Agent(cfg)
	if err != nil {
		t.Fatalf("Agent() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runtime.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestBuildRejectsUnknownComponentType(t *testing.T) {
	tests := []struct {
		name  string
		build func() error
	}{
		{
			name: "source",
			build: func() error {
				_, err := Source(config.PipelineSourceConfig{Type: "unknown"})
				return err
			},
		},
		{
			name: "parser",
			build: func() error {
				_, err := Parser(config.PipelineParserConfig{Type: "unknown"})
				return err
			},
		},
		{
			name: "sender",
			build: func() error {
				_, err := Sender(config.SenderConfig{Type: "unknown"})
				return err
			},
		},
		{
			name: "destination",
			build: func() error {
				_, err := Destination(config.SenderDestinationConfig{Type: "unknown"})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.build(); err == nil {
				t.Fatal("build error = nil, want unknown type error")
			}
		})
	}
}

func TestReliableBuildPersistsNewEpochAfterOfflineFileRotation(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "app.log")
	walPath := filepath.Join(directory, "agent.wal")
	logPath := filepath.Join(directory, "agent.log")
	if err := os.WriteFile(sourcePath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := func(value any) yaml.RawMessage {
		encoded, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	cfg := config.GlineAgentConfig{
		Version: config.CurrentVersion,
		Agent:   config.AgentConfig{ID: "22222222-2222-4222-8222-222222222222", Log: config.AgentLogConfig{Level: "info", File: logPath}},
		Pipelines: []config.PipelineConfig{{
			ID: "33333333-3333-4333-8333-333333333333", Service: "api", Host: "node-1",
			Source: config.PipelineSourceConfig{Type: "file", Params: raw(config.FileSourceParams{Path: sourcePath, SourceKey: "app"})},
			Parser: config.PipelineParserConfig{Type: "string_line", Params: raw(map[string]any{})},
		}},
		Sender: config.SenderConfig{Type: "reliable", Params: raw(config.ReliableSenderParams{SpoolPath: walPath}), Destination: config.SenderDestinationConfig{
			Type: "gline", Params: raw(config.GlineDestinationParams{URL: "http://127.0.0.1:1/api/v1/batches", Token: "test-token"}),
		}},
	}
	first, err := Agent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := first.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run() error=%v", err)
	}
	store, err := spool.Open(spool.Config{Path: walPath})
	if err != nil {
		t.Fatal(err)
	}
	old, ok := store.Checkpoint("app")
	if !ok {
		t.Fatal("first build did not anchor checkpoint")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	rotated := sourcePath + ".1"
	if err := os.Rename(sourcePath, rotated); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	second, err := Agent(cfg)
	if err != nil {
		t.Fatalf("build after offline rotation: %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if err := second.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("second Run() error=%v", err)
	}
	store, err = spool.Open(spool.Config{Path: walPath})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	next, ok := store.Checkpoint("app")
	if !ok || next.FileIdentity == old.FileIdentity || next.OffsetBytes != 0 {
		t.Fatalf("offline rotation checkpoint old=%+v next=%+v ok=%v", old, next, ok)
	}
}
