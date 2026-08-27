package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/isyuah/gline/internal/agent/build"
	"github.com/isyuah/gline/internal/agent/config"
	"github.com/isyuah/gline/internal/agent/spool"
)

func main() {
	configPath := flag.String("config", ".glineconf", "path to the agent YAML configuration")
	listQuarantine := flag.Bool("quarantine-list", false, "list locally quarantined batches and exit")
	discardQuarantine := flag.String("quarantine-discard", "", "discard one locally quarantined batch by ID and exit")
	flag.Parse()
	file, err := os.Open(*configPath)
	if err != nil {
		fmt.Printf("Error opening agent config %s: %s\n", *configPath, err)
		os.Exit(1)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		fmt.Printf("Error reading .glineconf: %s\n", err)
		os.Exit(1)
	}

	cfg, err := config.ParseConfig(data)
	if err != nil {
		fmt.Printf("Error parsing .glineconf: %s\n", err)
		os.Exit(1)
	}
	handled, err := manageQuarantine(context.Background(), cfg, *listQuarantine, *discardQuarantine, os.Stdout)
	if err != nil {
		fmt.Printf("Error managing local quarantine: %s\n", err)
		os.Exit(1)
	}
	if handled {
		return
	}
	ag, err := build.Agent(cfg)
	if err != nil {
		fmt.Printf("Error building agent: %s\n", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := ag.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Printf("Error running agent: %s\n", err)
		os.Exit(1)
	}
}

type quarantineSummary struct {
	BatchID       string `json:"batch_id"`
	HTTPStatus    int    `json:"http_status"`
	ErrorCode     string `json:"error_code,omitempty"`
	QuarantinedAt string `json:"quarantined_at"`
	SourceKey     string `json:"source_key"`
	OffsetBytes   int64  `json:"offset_bytes"`
	PayloadBytes  int    `json:"payload_bytes"`
}

func manageQuarantine(ctx context.Context, cfg config.GlineAgentConfig, list bool, discard string, output io.Writer) (bool, error) {
	if !list && discard == "" {
		return false, nil
	}
	if list && discard != "" {
		return true, errors.New("quarantine-list and quarantine-discard are mutually exclusive")
	}
	if cfg.Sender.Type != "reliable" {
		return true, errors.New("local quarantine is available only for reliable sender mode")
	}
	var params config.ReliableSenderParams
	if err := yaml.Unmarshal(cfg.Sender.Params, &params); err != nil {
		return true, fmt.Errorf("decode reliable sender params: %w", err)
	}
	store, err := spool.Open(spool.Config{Path: params.SpoolPath, MaxBytes: params.MaxSpoolBytes, MaxRecordBytes: params.MaxRecordBytes})
	if err != nil {
		return true, err
	}
	defer store.Close()
	if discard != "" {
		found := false
		for _, value := range store.Quarantined() {
			if value.Commit.BatchID == discard {
				found = true
				break
			}
		}
		if !found {
			return true, fmt.Errorf("batch %q is not in local quarantine", discard)
		}
		if err := store.Ack(ctx, discard); err != nil {
			return true, err
		}
		return true, json.NewEncoder(output).Encode(map[string]string{"batch_id": discard, "status": "discarded"})
	}
	values := store.Quarantined()
	summaries := make([]quarantineSummary, len(values))
	for index, value := range values {
		summaries[index] = quarantineSummary{
			BatchID: value.Commit.BatchID, HTTPStatus: value.HTTPCode, ErrorCode: value.ErrorCode,
			QuarantinedAt: value.QuarantinedAt.UTC().Format(time.RFC3339Nano),
			SourceKey:     value.Commit.Checkpoint.SourceKey, OffsetBytes: value.Commit.Checkpoint.OffsetBytes,
			PayloadBytes: len(value.Commit.Payload),
		}
	}
	return true, json.NewEncoder(output).Encode(summaries)
}
