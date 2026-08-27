package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type agentRepo struct{ called bool }

func (r *agentRepo) MarkStaleBefore(context.Context, time.Time, int) (int64, error) {
	r.called = true
	return 1, nil
}

type retentionRepo struct {
	ageCalls  int
	byteCalls int
}

func (r *retentionRepo) ListEnabled(context.Context, int) ([]domain.RetentionPolicy, error) {
	maxBytes := int64(1024)
	return []domain.RetentionPolicy{{ProjectID: "00000000-0000-4000-8000-000000000001", MaxAge: time.Hour, MaxBytes: &maxBytes, Enabled: true}}, nil
}
func (r *retentionRepo) DeleteEntriesBefore(context.Context, domain.ProjectID, time.Time, int) (int64, error) {
	r.ageCalls++
	if r.ageCalls == 1 {
		return 10, nil
	}
	return 0, nil
}
func (r *retentionRepo) DeleteOldestIfOverBytes(context.Context, domain.ProjectID, int64, int) (int64, error) {
	r.byteCalls++
	return 0, nil
}

type quarantineRepo struct{ called bool }

func (r *quarantineRepo) RequeueExpired(context.Context, time.Time, int) (int64, error) {
	r.called = true
	return 1, nil
}

func TestRunOnceCoordinatesBoundedJobs(t *testing.T) {
	agents := &agentRepo{}
	retention := &retentionRepo{}
	quarantine := &quarantineRepo{}
	cfg := DefaultConfig()
	cfg.BatchSize = 10
	worker, err := New(agents, retention, quarantine, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) }
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !agents.called || !quarantine.called || retention.ageCalls != 2 || retention.byteCalls != 1 {
		t.Fatalf("jobs = agents:%v quarantine:%v age:%d bytes:%d", agents.called, quarantine.called, retention.ageCalls, retention.byteCalls)
	}
}
