package sink

import (
	"context"
	"fmt"

	"github.com/isyuah/gline/internal/logentry"
)

type TestSink struct {
}

func (s TestSink) Accept(ctx context.Context, entries []logentry.LogEntry) error {
	fmt.Println("TestSink received entries:", entries)
	return nil
}
