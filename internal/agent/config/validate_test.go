package config

import "testing"

func validConfig() GlineAgentConfig {
	return GlineAgentConfig{
		Version: CurrentVersion,
		Pipelines: []PipelineConfig{
			{
				ID:      "app",
				Service: "app",
				Source:  PipelineSourceConfig{Type: "file"},
				Parser:  PipelineParserConfig{Type: "string_line"},
			},
		},
		Sender: SenderConfig{
			Type: "tick_or_batch",
			Destination: SenderDestinationConfig{
				Type: "terminal",
			},
		},
	}
}

func TestGlineAgentConfigValidate(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestGlineAgentConfigValidateRejectsUnsupportedVersion(t *testing.T) {
	cfg := validConfig()
	cfg.Version = CurrentVersion + 1

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want unsupported version error")
	}
}

func TestGlineAgentConfigValidateRejectsMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*GlineAgentConfig)
	}{
		{
			name: "pipelines",
			mutate: func(cfg *GlineAgentConfig) {
				cfg.Pipelines = nil
			},
		},
		{
			name: "pipeline id",
			mutate: func(cfg *GlineAgentConfig) {
				cfg.Pipelines[0].ID = "  "
			},
		},
		{
			name: "pipeline service",
			mutate: func(cfg *GlineAgentConfig) {
				cfg.Pipelines[0].Service = ""
			},
		},
		{
			name: "source type",
			mutate: func(cfg *GlineAgentConfig) {
				cfg.Pipelines[0].Source.Type = ""
			},
		},
		{
			name: "parser type",
			mutate: func(cfg *GlineAgentConfig) {
				cfg.Pipelines[0].Parser.Type = ""
			},
		},
		{
			name: "sender type",
			mutate: func(cfg *GlineAgentConfig) {
				cfg.Sender.Type = ""
			},
		},
		{
			name: "destination type",
			mutate: func(cfg *GlineAgentConfig) {
				cfg.Sender.Destination.Type = ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)

			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want validation error")
			}
		})
	}
}

func TestGlineAgentConfigValidateRejectsDuplicatePipelineID(t *testing.T) {
	cfg := validConfig()
	cfg.Pipelines = append(cfg.Pipelines, PipelineConfig{
		ID:      cfg.Pipelines[0].ID,
		Service: "other",
		Source:  PipelineSourceConfig{Type: "file"},
		Parser:  PipelineParserConfig{Type: "string_line"},
	})

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want duplicate pipeline ID error")
	}
}

func TestGlineAgentConfigValidateReliableMode(t *testing.T) {
	cfg := validConfig()
	cfg.Agent.ID = "22222222-2222-4222-8222-222222222222"
	cfg.Pipelines[0].ID = "33333333-3333-4333-8333-333333333333"
	cfg.Pipelines[0].Host = "node-1"
	cfg.Sender.Type = "reliable"
	cfg.Sender.Destination.Type = "gline"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}

	cfg.Pipelines[0].ID = "human-name"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want reliable UUID error")
	}
}
