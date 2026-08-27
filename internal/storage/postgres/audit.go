package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type AuditRepository struct{ q dbtx }

func (r *AuditRepository) Append(ctx context.Context, event domain.AuditEvent) (domain.AuditEvent, error) {
	if event.ActorType == "" || event.ActorID == "" || event.Action == "" ||
		event.Resource == "" || event.ResourceID == "" || !event.Outcome.Valid() {
		return domain.AuditEvent{}, fmt.Errorf("%w: audit event", domain.ErrInvalid)
	}
	if event.ProjectID != nil && !event.ProjectID.Valid() {
		return domain.AuditEvent{}, fmt.Errorf("%w: audit project", domain.ErrInvalid)
	}
	if len(event.Metadata) == 0 {
		event.Metadata = json.RawMessage(`{}`)
	}
	if !domain.ValidJSONObject(event.Metadata) {
		return domain.AuditEvent{}, fmt.Errorf("%w: audit metadata", domain.ErrInvalid)
	}
	return scanAudit(r.q.QueryRowContext(ctx, `
INSERT INTO audit_events (
    project_id, actor_type, actor_id, action, resource, resource_id, outcome, metadata
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, project_id, actor_type, actor_id, action, resource,
          resource_id, outcome, metadata, created_at`,
		event.ProjectID, event.ActorType, event.ActorID, event.Action,
		event.Resource, event.ResourceID, event.Outcome, []byte(event.Metadata)))
}

func (r *AuditRepository) List(ctx context.Context, projectID domain.ProjectID, before *time.Time, limit int) ([]domain.AuditEvent, error) {
	if !projectID.Valid() || limit <= 0 || limit > 500 {
		return nil, fmt.Errorf("%w: audit query", domain.ErrInvalid)
	}
	rows, err := r.q.QueryContext(ctx, `
SELECT id, project_id, actor_type, actor_id, action, resource,
       resource_id, outcome, metadata, created_at
FROM audit_events
WHERE project_id = $1 AND ($2::timestamptz IS NULL OR created_at < $2)
ORDER BY created_at DESC, id DESC
LIMIT $3`, projectID, before, limit)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()
	var result []domain.AuditEvent
	for rows.Next() {
		event, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, classifyError(rows.Err())
}

func scanAudit(row interface{ Scan(...any) error }) (domain.AuditEvent, error) {
	var event domain.AuditEvent
	var projectID sql.NullString
	var metadata []byte
	if err := row.Scan(
		&event.ID, &projectID, &event.ActorType, &event.ActorID, &event.Action,
		&event.Resource, &event.ResourceID, &event.Outcome, &metadata, &event.CreatedAt,
	); err != nil {
		return domain.AuditEvent{}, classifyError(err)
	}
	if projectID.Valid {
		id := domain.ProjectID(projectID.String)
		if !id.Valid() {
			return domain.AuditEvent{}, ErrCorruptRow
		}
		event.ProjectID = &id
	}
	event.Metadata = append(json.RawMessage(nil), metadata...)
	if !event.Outcome.Valid() || !domain.ValidJSONObject(event.Metadata) {
		return domain.AuditEvent{}, ErrCorruptRow
	}
	return event, nil
}
