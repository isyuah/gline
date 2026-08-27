package admission

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

var (
	ErrLimited              = errors.New("admission limit exceeded")
	ErrBatchExceedsCapacity = errors.New("batch cost exceeds admission capacity")
)

type Reason string

const (
	ReasonKeyRate         Reason = "key_rate"
	ReasonProjectEntries  Reason = "project_entries"
	ReasonProjectBytes    Reason = "project_bytes"
	ReasonProjectInflight Reason = "project_inflight"
)

func (r Reason) valid() bool {
	switch r {
	case ReasonKeyRate, ReasonProjectEntries, ReasonProjectBytes, ReasonProjectInflight:
		return true
	default:
		return false
	}
}

type LimitError struct {
	Reason     Reason
	RetryAfter time.Duration
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s: retry after %s", ErrLimited, e.RetryAfter.Round(time.Millisecond))
}

func (e *LimitError) Unwrap() error { return ErrLimited }

type Config struct {
	RequestsPerMinute int64
	EntriesPerMinute  int64
	BytesPerMinute    int64
	MaxInflight       int
	StateTTL          time.Duration
}

type Observer interface {
	ObserveAdmission(result, reason string)
	AddAdmissionInflight(delta float64)
}

type Reservation interface {
	Commit()
	Release()
}

type Option func(*Limiter)

func WithClock(clock func() time.Time) Option {
	return func(limiter *Limiter) { limiter.now = clock }
}

type Limiter struct {
	mu          sync.Mutex
	config      Config
	observer    Observer
	now         func() time.Time
	lastCleanup time.Time
	keys        map[domain.APIKeyID]*keyState
	projects    map[domain.ProjectID]*projectState
}

type keyState struct {
	requests tokenBucket
	lastSeen time.Time
}

type projectState struct {
	entries  tokenBucket
	bytes    tokenBucket
	inflight int
	lastSeen time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func New(config Config, observer Observer, options ...Option) (*Limiter, error) {
	if config.RequestsPerMinute <= 0 || config.EntriesPerMinute <= 0 || config.BytesPerMinute <= 0 ||
		config.MaxInflight <= 0 {
		return nil, errors.New("admission limits must be positive")
	}
	if config.RequestsPerMinute > 1_000_000 || config.EntriesPerMinute > 1_000_000_000 ||
		config.BytesPerMinute > 1<<50 || config.MaxInflight > 10_000 {
		return nil, errors.New("admission limits exceed supported bounds")
	}
	if config.StateTTL <= 0 {
		config.StateTTL = 10 * time.Minute
	}
	if config.StateTTL < 2*time.Minute {
		return nil, errors.New("admission state TTL must be at least two minutes")
	}
	limiter := &Limiter{
		config: config, observer: observer, now: time.Now,
		keys: make(map[domain.APIKeyID]*keyState), projects: make(map[domain.ProjectID]*projectState),
	}
	for _, option := range options {
		if option != nil {
			option(limiter)
		}
	}
	if limiter.now == nil {
		return nil, errors.New("admission clock is nil")
	}
	return limiter, nil
}

func (l *Limiter) AllowIngest(ctx context.Context, keyID domain.APIKeyID, projectID domain.ProjectID, entries int, payloadBytes int64) (Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !keyID.Valid() || !projectID.Valid() || entries <= 0 || payloadBytes <= 0 {
		return nil, fmt.Errorf("%w: invalid admission request", domain.ErrInvalid)
	}
	if int64(entries) > l.config.EntriesPerMinute || payloadBytes > l.config.BytesPerMinute {
		return nil, ErrBatchExceedsCapacity
	}

	now := l.now().UTC()
	l.mu.Lock()
	l.cleanupLocked(now)
	key := l.keys[keyID]
	if key == nil {
		key = &keyState{requests: fullBucket(l.config.RequestsPerMinute, now)}
		l.keys[keyID] = key
	}
	refill(&key.requests, l.config.RequestsPerMinute, now)
	key.lastSeen = now
	if key.requests.tokens < 1 {
		retry := retryAfter(key.requests.tokens, 1, l.config.RequestsPerMinute)
		l.mu.Unlock()
		return nil, l.rejected(ReasonKeyRate, retry)
	}
	// Every authenticated attempt consumes request-rate capacity, including a
	// project-budget rejection. This budget protects work, not billable usage.
	key.requests.tokens--

	project := l.projects[projectID]
	if project == nil {
		project = &projectState{
			entries: fullBucket(l.config.EntriesPerMinute, now),
			bytes:   fullBucket(l.config.BytesPerMinute, now),
		}
		l.projects[projectID] = project
	}
	refill(&project.entries, l.config.EntriesPerMinute, now)
	refill(&project.bytes, l.config.BytesPerMinute, now)
	project.lastSeen = now
	if project.inflight >= l.config.MaxInflight {
		l.mu.Unlock()
		return nil, l.rejected(ReasonProjectInflight, time.Second)
	}
	entryCost := float64(entries)
	if project.entries.tokens < entryCost {
		retry := retryAfter(project.entries.tokens, entryCost, l.config.EntriesPerMinute)
		l.mu.Unlock()
		return nil, l.rejected(ReasonProjectEntries, retry)
	}
	byteCost := float64(payloadBytes)
	if project.bytes.tokens < byteCost {
		retry := retryAfter(project.bytes.tokens, byteCost, l.config.BytesPerMinute)
		l.mu.Unlock()
		return nil, l.rejected(ReasonProjectBytes, retry)
	}
	project.entries.tokens -= entryCost
	project.bytes.tokens -= byteCost
	project.inflight++
	l.mu.Unlock()

	if l.observer != nil {
		l.observer.ObserveAdmission("accepted", "none")
		l.observer.AddAdmissionInflight(1)
	}
	return &reservation{
		limiter: l, projectID: projectID,
		entryCost: entryCost, byteCost: byteCost,
	}, nil
}

func (l *Limiter) rejected(reason Reason, retry time.Duration) error {
	if l.observer != nil {
		l.observer.ObserveAdmission("rejected", string(reason))
	}
	return &LimitError{Reason: reason, RetryAfter: retry}
}

func (l *Limiter) finish(projectID domain.ProjectID, entryCost, byteCost float64, refund bool) {
	now := l.now().UTC()
	l.mu.Lock()
	project := l.projects[projectID]
	released := false
	if project != nil && project.inflight > 0 {
		refill(&project.entries, l.config.EntriesPerMinute, now)
		refill(&project.bytes, l.config.BytesPerMinute, now)
		if refund {
			project.entries.tokens = math.Min(float64(l.config.EntriesPerMinute), project.entries.tokens+entryCost)
			project.bytes.tokens = math.Min(float64(l.config.BytesPerMinute), project.bytes.tokens+byteCost)
		}
		project.inflight--
		project.lastSeen = now
		released = true
	}
	l.mu.Unlock()
	if released && l.observer != nil {
		l.observer.AddAdmissionInflight(-1)
	}
}

func (l *Limiter) cleanupLocked(now time.Time) {
	if !l.lastCleanup.IsZero() && now.Sub(l.lastCleanup) < time.Minute {
		return
	}
	cutoff := now.Add(-l.config.StateTTL)
	for id, state := range l.keys {
		if state.lastSeen.Before(cutoff) {
			delete(l.keys, id)
		}
	}
	for id, state := range l.projects {
		if state.inflight == 0 && state.lastSeen.Before(cutoff) {
			delete(l.projects, id)
		}
	}
	l.lastCleanup = now
}

func fullBucket(capacity int64, now time.Time) tokenBucket {
	return tokenBucket{tokens: float64(capacity), last: now}
}

func refill(bucket *tokenBucket, capacity int64, now time.Time) {
	if !now.After(bucket.last) {
		return
	}
	perSecond := float64(capacity) / 60
	bucket.tokens = math.Min(float64(capacity), bucket.tokens+now.Sub(bucket.last).Seconds()*perSecond)
	bucket.last = now
}

func retryAfter(available, required float64, capacityPerMinute int64) time.Duration {
	missing := required - available
	if missing <= 0 {
		return 0
	}
	seconds := missing / (float64(capacityPerMinute) / 60)
	return time.Duration(math.Ceil(seconds*1000)) * time.Millisecond
}

type reservation struct {
	once      sync.Once
	limiter   *Limiter
	projectID domain.ProjectID
	entryCost float64
	byteCost  float64
}

func (r *reservation) Commit() { r.finish(false) }

func (r *reservation) Release() {
	r.finish(true)
}

func (r *reservation) finish(refund bool) {
	if r == nil || r.limiter == nil {
		return
	}
	r.once.Do(func() { r.limiter.finish(r.projectID, r.entryCost, r.byteCost, refund) })
}
