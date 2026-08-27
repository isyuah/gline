package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/isyuah/gline/internal/agent/config"
	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/agent/spool"
)

func TestManageQuarantineListsMetadataAndDiscardsExplicitBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.wal")
	store, err := spool.Open(spool.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	commit := spool.Commit{
		BatchID: "batch-bad", Payload: []byte(`{"batch_id":"batch-bad"}`),
		Checkpoint: source.Checkpoint{SourceKey: "app", FileIdentity: "file-1", OffsetBytes: 42, ObservedAt: time.Now().UTC()},
	}
	if err := store.Commit(t.Context(), commit); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Quarantine(t.Context(), commit.BatchID, 422, "invalid_entry", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	params, err := yaml.Marshal(config.ReliableSenderParams{SpoolPath: path})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.GlineAgentConfig{Sender: config.SenderConfig{Type: "reliable", Params: params}}

	var output bytes.Buffer
	handled, err := manageQuarantine(context.Background(), cfg, true, "", &output)
	if err != nil || !handled {
		t.Fatalf("list handled=%v error=%v", handled, err)
	}
	var listed []quarantineSummary
	if err := json.Unmarshal(output.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].BatchID != commit.BatchID || listed[0].PayloadBytes != len(commit.Payload) || listed[0].SourceKey != "app" {
		t.Fatalf("listed=%+v", listed)
	}

	output.Reset()
	handled, err = manageQuarantine(context.Background(), cfg, false, commit.BatchID, &output)
	if err != nil || !handled {
		t.Fatalf("discard handled=%v error=%v", handled, err)
	}
	store, err = spool.Open(spool.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(store.Quarantined()) != 0 {
		t.Fatalf("quarantine after discard=%+v", store.Quarantined())
	}
}
