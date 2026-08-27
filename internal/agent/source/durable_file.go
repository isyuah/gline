package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrFileIdentityChanged   = errors.New("file identity changed")
	ErrRotationPartialRecord = errors.New("cannot rotate while the old file has an unterminated record")
)

type RotationRequired struct{ Path string }

func (e *RotationRequired) Error() string {
	return fmt.Sprintf("%s: %s", ErrFileIdentityChanged, e.Path)
}
func (e *RotationRequired) Unwrap() error { return ErrFileIdentityChanged }

type Checkpoint struct {
	SourceKey    string    `json:"source_key"`
	FileIdentity string    `json:"file_identity"`
	OffsetBytes  int64     `json:"offset_bytes"`
	ObservedAt   time.Time `json:"observed_at"`
}

type DurableFileOptions struct {
	Path         string
	SourceKey    string
	Checkpoint   *Checkpoint
	PollInterval time.Duration
}

// DurableFileSource exposes the byte offset only after a complete newline-
// terminated record has been read. The caller owns checkpoint persistence.
type DurableFileSource struct {
	path         string
	sourceKey    string
	identity     string
	file         *os.File
	reader       *bufio.Reader
	pending      strings.Builder
	readOffset   int64
	pollInterval time.Duration
	checkpointAt time.Time
	rotationSeen int
}

func NewDurableFileSource(options DurableFileOptions) (*DurableFileSource, error) {
	if strings.TrimSpace(options.Path) == "" {
		return nil, errors.New("durable file source path is empty")
	}
	if strings.TrimSpace(options.SourceKey) == "" {
		return nil, errors.New("durable file source key is empty")
	}
	absolute, err := filepath.Abs(options.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve durable source path: %w", err)
	}
	file, err := openReadFile(absolute)
	if err != nil {
		return nil, fmt.Errorf("open durable source %q: %w", absolute, err)
	}
	closeWithError := func(cause error) (*DurableFileSource, error) {
		if closeErr := file.Close(); closeErr != nil {
			return nil, errors.Join(cause, closeErr)
		}
		return nil, cause
	}
	info, err := file.Stat()
	if err != nil {
		return closeWithError(fmt.Errorf("stat durable source: %w", err))
	}
	identity, err := persistentFileIdentity(absolute, file, info)
	if err != nil {
		return closeWithError(err)
	}
	offset := int64(0)
	checkpointAt := time.Now().UTC()
	if checkpoint := options.Checkpoint; checkpoint != nil {
		if checkpoint.SourceKey != options.SourceKey {
			return closeWithError(fmt.Errorf("checkpoint source key %q does not match %q", checkpoint.SourceKey, options.SourceKey))
		}
		if checkpoint.OffsetBytes < 0 {
			return closeWithError(errors.New("checkpoint offset is negative"))
		}
		if checkpoint.FileIdentity == identity {
			if info.Size() >= checkpoint.OffsetBytes {
				offset = checkpoint.OffsetBytes
			}
			checkpointAt = checkpoint.ObservedAt
			// A smaller file with the same identity is copytruncate. Restart at
			// zero; the next committed checkpoint begins a new byte epoch.
		}
		// A different identity means rename/recreate happened while the Agent
		// was stopped. Return the new zero-offset epoch; the build layer must CAS
		// it into the WAL before this source starts reading.
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return closeWithError(fmt.Errorf("seek durable source: %w", err))
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	return &DurableFileSource{
		path: absolute, sourceKey: options.SourceKey, identity: identity,
		file: file, reader: bufio.NewReader(file), readOffset: offset,
		pollInterval: options.PollInterval, checkpointAt: checkpointAt,
	}, nil
}

func (s *DurableFileSource) InitialCheckpoint() Checkpoint {
	return Checkpoint{SourceKey: s.sourceKey, FileIdentity: s.identity, OffsetBytes: s.readOffset, ObservedAt: s.checkpointAt}
}

func (s *DurableFileSource) NextRecord(ctx context.Context) (RawRecord, error) {
	for {
		line, err := s.reader.ReadString('\n')
		if len(line) > 0 {
			s.pending.WriteString(line)
			s.readOffset += int64(len(line))
		}
		if err == nil {
			content := strings.TrimSuffix(s.pending.String(), "\n")
			content = strings.TrimSuffix(content, "\r")
			s.pending.Reset()
			observedAt := time.Now().UTC()
			s.checkpointAt = observedAt
			s.rotationSeen = 0
			return RawRecord{
				ObservedAt: observedAt, Content: content,
				SourceKey: s.sourceKey, FileIdentity: s.identity, EndOffset: s.readOffset,
			}, nil
		}
		if !errors.Is(err, io.EOF) {
			return RawRecord{}, FromErrorFatal(err)
		}
		if err := s.detectRotation(); err != nil {
			return RawRecord{}, FromErrorFatal(err)
		}
		timer := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return RawRecord{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *DurableFileSource) detectRotation() error {
	openedInfo, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("stat open durable source: %w", err)
	}
	pathInfo, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("stat durable source path: %w", err)
	}
	if !os.SameFile(openedInfo, pathInfo) {
		if s.pending.Len() > 0 {
			return ErrRotationPartialRecord
		}
		s.rotationSeen++
		if s.rotationSeen >= 2 {
			return &RotationRequired{Path: s.path}
		}
		return nil
	}
	s.rotationSeen = 0
	if pathInfo.Size() >= s.readOffset {
		return nil
	}
	// copytruncate keeps the file identity but moves EOF behind the current
	// position. Discard any unterminated fragment and begin the new epoch.
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek copytruncated source: %w", err)
	}
	s.reader.Reset(s.file)
	s.pending.Reset()
	s.readOffset = 0
	return nil
}

// Rotate switches to a rename/recreate replacement only after persist commits
// the checkpoint epoch transition. The old handle is kept until that durable
// boundary succeeds.
func (s *DurableFileSource) Rotate(ctx context.Context, persist func(context.Context, *Checkpoint, Checkpoint) error) error {
	if persist == nil {
		return errors.New("rotation checkpoint persistence is required")
	}
	if s.pending.Len() > 0 {
		return ErrRotationPartialRecord
	}
	file, err := openReadFile(s.path)
	if err != nil {
		return fmt.Errorf("open rotated source: %w", err)
	}
	closeOnError := func(cause error) error {
		return errors.Join(cause, file.Close())
	}
	info, err := file.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("stat rotated source: %w", err))
	}
	openedInfo, err := s.file.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("stat previous source: %w", err))
	}
	if os.SameFile(openedInfo, info) {
		return closeOnError(errors.New("rotation candidate reverted to the current file"))
	}
	identity, err := persistentFileIdentity(s.path, file, info)
	if err != nil {
		return closeOnError(err)
	}
	expected := Checkpoint{SourceKey: s.sourceKey, FileIdentity: s.identity, OffsetBytes: s.readOffset, ObservedAt: s.checkpointAt}
	next := Checkpoint{SourceKey: s.sourceKey, FileIdentity: identity, OffsetBytes: 0, ObservedAt: time.Now().UTC()}
	if err := persist(ctx, &expected, next); err != nil {
		return closeOnError(err)
	}
	old := s.file
	s.file = file
	s.reader.Reset(file)
	s.identity = identity
	s.readOffset = 0
	s.checkpointAt = next.ObservedAt
	s.rotationSeen = 0
	if err := old.Close(); err != nil {
		return fmt.Errorf("close rotated source: %w", err)
	}
	return nil
}

func (s *DurableFileSource) Close() error { return s.file.Close() }
