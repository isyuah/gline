package bootstrap

import (
	"context"
	"errors"
	"testing"

	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/server/query"
)

func TestProjectLimiterIsolatesCapacityAndCleansCanceledWaiters(t *testing.T) {
	limiter := newProjectLimiter(1)
	projectA := domain.ProjectID("11111111-1111-4111-8111-111111111111")
	projectB := domain.ProjectID("22222222-2222-4222-8222-222222222222")

	releaseA, err := limiter.Acquire(context.Background(), projectA)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := limiter.Acquire(canceled, projectA); !errors.Is(err, context.Canceled) {
		t.Fatalf("second project A acquire error=%v", err)
	}
	if _, err := limiter.Acquire(context.Background(), projectA); !errors.Is(err, query.ErrCapacityLimited) {
		t.Fatalf("full project A acquire error=%v, want capacity error", err)
	}
	if _, err := limiter.Acquire(context.Background(), projectA); !errors.Is(err, query.ErrCapacityLimited) {
		t.Fatalf("full project A acquire error=%v, want capacity error", err)
	}

	releaseB, err := limiter.Acquire(context.Background(), projectB)
	if err != nil {
		t.Fatalf("project B should have independent capacity: %v", err)
	}
	releaseB()
	releaseA()
	releaseA()

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(limiter.projects) != 0 {
		t.Fatalf("limiter retained idle project semaphores: %+v", limiter.projects)
	}
}
