package observability

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/agent/spool"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsExposeRecoveredSpoolStateWithoutHighCardinalityLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.wal")
	store, err := spool.Open(spool.Config{Path: path, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(t.Context(), spool.Commit{
		BatchID: "batch-one", Payload: []byte(`{"batch_id":"batch-one"}`),
		Checkpoint: source.Checkpoint{SourceKey: "app", FileIdentity: "file-one", OffsetBytes: 12, ObservedAt: time.Now().Add(-time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = spool.Open(spool.Config{Path: path, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry, store)
	metrics.ObserveRecord("pipeline-one", "parsed")
	metrics.ObserveBatchSpooled()
	metrics.ObserveDelivery("accepted", time.Millisecond)
	metrics.SetPipelineUp("pipeline-one", true)

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"gline_agent_records_read_total": false, "gline_agent_batches_spooled_total": false,
		"gline_agent_send_attempts_total": false, "gline_agent_spool_bytes": false,
		"gline_agent_spool_batches": false, "gline_agent_quarantined_batches": false,
		"gline_agent_oldest_pending_seconds": false, "gline_agent_pipeline_up": false,
	}
	for _, family := range families {
		if _, ok := want[family.GetName()]; ok {
			want[family.GetName()] = true
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "batch_id" || label.GetName() == "source_key" {
					t.Fatalf("metric %s exposes forbidden label %s", family.GetName(), label.GetName())
				}
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric family %s was not gathered", name)
		}
	}
}
