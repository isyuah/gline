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
	attempt := 0
	currentBatch := ""
	for {
		pending := d.spool.Pending()
		if len(pending) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-d.spool.Changed():
				continue
			}
		}
		commit := pending[0]
		if commit.BatchID != currentBatch {
			currentBatch = commit.BatchID
			attempt = 0
		}
		result, err := d.transport.Send(ctx, commit.Payload)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && result.Class == 0 {
			result.Class = ResultRetryable
		}
		switch result.Class {
		case ResultAccepted, ResultDuplicate:
			if err := d.spool.Ack(ctx, commit.BatchID); err != nil {
				return fmt.Errorf("persist ACK for batch %s: %w", commit.BatchID, err)
			}
			currentBatch = ""
			attempt = 0
		case ResultRetryable:
			attempt++
			delay := Backoff(attempt, d.options.BaseDelay, d.options.MaxDelay, d.options.Jitter, d.options.Random)
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			if delay > d.options.MaxDelay {
				delay = d.options.MaxDelay
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		case ResultQuarantine:
			quarantined, quarantineErr := d.spool.Quarantine(ctx, commit.BatchID, result.StatusCode, result.Code, d.options.Clock())
			if quarantineErr != nil {
				return fmt.Errorf("persist quarantine for batch %s: %w", commit.BatchID, quarantineErr)
			}
			if d.options.OnQuarantine != nil {
				d.options.OnQuarantine(quarantined)
			}
			currentBatch = ""
			attempt = 0
		case ResultTerminal:
			return &TerminalError{BatchID: commit.BatchID, HTTPCode: result.StatusCode, ErrorCode: result.Code}
		default:
			return fmt.Errorf("transport returned unknown result class %d", result.Class)
		}
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
