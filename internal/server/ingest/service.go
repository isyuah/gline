package ingest

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/isyuah/gline/internal/domain"
	serverauth "github.com/isyuah/gline/internal/server/auth"
)

var (
	ErrIdempotencyConflict = domain.ErrIdempotencyConflict
	ErrAgentDisabled       = errors.New("ingest agent is disabled")
	ErrPipelineUnavailable = errors.New("ingest pipeline is not enabled")
)

type ProjectRepository interface {
	Get(context.Context, domain.ProjectID) (domain.Project, error)
}

type AgentRepository interface {
	Get(context.Context, domain.ProjectID, domain.AgentID) (domain.Agent, error)
}

type PipelineRepository interface {
	Get(context.Context, domain.ProjectID, domain.PipelineID) (domain.Pipeline, error)
}

type BatchRepository interface {
	InsertBatch(context.Context, domain.Batch, time.Time) (bool, error)
	FindBatch(context.Context, domain.ProjectID, domain.BatchID) (domain.StoredBatch, error)
	InsertEntries(context.Context, domain.Batch) error
}

type UsageRepository interface {
	Add(context.Context, domain.ProjectID, time.Time, int64, int64, int64) (domain.UsageBucket, error)
}

type Repositories struct {
	Projects  ProjectRepository
	Agents    AgentRepository
	Pipelines PipelineRepository
	Batches   BatchRepository
	Usage     UsageRepository
}

// WithinTx must commit only after fn returns nil and return a commit error to
// the caller. This callback is the service's ACK boundary.
type WithinTx func(context.Context, func(Repositories) error) error
type Clock func() time.Time

type Service struct {
	withinTx WithinTx
	now      Clock
}

func NewService(withinTx WithinTx, now Clock) (*Service, error) {
	if withinTx == nil {
		return nil, errors.New("ingest service requires a transaction runner")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{withinTx: withinTx, now: now}, nil
}

type Status string

const (
	StatusAccepted  Status = "accepted"
	StatusDuplicate Status = "duplicate"
)

type Result struct {
	BatchID         domain.BatchID
	Status          Status
	AcceptedEntries int
}

func (s *Service) Accept(ctx context.Context, principal serverauth.Principal, candidate domain.Batch) (Result, error) {
	if err := principal.Require(domain.ScopeIngest); err != nil {
		return Result{}, err
	}
	if candidate.ProjectID != "" && candidate.ProjectID != principal.ProjectID {
		return Result{}, serverauth.ErrProjectMismatch
	}
	if err := principal.RequireAgent(candidate.AgentID); err != nil {
		return Result{}, err
	}
	batch := cloneBatch(candidate)
	batch.ProjectID = principal.ProjectID
	for index := range batch.Entries {
		entry := &batch.Entries[index]
		if entry.ProjectID != "" && entry.ProjectID != principal.ProjectID {
			return Result{}, serverauth.ErrProjectMismatch
		}
		entry.ProjectID = principal.ProjectID
	}
	if subtle.ConstantTimeCompare(batch.PayloadHash[:], make([]byte, len(batch.PayloadHash))) == 1 {
		return Result{}, fmt.Errorf("%w: empty payload hash", domain.ErrInvalid)
	}
	if err := batch.Validate(); err != nil {
		return Result{}, err
	}

	result := Result{BatchID: batch.ID, AcceptedEntries: len(batch.Entries)}
	committedAt := s.now().UTC()
	err := s.withinTx(ctx, func(repos Repositories) error {
		if err := requireRepositories(repos); err != nil {
			return err
		}
		project, err := repos.Projects.Get(ctx, principal.ProjectID)
		if err != nil {
			return err
		}
		if err := project.CanIngest(); err != nil {
			return err
		}
		agent, err := repos.Agents.Get(ctx, principal.ProjectID, batch.AgentID)
		if err != nil {
			return err
		}
		if agent.Status == domain.AgentDisabled {
			return ErrAgentDisabled
		}
		pipeline, err := repos.Pipelines.Get(ctx, principal.ProjectID, batch.PipelineID)
		if err != nil {
			return err
		}
		if pipeline.AgentID != batch.AgentID {
			return fmt.Errorf("%w: pipeline agent", domain.ErrInvalid)
		}
		if pipeline.Status != domain.PipelineEnabled {
			return ErrPipelineUnavailable
		}

		inserted, err := repos.Batches.InsertBatch(ctx, batch, committedAt)
		if err != nil {
			return err
		}
		if !inserted {
			stored, err := repos.Batches.FindBatch(ctx, principal.ProjectID, batch.ID)
			if err != nil {
				return err
			}
			if err := stored.VerifyRetry(batch); err != nil {
				return err
			}
			result.Status = StatusDuplicate
			result.AcceptedEntries = stored.EntryCount
			return nil
		}
		if err := repos.Batches.InsertEntries(ctx, batch); err != nil {
			return err
		}
		if _, err := repos.Usage.Add(ctx, principal.ProjectID, committedAt, int64(len(batch.Entries)), int64(batch.PayloadBytes), 0); err != nil {
			return err
		}
		result.Status = StatusAccepted
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if result.Status != StatusAccepted && result.Status != StatusDuplicate {
		return Result{}, errors.New("ingest transaction completed without a terminal result")
	}
	return result, nil
}

func requireRepositories(repos Repositories) error {
	if repos.Projects == nil || repos.Agents == nil || repos.Pipelines == nil || repos.Batches == nil || repos.Usage == nil {
		return errors.New("ingest transaction is missing a repository")
	}
	return nil
}

func cloneBatch(source domain.Batch) domain.Batch {
	result := source
	result.Entries = make([]domain.Entry, len(source.Entries))
	for index, entry := range source.Entries {
		result.Entries[index] = entry
		if entry.Attributes != nil {
			result.Entries[index].Attributes = make(map[string]any, len(entry.Attributes))
			for key, value := range entry.Attributes {
				result.Entries[index].Attributes[key] = value
			}
		}
	}
	return result
}
