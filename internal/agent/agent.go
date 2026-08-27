package agent

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"

	"github.com/isyuah/gline/internal/agent/sender"
	"github.com/isyuah/gline/internal/logentry"
	"github.com/rs/zerolog"
)

type Agent struct {
	Logger zerolog.Logger

	Pipelines []SourcePipeline
	Sender    sender.Sender
}

func (a *Agent) Run(ctx context.Context) error {
	pipelineCtx, cancelPipelineCtx := context.WithCancelCause(ctx)
	senderLogger := a.Logger.With().Str("component", "sender").Logger()
	defer cancelPipelineCtx(nil)
	senderCtx, cancelSenderCtx := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelSenderCtx()
	pipelineWg := sync.WaitGroup{}
	entries := make(chan logentry.LogEntry, 10000)

	for _, pipeline := range a.Pipelines {
		pipeline.Logger = a.Logger.With().Str("component", "pipeline").Str("pipeline", pipeline.ID).Str("service", pipeline.Service).Str("host", pipeline.Host).Logger()
		pipelineWg.Go(func() {
			defer func() {
				if err := recover(); err != nil {
					pipeline.Logger.WithLevel(zerolog.FatalLevel).
						Interface("panic", err).
						Bytes("stack", debug.Stack()).
						Msg("pipeline panicked")
				}
			}()
			_ = pipeline.Start(pipelineCtx, entries)
		})
	}
	pipelinesDone := make(chan struct{})
	go func() {
		pipelineWg.Wait()
		close(entries)
		close(pipelinesDone)
	}()

	//Sender
	senderDone := make(chan error, 1)
	go func() {
		senderDone <- a.Sender.Run(senderCtx, entries)
	}()

	var runErr error
	var senderErr error
	senderFinished := false

	select {
	case <-pipelineCtx.Done():
		runErr = context.Cause(pipelineCtx)
	case err := <-senderDone:
		senderErr = err
		senderFinished = true
		if err != nil {
			cancelPipelineCtx(err)
			runErr = err
		}
	}

	<-pipelinesDone

	if !senderFinished {
		senderErr = <-senderDone
	}
	if senderErr != nil && !errors.Is(senderErr, context.Canceled) && !errors.Is(senderErr, context.DeadlineExceeded) {
		senderLogger.Error().Err(senderErr).Msg("sender stopped with error")
		return senderErr
	}
	return runErr
}
