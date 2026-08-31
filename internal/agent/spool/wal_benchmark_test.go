package spool

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/agent/source"
)

// BenchmarkWALCommitAck measures the durable local boundary used before an
// Agent advances a file checkpoint. It intentionally includes the WAL fsyncs.
func BenchmarkWALCommitAck(b *testing.B) {
	store, err := Open(Config{Path: filepath.Join(b.TempDir(), "benchmark.wal"), MaxBytes: 1 << 30, MaxRecordBytes: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		batchID := fmt.Sprintf("benchmark-%d", index)
		payload := []byte(`{"batch_id":"` + batchID + `","entries":[{"message":"benchmark"}]}`)
		commit := Commit{
			BatchID: batchID, Payload: payload,
			Checkpoint: source.Checkpoint{SourceKey: "benchmark", FileIdentity: "file-1", OffsetBytes: int64(index + 1), ObservedAt: time.Unix(int64(index), 0).UTC()},
		}
		if err := store.Commit(b.Context(), commit); err != nil {
			b.Fatal(err)
		}
		if err := store.Ack(b.Context(), batchID); err != nil {
			b.Fatal(err)
		}
	}
}
