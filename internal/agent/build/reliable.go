package build

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/isyuah/gline/internal/agent"
	"github.com/isyuah/gline/internal/agent/config"
	agentobservability "github.com/isyuah/gline/internal/agent/observability"
	"github.com/isyuah/gline/internal/agent/reliable"
	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/agent/spool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

func ReliableAgent(cfg config.GlineAgentConfig, logger zerolog.Logger) (agent.Runtime, error) {
	var senderParams config.ReliableSenderParams
	if err := yaml.Unmarshal(cfg.Sender.Params, &senderParams); err != nil {
		return nil, fmt.Errorf("decode reliable sender params: %w", err)
	}
	if strings.TrimSpace(senderParams.SpoolPath) == "" {
		return nil, fmt.Errorf("reliable sender spool_path is empty")
	}
	store, err := spool.Open(spool.Config{
		Path: senderParams.SpoolPath, MaxBytes: senderParams.MaxSpoolBytes,
		MaxRecordBytes: senderParams.MaxRecordBytes,
	})
	if err != nil {
		return nil, err
	}
	closeOnError := func(cause error, pipelines []reliable.Pipeline) (agent.Runtime, error) {
		for index := range pipelines {
			_ = pipelines[index].Source.Close()
		}
		return nil, errors.Join(cause, store.Close())
	}

	pipelines := make([]reliable.Pipeline, 0, len(cfg.Pipelines))
	for _, pipelineConfig := range cfg.Pipelines {
		var params config.FileSourceParams
		if err := yaml.Unmarshal(pipelineConfig.Source.Params, &params); err != nil {
			return closeOnError(fmt.Errorf("decode file source params for pipeline %s: %w", pipelineConfig.ID, err), pipelines)
		}
		sourceKey := strings.TrimSpace(params.SourceKey)
		if sourceKey == "" {
			sourceKey = pipelineConfig.ID
		}
		checkpoint, exists := store.Checkpoint(sourceKey)
		var initial *source.Checkpoint
		if exists {
			initial = &checkpoint
		}
		fileSource, err := source.NewDurableFileSource(source.DurableFileOptions{
			Path: params.Path, SourceKey: sourceKey, Checkpoint: initial,
		})
		if err != nil {
			return closeOnError(fmt.Errorf("open reliable pipeline %s: %w", pipelineConfig.ID, err), pipelines)
		}
		initialCheckpoint := fileSource.InitialCheckpoint()
		if !exists {
			err = store.Transition(context.Background(), nil, initialCheckpoint)
		} else if checkpoint.FileIdentity != initialCheckpoint.FileIdentity || checkpoint.OffsetBytes != initialCheckpoint.OffsetBytes {
			err = store.Transition(context.Background(), &checkpoint, initialCheckpoint)
		}
		if err != nil {
			_ = fileSource.Close()
			return closeOnError(fmt.Errorf("persist initial checkpoint for pipeline %s: %w", pipelineConfig.ID, err), pipelines)
		}
		entryParser, err := Parser(pipelineConfig.Parser)
		if err != nil {
			_ = fileSource.Close()
			return closeOnError(err, pipelines)
		}
		pipelines = append(pipelines, reliable.Pipeline{
			ID: pipelineConfig.ID, Source: fileSource, Parser: entryParser,
			Service: pipelineConfig.Service, Host: pipelineConfig.Host,
		})
	}

	var destinationParams config.GlineDestinationParams
	if err := yaml.Unmarshal(cfg.Sender.Destination.Params, &destinationParams); err != nil {
		return closeOnError(fmt.Errorf("decode reliable destination params: %w", err), pipelines)
	}
	transport, err := reliable.NewHTTPTransport(destinationParams.URL, destinationParams.Token, nil)
	if err != nil {
		return closeOnError(err, pipelines)
	}
	heartbeatURL := strings.TrimSpace(destinationParams.HeartbeatURL)
	if heartbeatURL == "" {
		parsed, parseErr := url.Parse(destinationParams.URL)
		if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" || !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/batches") {
			return closeOnError(errors.New("derive heartbeat URL: ingest URL must end in /batches or heartbeat_url must be set"), pipelines)
		}
		parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/batches") + "/agents/" + url.PathEscape(cfg.Agent.ID) + "/heartbeat"
		heartbeatURL = parsed.String()
	}
	pipelineIDs := make([]string, len(pipelines))
	for index := range pipelines {
		pipelineIDs[index] = pipelines[index].ID
	}
	heartbeat, err := reliable.NewHTTPHeartbeat(heartbeatURL, destinationParams.Token, cfg.Agent.Version, pipelineIDs, nil)
	if err != nil {
		return closeOnError(err, pipelines)
	}
	var metrics *agentobservability.Metrics
	var operations *http.Server
	if address := strings.TrimSpace(cfg.Agent.MetricsAddr); address != "" {
		registry := prometheus.NewRegistry()
		registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		metrics = agentobservability.NewMetrics(registry, store)
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
		mux.HandleFunc("/livez", func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"status":"healthy"}`))
		})
		operations = &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	}
	dispatcherOptions := reliable.DispatcherOptions{
		BaseDelay: senderParams.Retry.BaseDelay, MaxDelay: senderParams.Retry.MaxDelay,
		Jitter: senderParams.Retry.Jitter,
		OnQuarantine: func(value spool.Quarantined) {
			logger.Error().Str("batch_id", value.Commit.BatchID).Int("http_status", value.HTTPCode).
				Str("error_code", value.ErrorCode).Msg("batch moved to local quarantine")
		},
	}
	if metrics != nil {
		dispatcherOptions.Observer = metrics
	}
	dispatcher, err := reliable.NewDispatcher(store, transport, dispatcherOptions)
	if err != nil {
		return closeOnError(err, pipelines)
	}
	result := &reliable.Agent{
		Logger: logger, AgentID: cfg.Agent.ID, Pipelines: pipelines,
		Spool: store, Dispatcher: dispatcher, Heartbeat: heartbeat,
		Operations: operations,
		Options:    reliable.AgentOptions{BatchSize: senderParams.BatchSize, FlushInterval: senderParams.FlushInterval, HeartbeatInterval: senderParams.HeartbeatInterval},
	}
	if metrics != nil {
		result.Observer = metrics
	}
	return result, nil
}
