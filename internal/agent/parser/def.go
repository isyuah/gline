package parser

import (
	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/logentry"
)

type Parser interface {
	Parse(raw source.RawRecord) (logentry.LogEntry, error)
}
