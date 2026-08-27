package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type QuarantineRepository struct{ q dbtx }

func (r *QuarantineRepository) FindProject(ctx context.Context, id domain.QuarantineID) (domain.ProjectID, error) {
	if !id.Valid() {
		return "", fmt.Errorf("%w: quarantine identity", domain.ErrInvalid)
	}
	var projectID domain.ProjectID
	if err := r.q.QueryRowContext(ctx, `
SELECT project_id FROM quarantine_batches WHERE id = $1`, id).Scan(&projectID); err != nil {
		return "", classifyError(err)
	}
	if !projectID.Valid() {
		return "", ErrCorruptRow
	}
	return projectID, nil
}

func (r *QuarantineRepository) Get(ctx context.Context, projectID domain.ProjectID, id domain.QuarantineID) (domain.QuarantineBatch, error) {
	if !projectID.Valid() || !id.Valid() {
		return domain.QuarantineBatch{}, fmt.Errorf("%w: quarantine identity", domain.ErrInvalid)
	}
	return scanQuarantine(r.q.QueryRowContext(ctx, `
SELECT id, project_id, batch_id, payload, payload_hash, error_code,
       error_detail, status, attempts, created_at, claimed_at, resolved_at
FROM quarantine_batches WHERE project_id = $1 AND id = $2`, projectID, id))
}

func (r *QuarantineRepository) List(ctx context.Context, projectID domain.ProjectID, limit int) ([]domain.QuarantineBatch, error) {
	if !projectID.Valid() || limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("%w: quarantine list", domain.ErrInvalid)
	}
	rows, err := r.q.QueryContext(ctx, `
SELECT id, project_id, batch_id, payload, payload_hash, error_code,
       error_detail, status, attempts, created_at, claimed_at, resolved_at
FROM quarantine_batches WHERE project_id = $1
ORDER BY created_at DESC, id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()
	batches := make([]domain.QuarantineBatch, 0)
	for rows.Next() {
		batch, err := scanQuarantine(rows)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return batches, classifyError(rows.Err())
}

func (r *QuarantineRepository) Claim(ctx context.Context, projectID domain.ProjectID, id domain.QuarantineID) (domain.QuarantineBatch, error) {
	if !projectID.Valid() || !id.Valid() {
		return domain.QuarantineBatch{}, fmt.Errorf("%w: quarantine identity", domain.ErrInvalid)
	}
	return scanQuarantine(r.q.QueryRowContext(ctx, `
UPDATE quarantine_batches
SET status = 'replaying', attempts = attempts + 1, claimed_at = now(), resolved_at = NULL
WHERE project_id = $1 AND id = $2 AND status = 'pending'
RETURNING id, project_id, batch_id, payload, payload_hash, error_code,
          error_detail, status, attempts, created_at, claimed_at, resolved_at`, projectID, id))
}

func (r *QuarantineRepository) Create(ctx context.Context, batch domain.QuarantineBatch, maxPayloadBytes int) (domain.QuarantineBatch, error) {
	if !batch.ID.Valid() || !batch.ProjectID.Valid() || !batch.BatchID.Valid() || len(batch.Payload) == 0 ||
		maxPayloadBytes <= 0 || len(batch.Payload) > maxPayloadBytes || batch.ErrorCode == "" {
		return domain.QuarantineBatch{}, fmt.Errorf("%w: quarantine batch", domain.ErrInvalid)
	}
	return scanQuarantine(r.q.QueryRowContext(ctx, `
INSERT INTO quarantine_batches (
    id, project_id, batch_id, payload, payload_hash, error_code, error_detail, status
) VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending')
RETURNING id, project_id, batch_id, payload, payload_hash, error_code,
          error_detail, status, attempts, created_at, claimed_at, resolved_at`,
		batch.ID, batch.ProjectID, batch.BatchID, batch.Payload, batch.PayloadHash[:],
		batch.ErrorCode, batch.ErrorDetail))
}

// ClaimPending is one atomic statement. SKIP LOCKED lets multiple workers claim
// distinct rows without holding locks while replaying the payload.
func (r *QuarantineRepository) ClaimPending(ctx context.Context, limit int) ([]domain.QuarantineBatch, error) {
	if limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("%w: quarantine claim limit", domain.ErrInvalid)
	}
	rows, err := r.q.QueryContext(ctx, `
WITH picked AS (
    SELECT id
    FROM quarantine_batches
    WHERE status = 'pending'
    ORDER BY created_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT $1
)
UPDATE quarantine_batches q
SET status = 'replaying', attempts = attempts + 1,
    claimed_at = now(), resolved_at = NULL
FROM picked
WHERE q.id = picked.id
RETURNING q.id, q.project_id, q.batch_id, q.payload, q.payload_hash,
          q.error_code, q.error_detail, q.status, q.attempts,
          q.created_at, q.claimed_at, q.resolved_at`, limit)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()

	var result []domain.QuarantineBatch
	for rows.Next() {
		batch, err := scanQuarantine(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, batch)
	}
	return result, classifyError(rows.Err())
}

func (r *QuarantineRepository) MarkTerminal(ctx context.Context, projectID domain.ProjectID, id domain.QuarantineID, status domain.QuarantineStatus, detail string, resolvedAt time.Time) error {
	if status != domain.QuarantineResolved && status != domain.QuarantineDiscarded {
		return fmt.Errorf("%w: quarantine terminal status", domain.ErrInvalid)
	}
	result, err := r.q.ExecContext(ctx, `
UPDATE quarantine_batches
	SET status = $3, error_detail = $4, resolved_at = $5
WHERE project_id = $1 AND id = $2 AND status = 'replaying'`,
		projectID, id, status, detail, resolvedAt)
	if err != nil {
		return classifyError(err)
	}
	return requireAffected(result)
}

func (r *QuarantineRepository) ReleaseForRetry(ctx context.Context, projectID domain.ProjectID, id domain.QuarantineID, detail string) error {
	result, err := r.q.ExecContext(ctx, `
UPDATE quarantine_batches
SET status = 'pending', claimed_at = NULL, resolved_at = NULL, error_detail = $3
WHERE project_id = $1 AND id = $2 AND status = 'replaying'`, projectID, id, detail)
	if err != nil {
		return classifyError(err)
	}
	return requireAffected(result)
}

func (r *QuarantineRepository) RequeueExpired(ctx context.Context, claimedBefore time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("%w: quarantine recovery limit", domain.ErrInvalid)
	}
	result, err := r.q.ExecContext(ctx, `
WITH expired AS (
    SELECT id FROM quarantine_batches
    WHERE status = 'replaying' AND claimed_at < $1
    ORDER BY claimed_at, id
    LIMIT $2
)
UPDATE quarantine_batches q
SET status = 'pending', claimed_at = NULL, resolved_at = NULL,
    error_detail = 'replay lease expired'
FROM expired e WHERE q.id = e.id`, claimedBefore, limit)
	if err != nil {
		return 0, classifyError(err)
	}
	return result.RowsAffected()
}

func scanQuarantine(row interface{ Scan(...any) error }) (domain.QuarantineBatch, error) {
	var batch domain.QuarantineBatch
	var hash []byte
	var claimedAt, resolvedAt sql.NullTime
	if err := row.Scan(
		&batch.ID, &batch.ProjectID, &batch.BatchID, &batch.Payload, &hash,
		&batch.ErrorCode, &batch.ErrorDetail, &batch.Status, &batch.Attempts,
		&batch.CreatedAt, &claimedAt, &resolvedAt,
	); err != nil {
		return domain.QuarantineBatch{}, classifyError(err)
	}
	if len(hash) != len(batch.PayloadHash) || !batch.Status.Valid() {
		return domain.QuarantineBatch{}, ErrCorruptRow
	}
	copy(batch.PayloadHash[:], hash)
	batch.ClaimedAt = nullableTime(claimedAt)
	batch.ResolvedAt = nullableTime(resolvedAt)
	return batch, nil
}
