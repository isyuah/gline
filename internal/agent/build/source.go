package build

import (
	"fmt"

	"github.com/goccy/go-yaml"
	"github.com/isyuah/gline/internal/agent/config"
	"github.com/isyuah/gline/internal/agent/source"
)

func Source(cfg config.PipelineSourceConfig) (source.Source, error) {
	switch cfg.Type {
	case "file":
		var params config.FileSourceParams
		if err := yaml.Unmarshal(cfg.Params, &params); err != nil {
			return nil, err
		}
		return source.NewFileSource(params.Path)
	default:
		return nil, fmt.Errorf("unknown source type: %s", cfg.Type)
	}
}
