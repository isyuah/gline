package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type RetentionRepository struct{ q dbtx }

func (r *RetentionRepository) ListEnabled(ctx context.Context, limit int) ([]domain.RetentionPolicy, error) {
	if limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("%w: retention list limit", domain.ErrInvalid)
	}
	rows, err := r.q.QueryContext(ctx, `
SELECT project_id, max_age_seconds, max_bytes, enabled, updated_at
FROM retention_policies WHERE enabled = true
ORDER BY updated_at, project_id LIMIT $1`, limit)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()
	policies := make([]domain.RetentionPolicy, 0)
	for rows.Next() {
		policy, err := scanRetentionPolicy(rows)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, classifyError(rows.Err())
}

func (r *RetentionRepository) UpsertPolicy(ctx context.Context, policy domain.RetentionPolicy) (domain.RetentionPolicy, error) {
	seconds := int64(policy.MaxAge / time.Second)
	if !policy.ProjectID.Valid() || seconds <= 0 || policy.MaxAge != time.Duration(seconds)*time.Second ||
		(policy.MaxBytes != nil && *policy.MaxBytes <= 0) {
		return domain.RetentionPolicy{}, fmt.Errorf("%w: retention policy", domain.ErrInvalid)
	}
	return scanRetentionPolicy(r.q.QueryRowContext(ctx, `
INSERT INTO retention_policies (project_id, max_age_seconds, max_bytes, enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id) DO UPDATE
SET max_age_seconds = EXCLUDED.max_age_seconds,
    max_bytes = EXCLUDED.max_bytes,
    enabled = EXCLUDED.enabled,
    updated_at = now()
RETURNING project_id, max_age_seconds, max_bytes, enabled, updated_at`,
		policy.ProjectID, seconds, policy.MaxBytes, policy.Enabled))
}

func (r *RetentionRepository) GetPolicy(ctx context.Context, projectID domain.ProjectID) (domain.RetentionPolicy, error) {
	return scanRetentionPolicy(r.q.QueryRowContext(ctx, `
SELECT project_id, max_age_seconds, max_bytes, enabled, updated_at
FROM retention_policies WHERE project_id = $1`, projectID))
}

// DeleteEntriesBefore deletes one bounded Project-scoped batch. Callers own the
// job loop and its time budget.
func (r *RetentionRepository) DeleteEntriesBefore(ctx context.Context, projectID domain.ProjectID, before time.Time, limit int) (int64, error) {
	if !projectID.Valid() || before.IsZero() || limit <= 0 || limit > 10_000 {
		return 0, fmt.Errorf("%w: retention delete", domain.ErrInvalid)
	}
	result, err := r.q.ExecContext(ctx, `
WITH doomed AS (
    SELECT id FROM log_entries
    WHERE project_id = $1 AND ingested_at < $2
    ORDER BY ingested_at, id
    LIMIT $3
)
DELETE FROM log_entries e USING doomed d
WHERE e.id = d.id`, projectID, before, limit)
	if err != nil {
		return 0, classifyError(err)
	}
	return result.RowsAffected()
}

// DeleteOldestIfOverBytes removes at most limit oldest entries when the
// Project's stored row size exceeds maxBytes. Repeated bounded calls converge
// without holding one large delete transaction.
func (r *RetentionRepository) DeleteOldestIfOverBytes(ctx context.Context, projectID domain.ProjectID, maxBytes int64, limit int) (int64, error) {
	if !projectID.Valid() || maxBytes <= 0 || limit <= 0 || limit > 10_000 {
		return 0, fmt.Errorf("%w: retention byte limit", domain.ErrInvalid)
	}
	result, err := r.q.ExecContext(ctx, `
WITH project_size AS (
    SELECT COALESCE(sum(pg_column_size(e)), 0)::bigint AS bytes
    FROM log_entries e WHERE e.project_id = $1
), doomed AS (
    SELECT e.id FROM log_entries e CROSS JOIN project_size s
    WHERE e.project_id = $1 AND s.bytes > $2
    ORDER BY e.ingested_at, e.id
    LIMIT $3
)
DELETE FROM log_entries e USING doomed d WHERE e.id = d.id`, projectID, maxBytes, limit)
	if err != nil {
		return 0, classifyError(err)
	}
	return result.RowsAffected()
}

func scanRetentionPolicy(row interface{ Scan(...any) error }) (domain.RetentionPolicy, error) {
	var policy domain.RetentionPolicy
	var seconds int64
	var maxBytes sql.NullInt64
	if err := row.Scan(&policy.ProjectID, &seconds, &maxBytes, &policy.Enabled, &policy.UpdatedAt); err != nil {
		return domain.RetentionPolicy{}, classifyError(err)
	}
	if !policy.ProjectID.Valid() || seconds <= 0 {
		return domain.RetentionPolicy{}, ErrCorruptRow
	}
	policy.MaxAge = time.Duration(seconds) * time.Second
	if maxBytes.Valid {
		policy.MaxBytes = &maxBytes.Int64
	}
	return policy, nil
}
