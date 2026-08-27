package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/protocol/ingestv1"
	serverauth "github.com/isyuah/gline/internal/server/auth"
	"github.com/isyuah/gline/internal/server/ingest"
)

type RetentionRepository interface {
	UpsertPolicy(context.Context, domain.RetentionPolicy) (domain.RetentionPolicy, error)
}

type QuarantineRepository interface {
	Claim(context.Context, domain.ProjectID, domain.QuarantineID) (domain.QuarantineBatch, error)
	MarkTerminal(context.Context, domain.ProjectID, domain.QuarantineID, domain.QuarantineStatus, string, time.Time) error
	ReleaseForRetry(context.Context, domain.ProjectID, domain.QuarantineID, string) error
}

type AuditRepository interface {
	Append(context.Context, domain.AuditEvent) (domain.AuditEvent, error)
}

type Repositories struct {
	Retention  RetentionRepository
	Quarantine QuarantineRepository
	Audit      AuditRepository
}

type WithinTx func(context.Context, func(Repositories) error) error

type IngestService interface {
	Accept(context.Context, serverauth.Principal, domain.Batch) (ingest.Result, error)
}

type Service struct {
	withinTx WithinTx
	ingest   IngestService
	limits   ingestv1.Limits
	now      func() time.Time
}

func New(withinTx WithinTx, ingestService IngestService, limits ingestv1.Limits) (*Service, error) {
	if withinTx == nil || ingestService == nil {
		return nil, errors.New("operations requires transaction runner and ingest service")
	}
	if limits.MaxEntries <= 0 {
		limits = ingestv1.DefaultLimits()
	}
	return &Service{withinTx: withinTx, ingest: ingestService, limits: limits, now: time.Now}, nil
}

func (s *Service) SetRetention(ctx context.Context, principal serverauth.Principal, policy domain.RetentionPolicy) (domain.RetentionPolicy, error) {
	if err := principal.Require(domain.ScopeRetentionManage); err != nil {
		return domain.RetentionPolicy{}, err
	}
	if err := principal.RequireProject(policy.ProjectID); err != nil {
		return domain.RetentionPolicy{}, err
	}
	if policy.MaxAge <= 0 || policy.MaxAge%time.Second != 0 || policy.MaxBytes != nil && *policy.MaxBytes <= 0 {
		return domain.RetentionPolicy{}, fmt.Errorf("%w: retention policy", domain.ErrInvalid)
	}
	var saved domain.RetentionPolicy
	err := s.withinTx(ctx, func(repos Repositories) error {
		if repos.Retention == nil || repos.Audit == nil {
			return errors.New("operations transaction is missing retention dependencies")
		}
		var err error
		saved, err = repos.Retention.UpsertPolicy(ctx, policy)
		if err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, policy.ProjectID, "retention.set", "retention_policy", string(policy.ProjectID), domain.AuditSuccess, map[string]any{
			"max_age_seconds": int64(policy.MaxAge / time.Second), "max_bytes": policy.MaxBytes, "enabled": policy.Enabled,
		})
	})
	return saved, err
}

func (s *Service) ReplayQuarantine(ctx context.Context, principal serverauth.Principal, projectID domain.ProjectID, id domain.QuarantineID) (domain.QuarantineBatch, error) {
	if err := requireReplay(principal, projectID, id); err != nil {
		return domain.QuarantineBatch{}, err
	}
	claimed, err := s.claim(ctx, principal, projectID, id)
	if err != nil {
		return domain.QuarantineBatch{}, err
	}
	request, payloadBytes, err := ingestv1.Decode(bytes.NewReader(claimed.Payload), s.limits.MaxBodyBytes)
	if err == nil {
		var batch domain.Batch
		batch, err = ingestv1.Normalize(request, projectID, payloadBytes, s.limits)
		if err == nil {
			replayPrincipal := principal
			replayPrincipal.Scopes = cloneScopes(principal.Scopes)
			replayPrincipal.Scopes[domain.ScopeIngest] = struct{}{}
			_, err = s.ingest.Accept(ctx, replayPrincipal, batch)
		}
	}
	if err != nil {
		releaseErr := s.release(ctx, principal, claimed, err)
		if releaseErr != nil {
			return domain.QuarantineBatch{}, errors.Join(err, releaseErr)
		}
		return domain.QuarantineBatch{}, err
	}

	resolvedAt := s.now().UTC()
	err = s.withinTx(ctx, func(repos Repositories) error {
		if repos.Quarantine == nil || repos.Audit == nil {
			return errors.New("operations transaction is missing quarantine dependencies")
		}
		if err := repos.Quarantine.MarkTerminal(ctx, projectID, id, domain.QuarantineResolved, "manual replay accepted", resolvedAt); err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, projectID, "quarantine.replay.resolve", "quarantine_batch", string(id), domain.AuditSuccess, nil)
	})
	if err != nil {
		return domain.QuarantineBatch{}, err
	}
	claimed.Status = domain.QuarantineResolved
	claimed.ErrorDetail = "manual replay accepted"
	claimed.ResolvedAt = &resolvedAt
	return claimed, nil
}

func (s *Service) DiscardQuarantine(ctx context.Context, principal serverauth.Principal, projectID domain.ProjectID, id domain.QuarantineID) (domain.QuarantineBatch, error) {
	if err := requireReplay(principal, projectID, id); err != nil {
		return domain.QuarantineBatch{}, err
	}
	var batch domain.QuarantineBatch
	resolvedAt := s.now().UTC()
	err := s.withinTx(ctx, func(repos Repositories) error {
		if repos.Quarantine == nil || repos.Audit == nil {
			return errors.New("operations transaction is missing quarantine dependencies")
		}
		var err error
		batch, err = repos.Quarantine.Claim(ctx, projectID, id)
		if err != nil {
			return err
		}
		if err := repos.Quarantine.MarkTerminal(ctx, projectID, id, domain.QuarantineDiscarded, "discarded by operator", resolvedAt); err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, projectID, "quarantine.discard", "quarantine_batch", string(id), domain.AuditSuccess, nil)
	})
	if err != nil {
		return domain.QuarantineBatch{}, err
	}
	batch.Status = domain.QuarantineDiscarded
	batch.ErrorDetail = "discarded by operator"
	batch.ResolvedAt = &resolvedAt
	return batch, nil
}

func (s *Service) claim(ctx context.Context, principal serverauth.Principal, projectID domain.ProjectID, id domain.QuarantineID) (domain.QuarantineBatch, error) {
	var batch domain.QuarantineBatch
	err := s.withinTx(ctx, func(repos Repositories) error {
		if repos.Quarantine == nil || repos.Audit == nil {
			return errors.New("operations transaction is missing quarantine dependencies")
		}
		var err error
		batch, err = repos.Quarantine.Claim(ctx, projectID, id)
		if err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, projectID, "quarantine.replay.claim", "quarantine_batch", string(id), domain.AuditSuccess, nil)
	})
	return batch, err
}

func (s *Service) release(ctx context.Context, principal serverauth.Principal, batch domain.QuarantineBatch, replayErr error) error {
	detail := replayErr.Error()
	if len(detail) > 2048 {
		detail = detail[:2048]
	}
	return s.withinTx(ctx, func(repos Repositories) error {
		if repos.Quarantine == nil || repos.Audit == nil {
			return errors.New("operations transaction is missing quarantine dependencies")
		}
		if err := repos.Quarantine.ReleaseForRetry(ctx, batch.ProjectID, batch.ID, detail); err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, batch.ProjectID, "quarantine.replay.release", "quarantine_batch", string(batch.ID), domain.AuditFailed, map[string]any{"error": detail})
	})
}

func requireReplay(principal serverauth.Principal, projectID domain.ProjectID, id domain.QuarantineID) error {
	if err := principal.Require(domain.ScopeQuarantineReplay); err != nil {
		return err
	}
	if err := principal.RequireProject(projectID); err != nil {
		return err
	}
	if !id.Valid() {
		return fmt.Errorf("%w: quarantine id", domain.ErrInvalid)
	}
	return nil
}

func cloneScopes(source map[domain.Scope]struct{}) map[domain.Scope]struct{} {
	result := make(map[domain.Scope]struct{}, len(source)+1)
	for scope := range source {
		result[scope] = struct{}{}
	}
	return result
}

func appendAudit(ctx context.Context, repo AuditRepository, principal serverauth.Principal, projectID domain.ProjectID, action, resource, resourceID string, outcome domain.AuditOutcome, metadata map[string]any) error {
	raw := json.RawMessage(`{}`)
	if metadata != nil {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		raw = encoded
	}
	_, err := repo.Append(ctx, domain.AuditEvent{
		ProjectID: &projectID, ActorType: "api_key", ActorID: string(principal.KeyID),
		Action: action, Resource: resource, ResourceID: resourceID, Outcome: outcome, Metadata: raw,
	})
	return err
}
