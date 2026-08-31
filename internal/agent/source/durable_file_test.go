package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDurableFileSourceRotatesAfterStableEOF(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "app.log")
	if err := os.WriteFile(path, []byte("INFO old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewDurableFileSource(DurableFileOptions{Path: path, SourceKey: "app", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	oldRecord, err := source.NextRecord(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(directory, "app.log.1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("INFO new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = source.NextRecord(readCtx)
	var rotation *RotationRequired
	if !errors.As(err, &rotation) {
		t.Fatalf("NextRecord() error = %v, want RotationRequired", err)
	}
	var transitioned Checkpoint
	err = source.Rotate(t.Context(), func(_ context.Context, expected *Checkpoint, next Checkpoint) error {
		if expected == nil || expected.FileIdentity != oldRecord.FileIdentity || expected.OffsetBytes != oldRecord.EndOffset {
			t.Fatalf("rotation expected checkpoint = %+v", expected)
		}
		transitioned = next
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	newRecord, err := source.NextRecord(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if newRecord.Content != "INFO new" || newRecord.FileIdentity != transitioned.FileIdentity || newRecord.FileIdentity == oldRecord.FileIdentity {
		t.Fatalf("new record = %+v, transition = %+v", newRecord, transitioned)
	}
}

func TestDurableFileSourceAppendAndCheckpointRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("INFO first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewDurableFileSource(DurableFileOptions{Path: path, SourceKey: "app", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.NextRecord(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := Checkpoint{SourceKey: first.SourceKey, FileIdentity: first.FileIdentity, OffsetBytes: first.EndOffset, ObservedAt: first.ObservedAt}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("ERROR second\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	source, err = NewDurableFileSource(DurableFileOptions{Path: path, SourceKey: "app", Checkpoint: &checkpoint, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	second, err := source.NextRecord(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.Content != "ERROR second" || second.EndOffset <= first.EndOffset {
		t.Fatalf("second record = %+v, want appended line after offset %d", second, first.EndOffset)
	}
}

func TestDurableFileSourceReadsOrdinaryAppendWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewDurableFileSource(DurableFileOptions{Path: path, SourceKey: "app", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	done := make(chan RawRecord, 1)
	go func() {
		record, readErr := source.NextRecord(t.Context())
		if readErr != nil {
			t.Errorf("NextRecord() error = %v", readErr)
			return
		}
		done <- record
	}()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("INFO appended\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case record := <-done:
		if record.Content != "INFO appended" {
			t.Fatalf("record = %q, want INFO appended", record.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for appended record")
	}
}

func TestDurableFileSourceDetectsCopytruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("INFO a deliberately long original line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewDurableFileSource(DurableFileOptions{Path: path, SourceKey: "app", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	first, err := source.NextRecord(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("WARN new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := source.NextRecord(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.Content != "WARN new" {
		t.Fatalf("copytruncate record = %q, want WARN new", second.Content)
	}
	if second.EndOffset >= first.EndOffset {
		t.Fatalf("copytruncate offset = %d, want new epoch below %d", second.EndOffset, first.EndOffset)
	}
}

func TestDurableFileSourceRestartsAtBeginningOfRecreatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("INFO old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewDurableFileSource(DurableFileOptions{Path: path, SourceKey: "app"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := source.NextRecord(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	checkpoint := Checkpoint{SourceKey: record.SourceKey, FileIdentity: record.FileIdentity, OffsetBytes: record.EndOffset, ObservedAt: record.ObservedAt}
	rotatedPath := path + ".rotated"
	if err := os.Rename(path, rotatedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("INFO new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	newSource, err := NewDurableFileSource(DurableFileOptions{Path: path, SourceKey: "app", Checkpoint: &checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer newSource.Close()
	initial := newSource.InitialCheckpoint()
	if initial.FileIdentity == checkpoint.FileIdentity || initial.OffsetBytes != 0 {
		t.Fatalf("new epoch checkpoint=%+v old=%+v", initial, checkpoint)
	}
	newRecord, err := newSource.NextRecord(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if newRecord.Content != "INFO new" || newRecord.EndOffset != int64(len("INFO new\n")) {
		t.Fatalf("new record=%+v", newRecord)
	}
}
