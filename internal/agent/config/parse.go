package config

import "github.com/goccy/go-yaml"

func ParseConfig(rawStr []byte) (GlineAgentConfig, error) {
	var cfg GlineAgentConfig
	if err := yaml.UnmarshalWithOptions(rawStr, &cfg, yaml.Strict()); err != nil {
		return GlineAgentConfig{}, err
	}
	return cfg, nil
}
