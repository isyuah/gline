package agent

import (
	"context"
	"errors"
	"time"

	"github.com/isyuah/gline/internal/agent/agenterr"
	"github.com/isyuah/gline/internal/agent/parser"
	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/logentry"
	"github.com/rs/zerolog"
)

type SourcePipeline struct {
	Logger zerolog.Logger

	Source  source.Source
	Parser  parser.Parser
	Service string
	Host    string
}

func NewSourcePipeline(source source.Source, parser parser.Parser, service, host string) *SourcePipeline {
	return &SourcePipeline{Source: source, Parser: parser, Service: service, Host: host}
}

func (pipeline *SourcePipeline) Start(ctx context.Context, entries chan<- logentry.LogEntry) error {
	for {
		raw, err := pipeline.Source.NextRecord(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}

			var sourceErr *source.SourceError
			if errors.As(err, &sourceErr) && sourceErr.Kind == agenterr.ErrorKindTemporary {
				pipeline.Logger.Warn().Err(err).Msg("temporary source error occurred")
				timer := time.NewTimer(time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
				continue
			}
			pipeline.Logger.Error().Err(err).Msg("source error occurred, stopping pipeline")
			return err
		}
		entry, err := pipeline.Parser.Parse(raw)
		if err != nil {
			pipeline.Logger.Warn().Err(err).Str("content", raw.Content).Msg("failed to parse log record")
			entry = logentry.LogEntry{
				Timestamp: raw.ObservedAt,
				Level:     logentry.LevelUnknown,
				Message:   raw.Content,
			}
		}
		entry.Host = pipeline.Host
		entry.Service = pipeline.Service
		select {
		case <-ctx.Done():
			return ctx.Err()
		case entries <- entry:
		}
	}
}
