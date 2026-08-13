package sender

import (
	"context"
	"time"

	"github.com/isyuah/gline/internal/agent/destination"
	"github.com/isyuah/gline/internal/logentry"
)

type TickOrBatchSender struct {
	Destination destination.Destination
	Options     TickOrBatchSenderOptions
}

func NewTickOrBatchSender(destination destination.Destination, options TickOrBatchSenderOptions) *TickOrBatchSender {
	return &TickOrBatchSender{destination, options}
}

type TickOrBatchSenderOptions struct {
	BatchSize     int
	FlushInterval time.Duration
}

func (o *TickOrBatchSenderOptions) setDefaults() {
	if o.BatchSize == 0 {
		o.BatchSize = 100
	}
	if o.FlushInterval == 0 {
		o.FlushInterval = 5 * time.Second
	}
}

func (s *TickOrBatchSender) Run(ctx context.Context, source <-chan logentry.LogEntry) error {
	options := s.Options
	options.setDefaults()
	ticker := time.NewTicker(options.FlushInterval)
	defer ticker.Stop()
	batch := make([]logentry.LogEntry, 0)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.Destination.SendEntries(ctx, batch)
		if err != nil {
			return err
		}
		batch = make([]logentry.LogEntry, 0)
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := flush(); err != nil {
				return err
			}
		case entry, ok := <-source:
			if !ok {
				return flush()
			}
			batch = append(batch, entry)
			if len(batch) >= options.BatchSize {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
}
