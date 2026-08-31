package reliable

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/isyuah/gline/internal/agent/spool"
)

type DispatcherOptions struct {
	BaseDelay    time.Duration
	MaxDelay     time.Duration
	Jitter       float64
	Random       *rand.Rand
	Clock        func() time.Time
	OnQuarantine func(spool.Quarantined)
	Observer     DeliveryObserver
}

type DeliveryObserver interface {
	ObserveDelivery(result string, duration time.Duration)
}

type TerminalError struct {
	BatchID   string
	HTTPCode  int
	ErrorCode string
}

func (e *TerminalError) Error() string {
	return fmt.Sprintf("batch %s reached terminal delivery result: http=%d code=%s", e.BatchID, e.HTTPCode, e.ErrorCode)
}

type Dispatcher struct {
	spool     *spool.WAL
	transport Transport
	options   DispatcherOptions
}

func NewDispatcher(store *spool.WAL, transport Transport, options DispatcherOptions) (*Dispatcher, error) {
	if store == nil || transport == nil {
		return nil, errors.New("dispatcher requires spool and transport")
	}
	if options.BaseDelay <= 0 {
		options.BaseDelay = time.Second
	}
	if options.MaxDelay <= 0 {
		options.MaxDelay = time.Minute
	}
	if options.MaxDelay < options.BaseDelay {
		return nil, errors.New("dispatcher max delay is less than base delay")
	}
	if options.Jitter < 0 || options.Jitter > 1 {
		return nil, errors.New("dispatcher jitter must be between zero and one")
	}
	if options.Random == nil {
		options.Random = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &Dispatcher{spool: store, transport: transport, options: options}, nil
}

func (d *Dispatcher) Run(ctx context.Context) error {
	attempts := make(map[string]int)
	blockedUntil := make(map[string]time.Time)
	for {
		commit, wait := d.nextPending(blockedUntil)
		if commit == nil {
			if err := waitForDispatcherEvent(ctx, d.spool.Changed(), wait); err != nil {
				return err
			}
			continue
		}
		started := time.Now()
		result, err := d.transport.Send(ctx, commit.Payload)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && result.Class == 0 {
			result.Class = ResultRetryable
		}
		if d.options.Observer != nil {
			d.options.Observer.ObserveDelivery(resultLabel(result.Class), time.Since(started))
		}
		switch result.Class {
		case ResultAccepted, ResultDuplicate:
			if err := d.spool.Ack(ctx, commit.BatchID); err != nil {
				return fmt.Errorf("persist ACK for batch %s: %w", commit.BatchID, err)
			}
			delete(attempts, commit.BatchID)
			delete(blockedUntil, commit.BatchID)
		case ResultRetryable:
			attempts[commit.BatchID]++
			delay := Backoff(attempts[commit.BatchID], d.options.BaseDelay, d.options.MaxDelay, d.options.Jitter, d.options.Random)
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			if delay > d.options.MaxDelay {
				delay = d.options.MaxDelay
			}
			blockedUntil[commit.BatchID] = d.options.Clock().Add(delay)
		case ResultBlocked:
			attempts[commit.BatchID]++
			delay := Backoff(attempts[commit.BatchID], d.options.BaseDelay, d.options.MaxDelay, d.options.Jitter, d.options.Random)
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			if delay > d.options.MaxDelay {
				delay = d.options.MaxDelay
			}
			blockedUntil[commit.BatchID] = d.options.Clock().Add(delay)
		case ResultQuarantine:
			quarantined, quarantineErr := d.spool.Quarantine(ctx, commit.BatchID, result.StatusCode, result.Code, d.options.Clock())
			if quarantineErr != nil {
				return fmt.Errorf("persist quarantine for batch %s: %w", commit.BatchID, quarantineErr)
			}
			if d.options.OnQuarantine != nil {
				d.options.OnQuarantine(quarantined)
			}
			delete(attempts, commit.BatchID)
			delete(blockedUntil, commit.BatchID)
		case ResultTerminal:
			return &TerminalError{BatchID: commit.BatchID, HTTPCode: result.StatusCode, ErrorCode: result.Code}
		default:
			return fmt.Errorf("transport returned unknown result class %d", result.Class)
		}
	}
}

func (d *Dispatcher) nextPending(blockedUntil map[string]time.Time) (*spool.Commit, time.Duration) {
	if len(blockedUntil) == 0 {
		commit, exists := d.spool.Peek()
		if !exists {
			return nil, 0
		}
		return &commit, 0
	}
	now := d.options.Clock()
	pending := d.spool.Pending()
	if len(pending) == 0 {
		return nil, 0
	}
	var earliest time.Time
	for index := range pending {
		commit := pending[index]
		blocked, exists := blockedUntil[commit.BatchID]
		if !exists || !blocked.After(now) {
			return &commit, 0
		}
		if earliest.IsZero() || blocked.Before(earliest) {
			earliest = blocked
		}
	}
	return nil, earliest.Sub(now)
}

func waitForDispatcherEvent(ctx context.Context, changed <-chan struct{}, wait time.Duration) error {
	if wait <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
			return nil
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-timer.C:
		return nil
	}
}

func resultLabel(class ResultClass) string {
	switch class {
	case ResultAccepted:
		return "accepted"
	case ResultDuplicate:
		return "duplicate"
	case ResultRetryable:
		return "retryable"
	case ResultQuarantine:
		return "quarantined"
	case ResultTerminal:
		return "terminal"
	case ResultBlocked:
		return "blocked"
	default:
		return "invalid"
	}
}

func Backoff(attempt int, base, maximum time.Duration, jitter float64, random *rand.Rand) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for index := 1; index < attempt && delay < maximum; index++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	if jitter <= 0 || random == nil {
		return delay
	}
	factor := 1 + (random.Float64()*2-1)*jitter
	jittered := time.Duration(float64(delay) * factor)
	if jittered < 0 {
		return 0
	}
	if jittered > maximum {
		return maximum
	}
	return jittered
}
