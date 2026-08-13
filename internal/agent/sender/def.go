package sender

import (
	"context"

	"github.com/isyuah/gline/internal/logentry"
)

type Sender interface {
	Run(ctx context.Context, source <-chan logentry.LogEntry) error
}
