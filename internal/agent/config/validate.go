package config

import (
	"fmt"
	"strings"
)

const CurrentVersion = 1

func (cfg GlineAgentConfig) Validate() error {
	if cfg.Version != CurrentVersion {
		return fmt.Errorf("agent version %d != %d", cfg.Version, CurrentVersion)
	}
	if len(cfg.Pipelines) == 0 {
		return fmt.Errorf("pipelines is empty")
	}
	ids := make(map[string]int, len(cfg.Pipelines))
	for i, pipeline := range cfg.Pipelines {
		if strings.TrimSpace(pipeline.ID) == "" {
			return fmt.Errorf("pipeline[%d] id is empty", i)
		}
		if prev, exists := ids[pipeline.ID]; exists {
			return fmt.Errorf("pipeline %s has previously defined more than once, at [%d] [%d]", pipeline.ID, prev, i)
		}
		ids[pipeline.ID] = i
		if strings.TrimSpace(pipeline.Service) == "" {
			return fmt.Errorf("pipeline[%d] service is empty", i)
		}
		if strings.TrimSpace(pipeline.Source.Type) == "" {
			return fmt.Errorf("pipeline[%d] source type is empty", i)
		}
		if strings.TrimSpace(pipeline.Parser.Type) == "" {
			return fmt.Errorf("pipeline[%d] parser type is empty", i)
		}
	}
	if strings.TrimSpace(cfg.Sender.Type) == "" {
		return fmt.Errorf("sender type is empty")
	}
	if strings.TrimSpace(cfg.Sender.Destination.Type) == "" {
		return fmt.Errorf("sender destination type is empty")
	}
	if cfg.Sender.Type == "reliable" {
		if !validUUID(cfg.Agent.ID) {
			return fmt.Errorf("agent id must be a UUID in reliable mode")
		}
		if cfg.Sender.Destination.Type != "gline" {
			return fmt.Errorf("reliable sender requires gline destination")
		}
		for index, pipeline := range cfg.Pipelines {
			if !validUUID(pipeline.ID) {
				return fmt.Errorf("pipeline[%d] id must be a UUID in reliable mode", index)
			}
			if strings.TrimSpace(pipeline.Host) == "" {
				return fmt.Errorf("pipeline[%d] host is empty in reliable mode", index)
			}
			if pipeline.Source.Type != "file" {
				return fmt.Errorf("pipeline[%d] reliable mode requires file source", index)
			}
		}
	}
	return nil
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}
