package sink

import (
	"context"

	"github.com/isyuah/gline/internal/logentry"
)

type EntrySink interface {
	Accept(ctx context.Context, entries []logentry.LogEntry) error
}
