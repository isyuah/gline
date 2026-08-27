package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type IngestRepository struct{ q dbtx }

// InsertBatch records the committed form inside the caller-owned transaction.
// The row is not visible, and must never be ACKed, until the caller commits.
func (r *IngestRepository) InsertBatch(ctx context.Context, batch domain.Batch, committedAt time.Time) (bool, error) {
	if err := batch.Validate(); err != nil {
		return false, err
	}
	if committedAt.IsZero() {
		return false, fmt.Errorf("%w: committed at", domain.ErrInvalid)
	}
	result, err := r.q.ExecContext(ctx, `
INSERT INTO ingest_batches (
    id, project_id, agent_id, pipeline_id, sequence_no, payload_hash,
    entry_count, payload_bytes, status, created_at, committed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'committed', $9, $10)
ON CONFLICT (project_id, id) DO NOTHING`,
		batch.ID, batch.ProjectID, batch.AgentID, batch.PipelineID, batch.Sequence,
		batch.PayloadHash[:], len(batch.Entries), batch.PayloadBytes, batch.CreatedAt, committedAt)
	if err != nil {
		return false, classifyError(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, classifyError(err)
	}
	return affected == 1, nil
}

func (r *IngestRepository) FindBatch(ctx context.Context, projectID domain.ProjectID, batchID domain.BatchID) (domain.StoredBatch, error) {
	return scanStoredBatch(r.q.QueryRowContext(ctx, `
SELECT id, project_id, agent_id, pipeline_id, sequence_no, payload_hash,
       entry_count, payload_bytes, status, created_at, committed_at, error_code
FROM ingest_batches
WHERE project_id = $1 AND id = $2`, projectID, batchID))
}

func (r *IngestRepository) InsertEntries(ctx context.Context, batch domain.Batch) error {
	if err := batch.Validate(); err != nil {
		return err
	}
	var sqlText strings.Builder
	sqlText.WriteString(`INSERT INTO log_entries (
    project_id, batch_id, batch_sequence, agent_id, pipeline_id,
    service, host, level, message, observed_at, attributes
) VALUES `)
	args := make([]any, 0, len(batch.Entries)*11)
	for i, entry := range batch.Entries {
		if i > 0 {
			sqlText.WriteString(",")
		}
		base := len(args)
		fmt.Fprintf(&sqlText, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6,
			base+7, base+8, base+9, base+10, base+11)
		attributes := entry.Attributes
		if attributes == nil {
			attributes = map[string]any{}
		}
		encoded, err := json.Marshal(attributes)
		if err != nil {
			return fmt.Errorf("encode entry %d attributes: %w", i, err)
		}
		args = append(args,
			batch.ProjectID, batch.ID, entry.BatchSequence, batch.AgentID, batch.PipelineID,
			entry.Service, entry.Host, entry.Level, entry.Message, entry.ObservedAt, encoded,
		)
	}
	_, err := r.q.ExecContext(ctx, sqlText.String(), args...)
	return classifyError(err)
}

func scanStoredBatch(row interface{ Scan(...any) error }) (domain.StoredBatch, error) {
	var batch domain.StoredBatch
	var hash []byte
	var committedAt sql.NullTime
	var errorCode sql.NullString
	if err := row.Scan(
		&batch.ID, &batch.ProjectID, &batch.AgentID, &batch.PipelineID, &batch.Sequence,
		&hash, &batch.EntryCount, &batch.PayloadBytes, &batch.Status, &batch.CreatedAt,
		&committedAt, &errorCode,
	); err != nil {
		return domain.StoredBatch{}, classifyError(err)
	}
	if len(hash) != len(batch.PayloadHash) || !batch.Status.Valid() {
		return domain.StoredBatch{}, ErrCorruptRow
	}
	copy(batch.PayloadHash[:], hash)
	batch.CommittedAt = nullableTime(committedAt)
	batch.ErrorCode = nullableString(errorCode)
	if batch.Status == domain.BatchCommitted && batch.CommittedAt == nil {
		return domain.StoredBatch{}, ErrCorruptRow
	}
	return batch, nil
}
