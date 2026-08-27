package build

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/isyuah/gline/internal/agent"
	"github.com/isyuah/gline/internal/agent/config"
	"github.com/rs/zerolog"
)

func Agent(cfg config.GlineAgentConfig) (agent.Runtime, error) {
	err := cfg.Validate()
	if err != nil {
		return nil, err
	}

	// Logger
	consoleWriter := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	}
	file, err := os.OpenFile(cfg.Agent.Log.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
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
	if cfg.Sender.Type == "reliable" {
		runtime, err := ReliableAgent(cfg, rootLogger)
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
		return agent.WithCloser(runtime, file), nil
	}

	// Pipelines
	pipelines := make([]agent.SourcePipeline, 0, len(cfg.Pipelines))
	for _, p := range cfg.Pipelines {
		pipeline, err := Pipeline(p)
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
		pipelines = append(pipelines, pipeline)
	}

	// Sender
	sender, err := Sender(cfg.Sender)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}

	runtime := &agent.Agent{
		Logger:    rootLogger,
		Pipelines: pipelines,
		Sender:    sender,
	}
	return agent.WithCloser(runtime, file), nil
}
