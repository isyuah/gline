package spool

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/isyuah/gline/internal/agent/source"
)

const (
	recordHeaderBytes = 16
	recordVersion     = 1
	recordCommit      = 1
	recordAck         = 2
	recordCheckpoint  = 3
	recordQuarantine  = 4

	defaultMaxBytes       = int64(1 << 30)
	defaultMaxRecordBytes = int64(8 << 20)
)

var (
	ErrFull    = errors.New("agent spool is full")
	ErrCorrupt = errors.New("agent spool is corrupt")
	ErrClosed  = errors.New("agent spool is closed")

	recordMagic = [4]byte{'G', 'L', 'W', '1'}
)

type Config struct {
	Path           string
	MaxBytes       int64
	MaxRecordBytes int64
}

type Commit struct {
	BatchID    string
	Payload    []byte
	Checkpoint source.Checkpoint
}

type commitRecord struct {
	BatchID    string            `json:"batch_id"`
	Payload    json.RawMessage   `json:"payload"`
	Checkpoint source.Checkpoint `json:"checkpoint"`
}

type ackRecord struct {
	BatchID string `json:"batch_id"`
}

type Quarantined struct {
	Commit        Commit
	HTTPCode      int
	ErrorCode     string
	QuarantinedAt time.Time
}

type Stats struct {
	UsedBytes          int64
	PendingBatches     int
	QuarantinedBatches int
	OldestPendingAt    time.Time
}

type quarantineRecord struct {
	BatchID       string    `json:"batch_id"`
	HTTPCode      int       `json:"http_code"`
	ErrorCode     string    `json:"error_code"`
	QuarantinedAt time.Time `json:"quarantined_at"`
}

type WAL struct {
	mu sync.Mutex

	path           string
	file           *os.File
	maxBytes       int64
	maxRecordBytes int64
	usedBytes      int64
	closed         bool

	pending         map[string]Commit
	pendingSize     map[string]int64
	order           []string
	quarantined     map[string]Quarantined
	quarantineOrder []string
	seen            map[string]struct{}
	checkpoints     map[string]source.Checkpoint
	changed         chan struct{}
}

func Open(config Config) (*WAL, error) {
	if strings.TrimSpace(config.Path) == "" {
		return nil, errors.New("spool path is empty")
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = defaultMaxBytes
	}
	if config.MaxRecordBytes <= 0 {
		config.MaxRecordBytes = defaultMaxRecordBytes
	}
	if config.MaxRecordBytes > int64(^uint32(0)) {
		return nil, errors.New("spool max record bytes exceeds WAL format")
	}
	absolute, err := filepath.Abs(config.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve spool path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, fmt.Errorf("create spool directory: %w", err)
	}
	if err := recoverReplacement(absolute); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open spool WAL: %w", err)
	}
	w := &WAL{
		path: absolute, file: file, maxBytes: config.MaxBytes, maxRecordBytes: config.MaxRecordBytes,
		pending: make(map[string]Commit), pendingSize: make(map[string]int64),
		quarantined: make(map[string]Quarantined),
		seen:        make(map[string]struct{}), checkpoints: make(map[string]source.Checkpoint),
		changed: make(chan struct{}, 1),
	}
	if err := w.recover(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("seek spool WAL: %w", err)
	}
	return w, nil
}

func (w *WAL) Commit(ctx context.Context, commit Commit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := encodeCommit(commit)
	if err != nil {
		return err
	}
	if int64(len(body)) > w.maxRecordBytes {
		return fmt.Errorf("commit record contains %d bytes, maximum is %d", len(body), w.maxRecordBytes)
	}
	recordBytes := int64(recordHeaderBytes + len(body))
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if _, exists := w.seen[commit.BatchID]; exists {
		return fmt.Errorf("batch %q is already present in spool", commit.BatchID)
	}
	if recordBytes > w.maxBytes-w.usedBytes {
		return fmt.Errorf("%w: used=%d requested=%d maximum=%d", ErrFull, w.usedBytes, recordBytes, w.maxBytes)
	}
	if err := w.appendRecord(recordCommit, body); err != nil {
		return err
	}
	w.applyCommit(commit, recordBytes)
	w.signal()
	return nil
}

// Ack is the local deletion boundary. It returns only after the ACK record has
// been fsynced. The immutable batch remains pending for every earlier failure.
func (w *WAL) Ack(ctx context.Context, batchID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(ackRecord{BatchID: batchID})
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	_, pending := w.pending[batchID]
	_, quarantined := w.quarantined[batchID]
	if !pending && !quarantined {
		return nil
	}
	if err := w.appendRecord(recordAck, body); err != nil {
		return err
	}
	w.removeActive(batchID)
	w.signal()
	if info, statErr := w.file.Stat(); statErr == nil && info.Size() > maxInt64(w.maxBytes, 64<<10) {
		// Compaction is reclaim-only. The ACK is already durable, so a failed
		// compaction must not make the caller resend a batch indefinitely.
		if compactErr := w.compactLocked(); compactErr != nil && w.closed {
			return compactErr
		}
	}
	return nil
}

// Quarantine is a durable move from the delivery queue to the local dead-letter
// set. The payload continues to count against MaxBytes until an operator resolves
// it with Ack, so repeated permanent failures eventually apply backpressure.
func (w *WAL) Quarantine(ctx context.Context, batchID string, httpCode int, errorCode string, at time.Time) (Quarantined, error) {
	if err := ctx.Err(); err != nil {
		return Quarantined{}, err
	}
	if strings.TrimSpace(batchID) == "" || httpCode < 400 || at.IsZero() {
		return Quarantined{}, errors.New("invalid spool quarantine record")
	}
	record := quarantineRecord{BatchID: batchID, HTTPCode: httpCode, ErrorCode: strings.TrimSpace(errorCode), QuarantinedAt: at.UTC()}
	body, err := json.Marshal(record)
	if err != nil {
		return Quarantined{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return Quarantined{}, ErrClosed
	}
	commit, exists := w.pending[batchID]
	if !exists {
		if value, ok := w.quarantined[batchID]; ok {
			return cloneQuarantined(value), nil
		}
		return Quarantined{}, fmt.Errorf("quarantine references unknown batch %q", batchID)
	}
	if err := w.appendRecord(recordQuarantine, body); err != nil {
		return Quarantined{}, err
	}
	value := Quarantined{Commit: cloneCommit(commit), HTTPCode: httpCode, ErrorCode: record.ErrorCode, QuarantinedAt: record.QuarantinedAt}
	w.applyQuarantine(value)
	w.signal()
	return cloneQuarantined(value), nil
}

func (w *WAL) Pending() []Commit {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]Commit, 0, len(w.pending))
	for _, id := range w.order {
		if commit, exists := w.pending[id]; exists {
			result = append(result, cloneCommit(commit))
		}
	}
	return result
}

func (w *WAL) Quarantined() []Quarantined {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]Quarantined, 0, len(w.quarantined))
	for _, id := range w.quarantineOrder {
		if value, exists := w.quarantined[id]; exists {
			result = append(result, cloneQuarantined(value))
		}
	}
	return result
}

func (w *WAL) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	stats := Stats{
		UsedBytes: w.usedBytes, PendingBatches: len(w.pending),
		QuarantinedBatches: len(w.quarantined),
	}
	for _, commit := range w.pending {
		observedAt := commit.Checkpoint.ObservedAt
		if observedAt.IsZero() {
			continue
		}
		if stats.OldestPendingAt.IsZero() || observedAt.Before(stats.OldestPendingAt) {
			stats.OldestPendingAt = observedAt
		}
	}
	return stats
}

func (w *WAL) Checkpoint(sourceKey string) (source.Checkpoint, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	checkpoint, ok := w.checkpoints[sourceKey]
	return checkpoint, ok
}

// Transition persists a no-data checkpoint state change such as initial file
// anchoring or rename/recreate rotation. expected is a compare-and-set guard;
// nil means the source must not have a durable checkpoint yet.
func (w *WAL) Transition(ctx context.Context, expected *source.Checkpoint, next source.Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateCheckpoint(next); err != nil {
		return err
	}
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	current, exists := w.checkpoints[next.SourceKey]
	if expected == nil {
		if exists {
			return fmt.Errorf("checkpoint transition conflict for source %q", next.SourceKey)
		}
	} else if !exists || !sameCheckpointPosition(current, *expected) {
		return fmt.Errorf("checkpoint transition conflict for source %q", next.SourceKey)
	}
	if err := w.appendRecord(recordCheckpoint, body); err != nil {
		return err
	}
	w.checkpoints[next.SourceKey] = next
	w.signal()
	return nil
}

func sameCheckpointPosition(left, right source.Checkpoint) bool {
	return left.SourceKey == right.SourceKey && left.FileIdentity == right.FileIdentity && left.OffsetBytes == right.OffsetBytes
}

func (w *WAL) WaitForSpace(ctx context.Context, recordBytes int64) error {
	for {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return ErrClosed
		}
		if recordBytes <= w.maxBytes-w.usedBytes {
			w.mu.Unlock()
			return nil
		}
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (w *WAL) Changed() <-chan struct{} { return w.changed }

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.signal()
	return w.file.Close()
}

func (w *WAL) recover() error {
	info, err := w.file.Stat()
	if err != nil {
		return fmt.Errorf("stat spool WAL: %w", err)
	}
	size := info.Size()
	var offset int64
	header := make([]byte, recordHeaderBytes)
	for offset < size {
		remaining := size - offset
		if remaining < recordHeaderBytes {
			return w.truncateTail(offset)
		}
		if _, err := w.file.ReadAt(header, offset); err != nil {
			return fmt.Errorf("read spool record header at %d: %w", offset, err)
		}
		kind, length, checksum, err := parseHeader(header)
		if err != nil {
			return fmt.Errorf("%w at offset %d: %v", ErrCorrupt, offset, err)
		}
		if int64(length) > w.maxRecordBytes {
			return fmt.Errorf("%w at offset %d: record length %d exceeds maximum %d", ErrCorrupt, offset, length, w.maxRecordBytes)
		}
		total := int64(recordHeaderBytes) + int64(length)
		if remaining < total {
			return w.truncateTail(offset)
		}
		body := make([]byte, int(length))
		if _, err := w.file.ReadAt(body, offset+recordHeaderBytes); err != nil {
			return fmt.Errorf("read spool record body at %d: %w", offset, err)
		}
		if recordChecksum(kind, body) != checksum {
			return fmt.Errorf("%w at offset %d: checksum mismatch", ErrCorrupt, offset)
		}
		if err := w.applyRecovered(kind, body, total); err != nil {
			return fmt.Errorf("%w at offset %d: %v", ErrCorrupt, offset, err)
		}
		offset += total
	}
	return nil
}

func (w *WAL) applyRecovered(kind byte, body []byte, recordBytes int64) error {
	switch kind {
	case recordCommit:
		commit, err := decodeCommit(body)
		if err != nil {
			return err
		}
		if _, exists := w.seen[commit.BatchID]; exists {
			return fmt.Errorf("duplicate commit for batch %q", commit.BatchID)
		}
		w.applyCommit(commit, recordBytes)
	case recordAck:
		var ack ackRecord
		if err := decodeStrict(body, &ack); err != nil {
			return fmt.Errorf("decode ACK: %w", err)
		}
		_, pending := w.pending[ack.BatchID]
		_, quarantined := w.quarantined[ack.BatchID]
		if !pending && !quarantined {
			return fmt.Errorf("ACK references unknown batch %q", ack.BatchID)
		}
		w.removeActive(ack.BatchID)
	case recordCheckpoint:
		var checkpoint source.Checkpoint
		if err := decodeStrict(body, &checkpoint); err != nil {
			return fmt.Errorf("decode checkpoint snapshot: %w", err)
		}
		if err := validateCheckpoint(checkpoint); err != nil {
			return err
		}
		w.checkpoints[checkpoint.SourceKey] = checkpoint
	case recordQuarantine:
		var record quarantineRecord
		if err := decodeStrict(body, &record); err != nil {
			return fmt.Errorf("decode quarantine: %w", err)
		}
		commit, exists := w.pending[record.BatchID]
		if !exists || record.HTTPCode < 400 || record.QuarantinedAt.IsZero() {
			return fmt.Errorf("quarantine references invalid batch %q", record.BatchID)
		}
		w.applyQuarantine(Quarantined{
			Commit: commit, HTTPCode: record.HTTPCode, ErrorCode: record.ErrorCode,
			QuarantinedAt: record.QuarantinedAt,
		})
	default:
		return fmt.Errorf("unsupported record kind %d", kind)
	}
	return nil
}

func (w *WAL) applyCommit(commit Commit, recordBytes int64) {
	cloned := cloneCommit(commit)
	w.pending[commit.BatchID] = cloned
	w.pendingSize[commit.BatchID] = recordBytes
	w.order = append(w.order, commit.BatchID)
	w.seen[commit.BatchID] = struct{}{}
	w.checkpoints[commit.Checkpoint.SourceKey] = commit.Checkpoint
	w.usedBytes += recordBytes
}

func (w *WAL) removePending(batchID string) {
	delete(w.pending, batchID)
	w.usedBytes -= w.pendingSize[batchID]
	delete(w.pendingSize, batchID)
}

func (w *WAL) applyQuarantine(value Quarantined) {
	id := value.Commit.BatchID
	delete(w.pending, id)
	w.quarantined[id] = cloneQuarantined(value)
	w.quarantineOrder = append(w.quarantineOrder, id)
}

func (w *WAL) removeActive(batchID string) {
	delete(w.pending, batchID)
	delete(w.quarantined, batchID)
	w.usedBytes -= w.pendingSize[batchID]
	delete(w.pendingSize, batchID)
}

func (w *WAL) appendRecord(kind byte, body []byte) error {
	start, err := w.file.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek spool WAL: %w", err)
	}
	record, err := frameRecord(kind, body)
	if err != nil {
		return err
	}
	if err := writeAll(w.file, record); err != nil {
		return w.rollbackAppend(start, fmt.Errorf("append spool record: %w", err))
	}
	if err := w.file.Sync(); err != nil {
		return w.rollbackAppend(start, fmt.Errorf("sync spool record: %w", err))
	}
	return nil
}

func (w *WAL) rollbackAppend(offset int64, cause error) error {
	truncateErr := w.file.Truncate(offset)
	_, seekErr := w.file.Seek(0, io.SeekEnd)
	syncErr := w.file.Sync()
	return errors.Join(cause, truncateErr, seekErr, syncErr)
}

func (w *WAL) truncateTail(offset int64) error {
	if err := w.file.Truncate(offset); err != nil {
		return fmt.Errorf("truncate incomplete WAL tail: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("sync truncated WAL tail: %w", err)
	}
	return nil
}

func (w *WAL) compactLocked() error {
	temporary := w.path + ".compact"
	backup := w.path + ".bak"
	_ = os.Remove(temporary)
	tempFile, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	writeRecord := func(kind byte, body []byte) error {
		record, err := frameRecord(kind, body)
		if err != nil {
			return err
		}
		return writeAll(tempFile, record)
	}
	for _, id := range w.order {
		commit, exists := w.pending[id]
		if !exists {
			continue
		}
		body, marshalErr := encodeCommit(commit)
		if marshalErr != nil {
			_ = tempFile.Close()
			_ = os.Remove(temporary)
			return marshalErr
		}
		if err := writeRecord(recordCommit, body); err != nil {
			_ = tempFile.Close()
			_ = os.Remove(temporary)
			return err
		}
	}
	for _, id := range w.quarantineOrder {
		value, exists := w.quarantined[id]
		if !exists {
			continue
		}
		body, marshalErr := encodeCommit(value.Commit)
		if marshalErr != nil {
			_ = tempFile.Close()
			_ = os.Remove(temporary)
			return marshalErr
		}
		if err := writeRecord(recordCommit, body); err != nil {
			_ = tempFile.Close()
			_ = os.Remove(temporary)
			return err
		}
		recordBody, marshalErr := json.Marshal(quarantineRecord{
			BatchID: id, HTTPCode: value.HTTPCode, ErrorCode: value.ErrorCode, QuarantinedAt: value.QuarantinedAt,
		})
		if marshalErr != nil {
			_ = tempFile.Close()
			_ = os.Remove(temporary)
			return marshalErr
		}
		if err := writeRecord(recordQuarantine, recordBody); err != nil {
			_ = tempFile.Close()
			_ = os.Remove(temporary)
			return err
		}
	}
	// Checkpoint snapshots follow pending commits so a later no-data epoch
	// transition cannot be rolled back by replaying an older pending batch.
	keys := make([]string, 0, len(w.checkpoints))
	for key := range w.checkpoints {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		body, marshalErr := json.Marshal(w.checkpoints[key])
		if marshalErr != nil {
			_ = tempFile.Close()
			_ = os.Remove(temporary)
			return marshalErr
		}
		if err := writeRecord(recordCheckpoint, body); err != nil {
			_ = tempFile.Close()
			_ = os.Remove(temporary)
			return err
		}
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := w.file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	_ = os.Remove(backup)
	if err := os.Rename(w.path, backup); err != nil {
		w.file, _ = os.OpenFile(w.path, os.O_RDWR, 0o600)
		w.closed = w.file == nil
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, w.path); err != nil {
		_ = os.Rename(backup, w.path)
		w.file, _ = os.OpenFile(w.path, os.O_RDWR, 0o600)
		w.closed = w.file == nil
		return err
	}
	_ = syncParent(w.path)
	_ = os.Remove(backup)
	w.file, err = os.OpenFile(w.path, os.O_RDWR, 0o600)
	if err != nil {
		w.closed = true
		return err
	}
	_, err = w.file.Seek(0, io.SeekEnd)
	return err
}

func encodeCommit(commit Commit) ([]byte, error) {
	if strings.TrimSpace(commit.BatchID) == "" {
		return nil, errors.New("spool commit batch ID is empty")
	}
	if !json.Valid(commit.Payload) {
		return nil, errors.New("spool commit payload is not valid JSON")
	}
	var envelope struct {
		BatchID string `json:"batch_id"`
	}
	if err := json.Unmarshal(commit.Payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode batch identity: %w", err)
	}
	if envelope.BatchID != commit.BatchID {
		return nil, errors.New("spool commit batch ID does not match payload")
	}
	if err := validateCheckpoint(commit.Checkpoint); err != nil {
		return nil, err
	}
	return json.Marshal(commitRecord{
		BatchID: commit.BatchID, Payload: append(json.RawMessage(nil), commit.Payload...),
		Checkpoint: commit.Checkpoint,
	})
}

func decodeCommit(body []byte) (Commit, error) {
	var record commitRecord
	if err := decodeStrict(body, &record); err != nil {
		return Commit{}, fmt.Errorf("decode commit: %w", err)
	}
	commit := Commit{BatchID: record.BatchID, Payload: append([]byte(nil), record.Payload...), Checkpoint: record.Checkpoint}
	if _, err := encodeCommit(commit); err != nil {
		return Commit{}, err
	}
	return commit, nil
}

func cloneQuarantined(value Quarantined) Quarantined {
	value.Commit = cloneCommit(value.Commit)
	return value
}

func validateCheckpoint(checkpoint source.Checkpoint) error {
	if strings.TrimSpace(checkpoint.SourceKey) == "" || strings.TrimSpace(checkpoint.FileIdentity) == "" {
		return errors.New("checkpoint source key and file identity are required")
	}
	if checkpoint.OffsetBytes < 0 {
		return errors.New("checkpoint offset is negative")
	}
	if checkpoint.ObservedAt.IsZero() {
		return errors.New("checkpoint observed time is zero")
	}
	return nil
}

func decodeStrict(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

func frameRecord(kind byte, body []byte) ([]byte, error) {
	if len(body) > int(^uint32(0)) {
		return nil, errors.New("WAL record exceeds format length")
	}
	record := make([]byte, recordHeaderBytes+len(body))
	copy(record[:4], recordMagic[:])
	record[4] = recordVersion
	record[5] = kind
	binary.BigEndian.PutUint32(record[8:12], uint32(len(body)))
	binary.BigEndian.PutUint32(record[12:16], recordChecksum(kind, body))
	copy(record[recordHeaderBytes:], body)
	return record, nil
}

func parseHeader(header []byte) (byte, uint32, uint32, error) {
	if len(header) != recordHeaderBytes || !bytes.Equal(header[:4], recordMagic[:]) {
		return 0, 0, 0, errors.New("invalid record magic")
	}
	if header[4] != recordVersion {
		return 0, 0, 0, fmt.Errorf("unsupported record version %d", header[4])
	}
	if header[6] != 0 || header[7] != 0 {
		return 0, 0, 0, errors.New("reserved record bytes are not zero")
	}
	return header[5], binary.BigEndian.Uint32(header[8:12]), binary.BigEndian.Uint32(header[12:16]), nil
}

func recordChecksum(kind byte, body []byte) uint32 {
	hash := crc32.NewIEEE()
	_, _ = hash.Write([]byte{kind})
	_, _ = hash.Write(body)
	return hash.Sum32()
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func recoverReplacement(path string) error {
	backup := path + ".bak"
	_, pathErr := os.Stat(path)
	_, backupErr := os.Stat(backup)
	switch {
	case errors.Is(pathErr, os.ErrNotExist) && backupErr == nil:
		if err := os.Rename(backup, path); err != nil {
			return fmt.Errorf("restore spool backup: %w", err)
		}
	case pathErr == nil && backupErr == nil:
		if err := os.Remove(backup); err != nil {
			return fmt.Errorf("remove stale spool backup: %w", err)
		}
	case pathErr != nil && !errors.Is(pathErr, os.ErrNotExist):
		return pathErr
	case backupErr != nil && !errors.Is(backupErr, os.ErrNotExist):
		return backupErr
	}
	_ = os.Remove(path + ".compact")
	return nil
}

func syncParent(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func cloneCommit(commit Commit) Commit {
	commit.Payload = append([]byte(nil), commit.Payload...)
	return commit
}

func (w *WAL) signal() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
