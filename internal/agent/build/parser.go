package build

import (
	"fmt"

	"github.com/isyuah/gline/internal/agent/config"
	"github.com/isyuah/gline/internal/agent/parser"
)

func Parser(cfg config.PipelineParserConfig) (parser.Parser, error) {
	switch cfg.Type {
	case "string_line":
		return parser.NewStringLineLogParser(), nil
	default:
		return nil, fmt.Errorf("unknown parser type %q", cfg.Type)
	}
}
