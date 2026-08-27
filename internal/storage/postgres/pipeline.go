package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type PipelineRepository struct{ q dbtx }

func (r *PipelineRepository) Create(ctx context.Context, pipeline domain.Pipeline) (domain.Pipeline, error) {
	if err := pipeline.Validate(); err != nil {
		return domain.Pipeline{}, err
	}
	return scanPipeline(r.q.QueryRowContext(ctx, `
INSERT INTO pipelines (
    id, project_id, agent_id, name, service, config, config_version,
    status, reported_status, reported_at, last_error
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING id, project_id, agent_id, name, service, config, config_version,
          status, reported_status, reported_at, last_error, updated_at`,
		pipeline.ID, pipeline.ProjectID, pipeline.AgentID, pipeline.Name, pipeline.Service,
		[]byte(pipeline.Config), pipeline.ConfigVersion, pipeline.Status,
		pipeline.ReportedStatus, pipeline.ReportedAt, pipeline.LastError))
}

func (r *PipelineRepository) Get(ctx context.Context, projectID domain.ProjectID, pipelineID domain.PipelineID) (domain.Pipeline, error) {
	return scanPipeline(r.q.QueryRowContext(ctx, `
SELECT id, project_id, agent_id, name, service, config, config_version,
       status, reported_status, reported_at, last_error, updated_at
FROM pipelines WHERE project_id = $1 AND id = $2`, projectID, pipelineID))
}

func (r *PipelineRepository) List(ctx context.Context, projectID domain.ProjectID, limit int) ([]domain.Pipeline, error) {
	if !projectID.Valid() || limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("%w: pipeline list", domain.ErrInvalid)
	}
	rows, err := r.q.QueryContext(ctx, `
SELECT id, project_id, agent_id, name, service, config, config_version,
       status, reported_status, reported_at, last_error, updated_at
FROM pipelines WHERE project_id = $1
ORDER BY updated_at DESC, id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()
	pipelines := make([]domain.Pipeline, 0)
	for rows.Next() {
		pipeline, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		pipelines = append(pipelines, pipeline)
	}
	return pipelines, classifyError(rows.Err())
}

func (r *PipelineRepository) UpdateConfig(ctx context.Context, projectID domain.ProjectID, pipelineID domain.PipelineID, expectedVersion int64, config json.RawMessage) (domain.Pipeline, error) {
	if expectedVersion <= 0 || !domain.ValidJSONObject(config) {
		return domain.Pipeline{}, fmt.Errorf("%w: pipeline config", domain.ErrInvalid)
	}
	return scanPipeline(r.q.QueryRowContext(ctx, `
UPDATE pipelines
SET config = $4, config_version = config_version + 1, updated_at = now()
WHERE project_id = $1 AND id = $2 AND config_version = $3 AND status <> 'disabled'
RETURNING id, project_id, agent_id, name, service, config, config_version,
          status, reported_status, reported_at, last_error, updated_at`,
		projectID, pipelineID, expectedVersion, []byte(config)))
}

func (r *PipelineRepository) PauseByProject(ctx context.Context, projectID domain.ProjectID) (int64, error) {
	if !projectID.Valid() {
		return 0, fmt.Errorf("%w: project id", domain.ErrInvalid)
	}
	result, err := r.q.ExecContext(ctx, `
UPDATE pipelines SET status = 'paused', updated_at = now()
WHERE project_id = $1 AND status IN ('enabled', 'error')`, projectID)
	if err != nil {
		return 0, classifyError(err)
	}
	return result.RowsAffected()
}

func (r *PipelineRepository) SetDesiredStatus(ctx context.Context, projectID domain.ProjectID, pipelineID domain.PipelineID, status domain.PipelineStatus) (domain.Pipeline, error) {
	if !status.Valid() {
		return domain.Pipeline{}, fmt.Errorf("%w: pipeline status", domain.ErrInvalid)
	}
	return scanPipeline(r.q.QueryRowContext(ctx, `
UPDATE pipelines SET status = $3, updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING id, project_id, agent_id, name, service, config, config_version,
          status, reported_status, reported_at, last_error, updated_at`,
		projectID, pipelineID, status))
}

func (r *PipelineRepository) ReportStatus(ctx context.Context, projectID domain.ProjectID, pipelineID domain.PipelineID, status domain.ReportedPipelineStatus, reportedAt time.Time, lastError *string) (domain.Pipeline, error) {
	if !status.Valid() {
		return domain.Pipeline{}, fmt.Errorf("%w: reported pipeline status", domain.ErrInvalid)
	}
	return scanPipeline(r.q.QueryRowContext(ctx, `
UPDATE pipelines
SET reported_status = $3, reported_at = $4, last_error = $5, updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING id, project_id, agent_id, name, service, config, config_version,
          status, reported_status, reported_at, last_error, updated_at`,
		projectID, pipelineID, status, reportedAt, lastError))
}

func scanPipeline(row interface{ Scan(...any) error }) (domain.Pipeline, error) {
	var pipeline domain.Pipeline
	var config []byte
	var reportedAt sql.NullTime
	var lastError sql.NullString
	if err := row.Scan(
		&pipeline.ID, &pipeline.ProjectID, &pipeline.AgentID, &pipeline.Name, &pipeline.Service,
		&config, &pipeline.ConfigVersion, &pipeline.Status, &pipeline.ReportedStatus,
		&reportedAt, &lastError, &pipeline.UpdatedAt,
	); err != nil {
		return domain.Pipeline{}, classifyError(err)
	}
	pipeline.Config = append(json.RawMessage(nil), config...)
	pipeline.ReportedAt = nullableTime(reportedAt)
	pipeline.LastError = nullableString(lastError)
	if err := pipeline.Validate(); err != nil {
		return domain.Pipeline{}, fmt.Errorf("%w: %v", ErrCorruptRow, err)
	}
	return pipeline, nil
}
