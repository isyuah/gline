package build

import (
	"testing"

	"github.com/isyuah/gline/internal/agent/config"
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
