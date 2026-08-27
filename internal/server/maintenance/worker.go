package maintenance

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type AgentRepository interface {
	MarkStaleBefore(context.Context, time.Time, int) (int64, error)
}

type RetentionRepository interface {
	ListEnabled(context.Context, int) ([]domain.RetentionPolicy, error)
	DeleteEntriesBefore(context.Context, domain.ProjectID, time.Time, int) (int64, error)
	DeleteOldestIfOverBytes(context.Context, domain.ProjectID, int64, int) (int64, error)
}

type QuarantineRepository interface {
	RequeueExpired(context.Context, time.Time, int) (int64, error)
}

type Config struct {
	Interval             time.Duration
	AgentStaleAfter      time.Duration
	QuarantineLease      time.Duration
	BatchSize            int
	MaxBatchesPerProject int
	MaxProjectsPerCycle  int
}

func DefaultConfig() Config {
	return Config{
		Interval: time.Minute, AgentStaleAfter: 2 * time.Minute,
		QuarantineLease: 5 * time.Minute, BatchSize: 1000,
		MaxBatchesPerProject: 10, MaxProjectsPerCycle: 500,
	}
}

type Worker struct {
	agents     AgentRepository
	retention  RetentionRepository
	quarantine QuarantineRepository
	config     Config
	logger     *slog.Logger
	now        func() time.Time
	observer   Observer
}

type Observer interface {
	ObserveBackgroundJob(job, result string, processed int64, duration time.Duration)
}

type Option func(*Worker)

func WithObserver(observer Observer) Option {
	return func(worker *Worker) { worker.observer = observer }
}

func New(agents AgentRepository, retention RetentionRepository, quarantine QuarantineRepository, cfg Config, logger *slog.Logger, options ...Option) (*Worker, error) {
	if agents == nil || retention == nil || quarantine == nil {
		return nil, fmt.Errorf("maintenance: repositories are required")
	}
	if cfg.Interval <= 0 || cfg.AgentStaleAfter <= cfg.Interval || cfg.QuarantineLease <= 0 ||
		cfg.BatchSize <= 0 || cfg.BatchSize > 10_000 || cfg.MaxBatchesPerProject <= 0 || cfg.MaxProjectsPerCycle <= 0 {
		return nil, fmt.Errorf("maintenance: invalid configuration")
	}
	if logger == nil {
		logger = slog.Default()
	}
	worker := &Worker{agents: agents, retention: retention, quarantine: quarantine, config: cfg, logger: logger, now: time.Now}
	for _, option := range options {
		if option != nil {
			option(worker)
		}
	}
	return worker, nil
}

func (w *Worker) Run(ctx context.Context) error {
	if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
		w.logger.Error("maintenance cycle failed", "error", err)
	}
	ticker := time.NewTicker(w.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
				w.logger.Error("maintenance cycle failed", "error", err)
			}
		}
	}
}

func (w *Worker) RunOnce(ctx context.Context) error {
	now := w.now().UTC()
	if _, err := w.observeCount("agent_state", func() (int64, error) {
		return w.agents.MarkStaleBefore(ctx, now.Add(-w.config.AgentStaleAfter), w.config.BatchSize)
	}); err != nil {
		return fmt.Errorf("mark stale agents: %w", err)
	}
	if _, err := w.observeCount("quarantine_lease", func() (int64, error) {
		return w.quarantine.RequeueExpired(ctx, now.Add(-w.config.QuarantineLease), w.config.BatchSize)
	}); err != nil {
		return fmt.Errorf("recover quarantine leases: %w", err)
	}
	started := time.Now()
	policies, err := w.retention.ListEnabled(ctx, w.config.MaxProjectsPerCycle)
	w.observe("retention_scan", int64(len(policies)), time.Since(started), err)
	if err != nil {
		return fmt.Errorf("list retention policies: %w", err)
	}
	for _, policy := range policies {
		for attempt := 0; attempt < w.config.MaxBatchesPerProject; attempt++ {
			deleted, err := w.observeCount("retention_age", func() (int64, error) {
				return w.retention.DeleteEntriesBefore(ctx, policy.ProjectID, now.Add(-policy.MaxAge), w.config.BatchSize)
			})
			if err != nil {
				return fmt.Errorf("apply age retention for project %s: %w", policy.ProjectID, err)
			}
			if deleted < int64(w.config.BatchSize) {
				break
			}
		}
		if policy.MaxBytes == nil {
			continue
		}
		for attempt := 0; attempt < w.config.MaxBatchesPerProject; attempt++ {
			deleted, err := w.observeCount("retention_bytes", func() (int64, error) {
				return w.retention.DeleteOldestIfOverBytes(ctx, policy.ProjectID, *policy.MaxBytes, w.config.BatchSize)
			})
			if err != nil {
				return fmt.Errorf("apply byte retention for project %s: %w", policy.ProjectID, err)
			}
			if deleted == 0 {
				break
			}
		}
	}
	return nil
}

func (w *Worker) observeCount(job string, operation func() (int64, error)) (int64, error) {
	started := time.Now()
	processed, err := operation()
	w.observe(job, processed, time.Since(started), err)
	return processed, err
}

func (w *Worker) observe(job string, processed int64, duration time.Duration, err error) {
	if w.observer == nil {
		return
	}
	result := "success"
	if err != nil {
		result = "error"
		processed = 0
	}
	w.observer.ObserveBackgroundJob(job, result, processed, duration)
}
