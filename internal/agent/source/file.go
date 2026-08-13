package source

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

type FileSource struct {
	Filename string

	file   *os.File
	reader *bufio.Reader

	pending strings.Builder

	pollInterval time.Duration
}

func NewFileSource(filename string) (*FileSource, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	if _, err = file.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(file)
	return &FileSource{filename, file, reader, strings.Builder{}, 1 * time.Second}, nil
}

func (s *FileSource) NextRecord(ctx context.Context) (RawRecord, error) {
	for {
		line, err := s.reader.ReadString('\n')
		if len(line) > 0 {
			s.pending.WriteString(line)
		}
		if err == nil {
			content := strings.TrimSuffix(s.pending.String(), "\n")
			content = strings.TrimSuffix(content, "\r")
			s.pending = strings.Builder{}
			return RawRecord{
				ObservedAt: time.Now(),
				Content:    content,
			}, nil
		}
		if !errors.Is(err, io.EOF) {
			return RawRecord{}, FromErrorFatal(err)
		}
		select {
		case <-ctx.Done():
			return RawRecord{}, ctx.Err()
		case <-time.After(s.pollInterval):
		}
	}
}

func (s *FileSource) Close() error {
	return s.file.Close()
}
