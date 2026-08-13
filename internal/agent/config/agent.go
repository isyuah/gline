package config

import "github.com/goccy/go-yaml"

type GlineAgentConfig struct {
	Version   int              `yaml:"version"`
	Agent     AgentConfig      `yaml:"agent"`
	Pipelines []PipelineConfig `yaml:"pipelines"`
	Sender    SenderConfig     `yaml:"sender"`
}

type AgentLogConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

type AgentConfig struct {
	Log AgentLogConfig `yaml:"log"`
}

type PipelineSourceConfig struct {
	Type   string          `yaml:"type"`
	Params yaml.RawMessage `yaml:"params"`
}

type PipelineParserConfig struct {
	Type   string          `yaml:"type"`
	Params yaml.RawMessage `yaml:"params"`
}

type PipelineConfig struct {
	ID      string `yaml:"id"`
	Service string `yaml:"service"`
	Host    string `yaml:"host"`

	Source PipelineSourceConfig `yaml:"source"`
	Parser PipelineParserConfig `yaml:"parser"`
}

type SenderDestinationConfig struct {
	Type   string          `yaml:"type"`
	Params yaml.RawMessage `yaml:"params"`
}

type SenderConfig struct {
	Type        string                  `yaml:"type"`
	Params      yaml.RawMessage         `yaml:"params"`
	Destination SenderDestinationConfig `yaml:"destination"`
}
