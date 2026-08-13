package build

import (
	"fmt"

	"github.com/goccy/go-yaml"
	"github.com/isyuah/gline/internal/agent/config"
	"github.com/isyuah/gline/internal/agent/destination"
)

func Destination(cfg config.SenderDestinationConfig) (destination.Destination, error) {
	switch cfg.Type {
	case "terminal":
		return destination.NewTerminalDestination(), nil
	case "gline":
		var params config.GlineDestinationParams
		if err := yaml.Unmarshal(cfg.Params, &params); err != nil {
			return nil, err
		}
		return destination.NewGlineDest(params.URL, params.Token), nil
	default:
		return nil, fmt.Errorf("unknown destination type: %s", cfg.Type)
	}
}
