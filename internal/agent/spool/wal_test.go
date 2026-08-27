package spool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/agent/source"
)

func TestWALCommitAckAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.wal")
	store := openTestWAL(t, path, 1<<20)
	commit := testCommit("batch-1", "source-a", 42)
	if err := store.Commit(t.Context(), commit); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	assertPending(t, store, []string{"batch-1"})
	assertCheckpoint(t, store, "source-a", 42)
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	store = openTestWAL(t, path, 1<<20)
	assertPending(t, store, []string{"batch-1"})
	assertCheckpoint(t, store, "source-a", 42)
	if err := store.Ack(t.Context(), "batch-1"); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	store = openTestWAL(t, path, 1<<20)
	t.Cleanup(func() { _ = store.Close() })
	assertPending(t, store, nil)
	assertCheckpoint(t, store, "source-a", 42)
}

func TestWALTruncatesIncompleteTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.wal")
	store := openTestWAL(t, path, 1<<20)
	if err := store.Commit(t.Context(), testCommit("batch-1", "source-a", 10)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	validSize := info.Size()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{'G', 'L', 'W', '1', 1}); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestWAL(t, path, 1<<20)
	t.Cleanup(func() { _ = store.Close() })
	assertPending(t, store, []string{"batch-1"})
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != validSize {
		t.Fatalf("recovered WAL size = %d, want %d", info.Size(), validSize)
	}
}

func TestWALRejectsCompleteCorruptRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.wal")
	store := openTestWAL(t, path, 1<<20)
	if err := store.Commit(t.Context(), testCommit("batch-1", "source-a", 10)); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(t.Context(), testCommit("batch-2", "source-a", 20)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	byteAt := []byte{0}
	if _, err := file.ReadAt(byteAt, recordHeaderBytes+5); err != nil {
		t.Fatal(err)
	}
	byteAt[0] ^= 0xff
	if _, err := file.WriteAt(byteAt, recordHeaderBytes+5); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(Config{Path: path, MaxBytes: 1 << 20})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open() error = %v, want ErrCorrupt", err)
	}
}

func TestWALFullDoesNotAdvanceCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.wal")
	store := openTestWAL(t, path, 700)
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Commit(t.Context(), testCommit("batch-1", "source-a", 10)); err != nil {
		t.Fatal(err)
	}
	oversized := testCommit("batch-2", "source-a", 999)
	oversized.Payload = []byte(fmt.Sprintf(`{"batch_id":"batch-2","padding":"%s"}`, strings.Repeat("x", 600)))
	if err := store.Commit(t.Context(), oversized); !errors.Is(err, ErrFull) {
		t.Fatalf("Commit() error = %v, want ErrFull", err)
	}
	assertPending(t, store, []string{"batch-1"})
	assertCheckpoint(t, store, "source-a", 10)
}

func TestWALCompactionPreservesPendingBatchAndLatestCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.wal")
	store := openTestWAL(t, path, 100<<10)
	large := func(batchID string, offset int64, bytes int) Commit {
		commit := testCommit(batchID, "source-a", offset)
		commit.Payload = []byte(fmt.Sprintf(`{"batch_id":%q,"padding":"%s"}`, batchID, strings.Repeat("x", bytes)))
		return commit
	}
	first := large("batch-1", 10, 55<<10)
	if err := store.Commit(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := store.Ack(t.Context(), first.BatchID); err != nil {
		t.Fatal(err)
	}
	second := large("batch-2", 20, 25<<10)
	third := large("batch-3", 30, 25<<10)
	if err := store.Commit(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(t.Context(), third); err != nil {
		t.Fatal(err)
	}
	if err := store.Ack(t.Context(), second.BatchID); err != nil {
		t.Fatal(err)
	}
	assertPending(t, store, []string{"batch-3"})
	assertCheckpoint(t, store, "source-a", 30)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestWAL(t, path, 100<<10)
	t.Cleanup(func() { _ = store.Close() })
	assertPending(t, store, []string{"batch-3"})
	assertCheckpoint(t, store, "source-a", 30)
}

func openTestWAL(t *testing.T, path string, maxBytes int64) *WAL {
	t.Helper()
	store, err := Open(Config{Path: path, MaxBytes: maxBytes, MaxRecordBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return store
}

func testCommit(batchID, sourceKey string, offset int64) Commit {
	return Commit{
		BatchID: batchID,
		Payload: []byte(fmt.Sprintf(`{"batch_id":%q,"entries":[{"message":"value"}]}`, batchID)),
		Checkpoint: source.Checkpoint{
			SourceKey: sourceKey, FileIdentity: "identity-1", OffsetBytes: offset,
			ObservedAt: time.Unix(offset, 0).UTC(),
		},
	}
}

func assertPending(t *testing.T, store *WAL, want []string) {
	t.Helper()
	gotCommits := store.Pending()
	got := make([]string, len(gotCommits))
	for index := range gotCommits {
		got[index] = gotCommits[index].BatchID
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Pending() = %v, want %v", got, want)
	}
}

func assertCheckpoint(t *testing.T, store *WAL, sourceKey string, wantOffset int64) {
	t.Helper()
	checkpoint, ok := store.Checkpoint(sourceKey)
	if !ok || checkpoint.OffsetBytes != wantOffset {
		t.Fatalf("Checkpoint(%q) = %+v, %t; want offset %d", sourceKey, checkpoint, ok, wantOffset)
	}
}

func TestWALCommitHonorsCanceledContext(t *testing.T) {
	store := openTestWAL(t, filepath.Join(t.TempDir(), "agent.wal"), 1<<20)
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Commit(ctx, testCommit("batch-1", "source-a", 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Commit() error = %v, want context.Canceled", err)
	}
	assertPending(t, store, nil)
}

func TestWALCheckpointTransitionIsDurableAndCompareAndSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.wal")
	store, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	initial := source.Checkpoint{SourceKey: "app", FileIdentity: "file-old", OffsetBytes: 10, ObservedAt: time.Now().UTC()}
	if err := store.Transition(t.Context(), nil, initial); err != nil {
		t.Fatal(err)
	}
	next := source.Checkpoint{SourceKey: "app", FileIdentity: "file-new", OffsetBytes: 0, ObservedAt: time.Now().UTC()}
	wrong := initial
	wrong.OffsetBytes = 9
	if err := store.Transition(t.Context(), &wrong, next); err == nil {
		t.Fatal("Transition() error = nil for stale checkpoint")
	}
	if err := store.Transition(t.Context(), &initial, next); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	checkpoint, ok := recovered.Checkpoint("app")
	if !ok || !sameCheckpointPosition(checkpoint, next) {
		t.Fatalf("recovered checkpoint = %+v, ok = %v", checkpoint, ok)
	}
}

func TestWALQuarantineIsDurableAndStillCountsTowardCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.wal")
	store := openTestWAL(t, path, 1<<20)
	commit := testCommit("batch-bad", "app", 10)
	if err := store.Commit(t.Context(), commit); err != nil {
		t.Fatal(err)
	}
	quarantinedAt := time.Now().UTC().Truncate(time.Microsecond)
	value, err := store.Quarantine(t.Context(), commit.BatchID, 422, "invalid_entry", quarantinedAt)
	if err != nil {
		t.Fatal(err)
	}
	if value.Commit.BatchID != commit.BatchID || len(store.Pending()) != 0 || len(store.Quarantined()) != 1 || store.usedBytes == 0 {
		t.Fatalf("value=%+v pending=%+v quarantined=%+v used=%d", value, store.Pending(), store.Quarantined(), store.usedBytes)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestWAL(t, path, 1<<20)
	quarantined := store.Quarantined()
	if len(quarantined) != 1 || quarantined[0].Commit.BatchID != commit.BatchID || quarantined[0].ErrorCode != "invalid_entry" || !quarantined[0].QuarantinedAt.Equal(quarantinedAt) {
		t.Fatalf("recovered quarantine=%+v", quarantined)
	}
	if err := store.Ack(t.Context(), commit.BatchID); err != nil {
		t.Fatal(err)
	}
	if len(store.Quarantined()) != 0 || store.usedBytes != 0 {
		t.Fatalf("resolved quarantine=%+v used=%d", store.Quarantined(), store.usedBytes)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestWAL(t, path, 1<<20)
	defer store.Close()
	if len(store.Quarantined()) != 0 {
		t.Fatalf("resolved quarantine recovered=%+v", store.Quarantined())
	}
}

func TestWALCompactionPreservesTransitionNewerThanPendingBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.wal")
	store := openTestWAL(t, path, 1<<20)
	commit := testCommit("batch-old", "app", 10)
	if err := store.Commit(t.Context(), commit); err != nil {
		t.Fatal(err)
	}
	next := source.Checkpoint{SourceKey: "app", FileIdentity: "identity-2", OffsetBytes: 0, ObservedAt: time.Now().UTC()}
	if err := store.Transition(t.Context(), &commit.Checkpoint, next); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	err := store.compactLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered := openTestWAL(t, path, 1<<20)
	defer recovered.Close()
	checkpoint, ok := recovered.Checkpoint("app")
	if !ok || !sameCheckpointPosition(checkpoint, next) {
		t.Fatalf("recovered checkpoint = %+v, ok = %v", checkpoint, ok)
	}
	assertPending(t, recovered, []string{"batch-old"})
}
