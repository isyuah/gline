package sender

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/isyuah/gline/internal/logentry"
)

type MockDestination struct {
	CountRecord []int
}

func (m *MockDestination) SendEntries(_ context.Context, entries []logentry.LogEntry) error {
	m.CountRecord = append(m.CountRecord, len(entries))
	return nil
}

type MockErrorDestination struct {
	Error error
}

func (m *MockErrorDestination) SendEntries(ctx context.Context, entries []logentry.LogEntry) error {
	return m.Error
}

func SendBatch(c chan<- logentry.LogEntry, times int) {
	for _ = range times {
		c <- logentry.LogEntry{}
	}
}

func TestNewTickOrBatchSender_Run_BatchSizeAndTickAndCloseFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		md := MockDestination{}
		s := NewTickOrBatchSender(&md, TickOrBatchSenderOptions{
			BatchSize:     5,
			FlushInterval: 5 * time.Second,
		})
		src := make(chan logentry.LogEntry, 1000)
		done := make(chan error, 1)
		go func() {
			done <- s.Run(context.Background(), src)
		}()

		SendBatch(src, 7)
		time.Sleep(6 * time.Second)
		SendBatch(src, 3)
		close(src)

		err := <-done
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if diff := cmp.Diff([]int{5, 2, 3}, md.CountRecord); diff != "" {
			t.Fatalf("batch sizes mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestNewTickOrBatchSender_Run_Cancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	md := MockDestination{}
	s := NewTickOrBatchSender(&md, TickOrBatchSenderOptions{
		BatchSize:     5,
		FlushInterval: 5 * time.Second,
	})
	src := make(chan logentry.LogEntry)
	done := make(chan error, 1)
	go func() {
		done <- s.Run(ctx, src)
	}()

	src <- logentry.LogEntry{}
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatal("expected context.Canceled error but got:", err)
	}
	if md.CountRecord != nil {
		t.Fatalf("CountRecord = %v, want nil", md.CountRecord)
	}
}

func TestNewTickOrBatchSender_Run_Error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		testErr := errors.New("test error")
		md := MockErrorDestination{Error: testErr}
		s := NewTickOrBatchSender(&md, TickOrBatchSenderOptions{
			BatchSize:     5,
			FlushInterval: 5 * time.Second,
		})
		src := make(chan logentry.LogEntry)
		done := make(chan error, 1)
		go func() {
			done <- s.Run(t.Context(), src)
		}()

		src <- logentry.LogEntry{}

		err := <-done
		if !errors.Is(err, testErr) {
			t.Fatalf("Run() error = %v, want %v", err, testErr)
		}
	})
}
