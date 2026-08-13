package destination

import (
	"context"

	"github.com/isyuah/gline/internal/logentry"
)

type Destination interface {
	SendEntries(ctx context.Context, entries []logentry.LogEntry) error
}
