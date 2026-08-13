package build

import (
	"fmt"
	"os"
	"time"

	"github.com/isyuah/gline/internal/agent"
	"github.com/isyuah/gline/internal/agent/config"
	"github.com/rs/zerolog"
)

func Agent(cfg config.GlineAgentConfig) (agent.Agent, error) {
	err := cfg.Validate()
	if err != nil {
		return agent.Agent{}, err
	}

	// Logger
	consoleWriter := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	}
	file, err := os.OpenFile(cfg.Agent.Log.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return agent.Agent{}, fmt.Errorf("failed to open log file: %w", err)
	}

	writer := zerolog.SyncWriter(zerolog.MultiLevelWriter(consoleWriter, file))
	var level zerolog.Level
	switch cfg.Agent.Log.Level {
	case "debug":
		level = zerolog.DebugLevel
	case "info":
		level = zerolog.InfoLevel
	case "warn":
		level = zerolog.WarnLevel
	case "error":
		level = zerolog.ErrorLevel
	case "fatal":
		level = zerolog.FatalLevel
	case "panic":
		level = zerolog.PanicLevel
	default:
		level = zerolog.DebugLevel
	}
	rootLogger := zerolog.New(writer).Level(level).With().Timestamp().Logger()

	// Pipelines
	pipelines := make([]agent.SourcePipeline, 0, len(cfg.Pipelines))
	for _, p := range cfg.Pipelines {
		pipeline, err := Pipeline(p)
		if err != nil {
			return agent.Agent{}, err
		}
		pipelines = append(pipelines, pipeline)
	}

	// Sender
	sender, err := Sender(cfg.Sender)
	if err != nil {
		return agent.Agent{}, err
	}

	return agent.Agent{
		Logger:    rootLogger,
		Pipelines: pipelines,
		Sender:    sender,
	}, nil
}
