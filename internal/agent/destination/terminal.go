package destination

import (
	"context"
	"fmt"

	"github.com/isyuah/gline/internal/logentry"
)

type TerminalDestination struct{}

func NewTerminalDestination() *TerminalDestination {
	return &TerminalDestination{}
}

func (d *TerminalDestination) SendEntries(ctx context.Context, entries []logentry.LogEntry) error {
	for _, entry := range entries {
		fmt.Println(entry)
	}
	return nil
}
