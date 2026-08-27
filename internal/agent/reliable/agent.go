package reliable

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/isyuah/gline/internal/agent/parser"
	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/agent/spool"
	"github.com/isyuah/gline/internal/logentry"
	"github.com/rs/zerolog"
)

type Pipeline struct {
	ID      string
	Source  *source.DurableFileSource
	Parser  parser.Parser
	Service string
	Host    string
}

type AgentOptions struct {
	BatchSize         int
	FlushInterval     time.Duration
	HeartbeatInterval time.Duration
}

type HeartbeatReporter interface {
	Report(context.Context) error
}

type Agent struct {
	Logger     zerolog.Logger
	AgentID    string
	Pipelines  []Pipeline
	Spool      *spool.WAL
	Dispatcher *Dispatcher
	Heartbeat  HeartbeatReporter
	Options    AgentOptions
}

func (a *Agent) Run(ctx context.Context) error {
	if len(a.Pipelines) == 0 || a.Spool == nil || a.Dispatcher == nil {
		return errors.New("reliable agent is incomplete")
	}
	defer a.Spool.Close()
	if a.Options.BatchSize <= 0 {
		a.Options.BatchSize = 100
	}
	if a.Options.FlushInterval <= 0 {
		a.Options.FlushInterval = 5 * time.Second
	}
	if a.Options.HeartbeatInterval <= 0 {
		a.Options.HeartbeatInterval = 30 * time.Second
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	dispatcherDone := make(chan error, 1)
	go func() { dispatcherDone <- a.Dispatcher.Run(runCtx) }()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		a.runHeartbeat(runCtx)
	}()

	pipelineErrors := make(chan error, len(a.Pipelines))
	var pipelineWait sync.WaitGroup
	for index := range a.Pipelines {
		pipeline := &a.Pipelines[index]
		pipelineWait.Go(func() {
			logger := a.Logger.With().Str("component", "reliable_pipeline").Str("pipeline", pipeline.ID).Logger()
			if err := a.runPipeline(runCtx, logger, pipeline); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case pipelineErrors <- err:
				default:
				}
			}
		})
	}

	var cause error
	dispatcherFinished := false
	select {
	case <-ctx.Done():
		cause = ctx.Err()
	case err := <-dispatcherDone:
		dispatcherFinished = true
		cause = err
	case err := <-pipelineErrors:
		cause = err
	}
	cancel(cause)
	pipelineWait.Wait()
	if !dispatcherFinished {
		<-dispatcherDone
	}
	<-heartbeatDone
	return cause
}

func (a *Agent) runHeartbeat(ctx context.Context) {
	if a.Heartbeat == nil {
		return
	}
	ticker := time.NewTicker(a.Options.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := a.Heartbeat.Report(ctx); err != nil && ctx.Err() == nil {
			a.Logger.Warn().Err(err).Msg("agent heartbeat failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) runPipeline(ctx context.Context, logger zerolog.Logger, pipeline *Pipeline) error {
	defer pipeline.Source.Close()
	ticker := time.NewTicker(a.Options.FlushInterval)
	defer ticker.Stop()
	records := make(chan source.RawRecord)
	sourceDone := make(chan error, 1)
	startSource := func() {
		go func() {
			for {
				record, err := pipeline.Source.NextRecord(ctx)
				if err != nil {
					sourceDone <- err
					return
				}
				select {
				case <-ctx.Done():
					sourceDone <- ctx.Err()
					return
				case records <- record:
				}
			}
		}()
	}
	startSource()
	entries := make([]logentry.LogEntry, 0, a.Options.BatchSize)
	var checkpoint source.Checkpoint
	flush := func(commitCtx context.Context) error {
		if len(entries) == 0 {
			return nil
		}
		batchID, payload, err := buildBatch(a.AgentID, pipeline.ID, checkpoint.OffsetBytes, entries, time.Now())
		if err != nil {
			return err
		}
		commit := spool.Commit{BatchID: batchID, Payload: payload, Checkpoint: checkpoint}
		for {
			err = a.Spool.Commit(commitCtx, commit)
			if !errors.Is(err, spool.ErrFull) {
				break
			}
			select {
			case <-commitCtx.Done():
				return commitCtx.Err()
			case <-a.Spool.Changed():
			}
		}
		if err != nil {
			return err
		}
		entries = entries[:0]
		return nil
	}

	shutdownFlush := func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := flush(flushCtx); err != nil {
			logger.Warn().Err(err).Msg("shutdown left uncommitted records for checkpoint replay")
		}
	}
	for {
		select {
		case <-ctx.Done():
			shutdownFlush()
			return ctx.Err()
		case err := <-sourceDone:
			var rotation *source.RotationRequired
			if errors.As(err, &rotation) {
				if flushErr := flush(ctx); flushErr != nil {
					return errors.Join(err, flushErr)
				}
				if rotateErr := pipeline.Source.Rotate(ctx, a.Spool.Transition); rotateErr != nil {
					return errors.Join(err, rotateErr)
				}
				logger.Info().Str("path", rotation.Path).Msg("source switched to recreated file")
				startSource()
				continue
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				shutdownFlush()
				return err
			}
			if flushErr := flush(ctx); flushErr != nil {
				return errors.Join(err, flushErr)
			}
			return err
		case <-ticker.C:
			if err := flush(ctx); err != nil {
				return fmt.Errorf("flush reliable pipeline %s: %w", pipeline.ID, err)
			}
		case raw := <-records:
			entry, parseErr := pipeline.Parser.Parse(raw)
			if parseErr != nil {
				logger.Warn().Err(parseErr).Msg("failed to parse log record")
				entry = logentry.LogEntry{Timestamp: raw.ObservedAt, Level: logentry.LevelUnknown, Message: raw.Content}
			}
			entry.Host = pipeline.Host
			entry.Service = pipeline.Service
			entries = append(entries, entry)
			checkpoint = source.Checkpoint{
				SourceKey: raw.SourceKey, FileIdentity: raw.FileIdentity,
				OffsetBytes: raw.EndOffset, ObservedAt: raw.ObservedAt,
			}
			if len(entries) >= a.Options.BatchSize {
				if err := flush(ctx); err != nil {
					return fmt.Errorf("flush reliable pipeline %s: %w", pipeline.ID, err)
				}
			}
		}
	}
}
