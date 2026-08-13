package parser

import (
	"strings"

	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/logentry"
)

type StringLineLogParser struct{}

func NewStringLineLogParser() *StringLineLogParser {
	return &StringLineLogParser{}
}

func ParseLevel(s string) logentry.LogLevel {
	switch s {
	case "INFO":
		return logentry.LevelInfo
	case "WARN":
		return logentry.LevelWarn
	case "ERROR":
		return logentry.LevelError
	case "FATAL":
		return logentry.LevelFatal
	case "DEBUG":
		return logentry.LevelDebug
	case "TRACE":
		return logentry.LevelTrace
	default:
		return logentry.LevelUnknown
	}
}

func (p *StringLineLogParser) Parse(raw source.RawRecord) (logentry.LogEntry, error) {
	levelText, message, found := strings.Cut(raw.Content, " ")
	if !found {
		return logentry.LogEntry{
			Timestamp: raw.ObservedAt,
			Level:     logentry.LevelUnknown,
			Message:   raw.Content,
		}, nil
	}
	level := ParseLevel(levelText)
	if level == logentry.LevelUnknown {
		return logentry.LogEntry{
			Timestamp: raw.ObservedAt,
			Level:     logentry.LevelUnknown,
			Message:   raw.Content,
		}, nil
	}
	return logentry.LogEntry{
		Timestamp: raw.ObservedAt,
		Level:     ParseLevel(levelText),
		Message:   message,
	}, nil
}
