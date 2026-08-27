package admission

import (
	"errors"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

const (
	keyOne     domain.APIKeyID  = "11111111-1111-4111-8111-111111111111"
	keyTwo     domain.APIKeyID  = "22222222-2222-4222-8222-222222222222"
	keyThree   domain.APIKeyID  = "55555555-5555-4555-8555-555555555555"
	projectOne domain.ProjectID = "33333333-3333-4333-8333-333333333333"
	projectTwo domain.ProjectID = "44444444-4444-4444-8444-444444444444"
)

func TestLimiterEnforcesInflightAndKeepsProjectsIndependent(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	observer := &recordingObserver{}
	limiter := newTestLimiter(t, &now, observer)

	first, err := limiter.AllowIngest(t.Context(), keyOne, projectOne, 2, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.AllowIngest(t.Context(), keyTwo, projectOne, 1, 10); limitReason(err) != ReasonProjectInflight {
		t.Fatalf("same-project concurrent admission error = %v", err)
	}
	other, err := limiter.AllowIngest(t.Context(), keyTwo, projectTwo, 1, 10)
	if err != nil {
		t.Fatalf("another project should have independent capacity: %v", err)
	}
	other.Release()
	first.Release()
	first.Release()

	if observer.inflight != 0 || observer.accepted != 2 || observer.rejected[ReasonProjectInflight] != 1 {
		t.Fatalf("observer = %+v", observer)
	}
}

func TestLimiterCommitsUsageRefillsAndReleaseRefunds(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(t, &now, nil)
	reservation, err := limiter.AllowIngest(t.Context(), keyOne, projectOne, 2, 20)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Commit()

	if _, err := limiter.AllowIngest(t.Context(), keyTwo, projectOne, 2, 10); limitReason(err) != ReasonProjectEntries {
		t.Fatalf("entry budget error = %v", err)
	}
	now = now.Add(20 * time.Second)
	refilled, err := limiter.AllowIngest(t.Context(), keyTwo, projectOne, 2, 10)
	if err != nil {
		t.Fatalf("refilled budget rejected: %v", err)
	}
	refilled.Release()

	// Release refunds project cost, so the same immutable batch can retry after
	// a downstream failure without slowly exhausting the minute budget.
	retry, err := limiter.AllowIngest(t.Context(), keyThree, projectOne, 2, 10)
	if err != nil {
		t.Fatalf("refunded budget rejected: %v", err)
	}
	retry.Release()
}

func TestLimiterCountsAuthenticatedAttemptsPerKey(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(t, &now, nil)
	for range 2 {
		reservation, err := limiter.AllowIngest(t.Context(), keyOne, projectOne, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		reservation.Release()
	}
	_, err := limiter.AllowIngest(t.Context(), keyOne, projectOne, 1, 1)
	var limited *LimitError
	if !errors.As(err, &limited) || limited.Reason != ReasonKeyRate || limited.RetryAfter != 30*time.Second {
		t.Fatalf("third request error = %#v", err)
	}
	now = now.Add(30 * time.Second)
	reservation, err := limiter.AllowIngest(t.Context(), keyOne, projectOne, 1, 1)
	if err != nil {
		t.Fatalf("refilled key budget rejected: %v", err)
	}
	reservation.Release()
}

func TestLimiterRejectsBatchThatCanNeverFit(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(t, &now, nil)
	if _, err := limiter.AllowIngest(t.Context(), keyOne, projectOne, 4, 1); !errors.Is(err, ErrBatchExceedsCapacity) {
		t.Fatalf("oversized batch error = %v", err)
	}
}

func TestLimiterReportsByteBudgetSeparately(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	limiter, err := New(Config{
		RequestsPerMinute: 100, EntriesPerMinute: 100, BytesPerMinute: 10,
		MaxInflight: 2, StateTTL: 2 * time.Minute,
	}, nil, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	first, err := limiter.AllowIngest(t.Context(), keyOne, projectOne, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	first.Commit()
	if _, err := limiter.AllowIngest(t.Context(), keyTwo, projectOne, 1, 3); limitReason(err) != ReasonProjectBytes {
		t.Fatalf("byte budget error = %v", err)
	}
}

func TestLimiterEvictsIdleIdentityState(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(t, &now, nil)
	reservation, err := limiter.AllowIngest(t.Context(), keyOne, projectOne, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Release()
	now = now.Add(3 * time.Minute)
	reservation, err = limiter.AllowIngest(t.Context(), keyTwo, projectTwo, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Release()
	if _, exists := limiter.keys[keyOne]; exists {
		t.Fatal("idle API key state was not evicted")
	}
	if _, exists := limiter.projects[projectOne]; exists {
		t.Fatal("idle Project state was not evicted")
	}
}

func newTestLimiter(t *testing.T, now *time.Time, observer Observer) *Limiter {
	t.Helper()
	limiter, err := New(Config{
		RequestsPerMinute: 2, EntriesPerMinute: 3, BytesPerMinute: 30,
		MaxInflight: 1, StateTTL: 2 * time.Minute,
	}, observer, WithClock(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

func limitReason(err error) Reason {
	var limited *LimitError
	if errors.As(err, &limited) {
		return limited.Reason
	}
	return ""
}

type recordingObserver struct {
	accepted int
	rejected map[Reason]int
	inflight float64
}

func (o *recordingObserver) ObserveAdmission(result, reason string) {
	if result == "accepted" {
		o.accepted++
		return
	}
	if o.rejected == nil {
		o.rejected = make(map[Reason]int)
	}
	o.rejected[Reason(reason)]++
}

func (o *recordingObserver) AddAdmissionInflight(delta float64) { o.inflight += delta }
