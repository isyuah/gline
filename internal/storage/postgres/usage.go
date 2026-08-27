package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type UsageRepository struct{ q dbtx }

func (r *UsageRepository) Add(ctx context.Context, projectID domain.ProjectID, at time.Time, entries, bytes, failedBatches int64) (domain.UsageBucket, error) {
	if !projectID.Valid() || at.IsZero() || entries < 0 || bytes < 0 || failedBatches < 0 ||
		(entries == 0 && bytes == 0 && failedBatches == 0) {
		return domain.UsageBucket{}, fmt.Errorf("%w: usage increment", domain.ErrInvalid)
	}
	return scanUsage(r.q.QueryRowContext(ctx, `
INSERT INTO usage_buckets (project_id, bucket_start, entries, bytes, failed_batches)
VALUES ($1, date_trunc('minute', $2::timestamptz), $3, $4, $5)
ON CONFLICT (project_id, bucket_start) DO UPDATE
SET entries = usage_buckets.entries + EXCLUDED.entries,
    bytes = usage_buckets.bytes + EXCLUDED.bytes,
    failed_batches = usage_buckets.failed_batches + EXCLUDED.failed_batches
RETURNING project_id, bucket_start, entries, bytes, failed_batches`,
		projectID, at, entries, bytes, failedBatches))
}

func (r *UsageRepository) List(ctx context.Context, projectID domain.ProjectID, from, to time.Time) ([]domain.UsageBucket, error) {
	if !projectID.Valid() || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, fmt.Errorf("%w: usage query", domain.ErrInvalid)
	}
	rows, err := r.q.QueryContext(ctx, `
SELECT project_id, bucket_start, entries, bytes, failed_batches
FROM usage_buckets
WHERE project_id = $1 AND bucket_start >= $2 AND bucket_start < $3
ORDER BY bucket_start`, projectID, from, to)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()
	var result []domain.UsageBucket
	for rows.Next() {
		bucket, err := scanUsage(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, bucket)
	}
	return result, classifyError(rows.Err())
}

func scanUsage(row interface{ Scan(...any) error }) (domain.UsageBucket, error) {
	var bucket domain.UsageBucket
	if err := row.Scan(&bucket.ProjectID, &bucket.BucketStart, &bucket.Entries, &bucket.Bytes, &bucket.FailedBatches); err != nil {
		return domain.UsageBucket{}, classifyError(err)
	}
	if !bucket.ProjectID.Valid() || bucket.Entries < 0 || bucket.Bytes < 0 || bucket.FailedBatches < 0 {
		return domain.UsageBucket{}, ErrCorruptRow
	}
	return bucket, nil
}
