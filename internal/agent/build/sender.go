package build

import (
	"fmt"

	"github.com/goccy/go-yaml"
	"github.com/isyuah/gline/internal/agent/config"
	"github.com/isyuah/gline/internal/agent/sender"
)

func Sender(cfg config.SenderConfig) (sender.Sender, error) {
	switch cfg.Type {
	case "tick_or_batch":
		var params config.TickOrBatchSenderParams
		if err := yaml.Unmarshal(cfg.Params, &params); err != nil {
			return nil, fmt.Errorf("failed to unmarshal TickOrBatchSenderParams: %w", err)
		}
		dest, err := Destination(cfg.Destination)
		if err != nil {
			return nil, err
		}
		return sender.NewTickOrBatchSender(dest, sender.TickOrBatchSenderOptions{
			BatchSize:     params.BatchSize,
			FlushInterval: params.FlushInterval,
		}), nil
	default:
		return nil, fmt.Errorf("unknown sender type: %s", cfg.Type)
	}
}
