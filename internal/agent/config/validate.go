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
	return nil
}
