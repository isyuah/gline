package build

import (
	"github.com/isyuah/gline/internal/agent"
	"github.com/isyuah/gline/internal/agent/config"
)

func Pipeline(cfg config.PipelineConfig) (agent.SourcePipeline, error) {

	src, err := Source(cfg.Source)
	if err != nil {
		return agent.SourcePipeline{}, err
	}
	parser, err := Parser(cfg.Parser)
	if err != nil {
		return agent.SourcePipeline{}, err
	}

	return agent.SourcePipeline{
		Source:  src,
		Parser:  parser,
		Service: cfg.Service,
		Host:    cfg.Host,
	}, nil
}
