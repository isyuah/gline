package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type AgentRepository struct{ q dbtx }

// Register makes retries with the same Project/name/hostname idempotent. A
// conflicting hostname or disabled existing Agent remains an explicit conflict.
func (r *AgentRepository) Register(ctx context.Context, agent domain.Agent) (domain.Agent, error) {
	if err := agent.Validate(); err != nil {
		return domain.Agent{}, err
	}
	created, err := scanAgent(r.q.QueryRowContext(ctx, `
INSERT INTO agents (id, project_id, name, hostname, version, status, last_heartbeat_at, last_seen_ip)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (project_id, name) DO NOTHING
RETURNING id, project_id, name, hostname, version, status,
          last_heartbeat_at, last_seen_ip::text, created_at, updated_at`,
		agent.ID, agent.ProjectID, agent.Name, agent.Hostname, agent.Version,
		agent.Status, agent.LastHeartbeat, nullableIPString(agent.LastSeenIP)))
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return domain.Agent{}, err
	}
	existing, err := r.GetByName(ctx, agent.ProjectID, agent.Name)
	if err != nil {
		return domain.Agent{}, err
	}
	if existing.Hostname != agent.Hostname || existing.Status == domain.AgentDisabled {
		return domain.Agent{}, ErrConflict
	}
	return existing, nil
}

func (r *AgentRepository) Create(ctx context.Context, agent domain.Agent) (domain.Agent, error) {
	if err := agent.Validate(); err != nil {
		return domain.Agent{}, err
	}
	return scanAgent(r.q.QueryRowContext(ctx, `
INSERT INTO agents (id, project_id, name, hostname, version, status, last_heartbeat_at, last_seen_ip)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, project_id, name, hostname, version, status,
          last_heartbeat_at, last_seen_ip::text, created_at, updated_at`,
		agent.ID, agent.ProjectID, agent.Name, agent.Hostname, agent.Version,
		agent.Status, agent.LastHeartbeat, nullableIPString(agent.LastSeenIP)))
}

func (r *AgentRepository) Get(ctx context.Context, projectID domain.ProjectID, agentID domain.AgentID) (domain.Agent, error) {
	return scanAgent(r.q.QueryRowContext(ctx, `
SELECT id, project_id, name, hostname, version, status,
       last_heartbeat_at, last_seen_ip::text, created_at, updated_at
FROM agents WHERE project_id = $1 AND id = $2`, projectID, agentID))
}

func (r *AgentRepository) GetByName(ctx context.Context, projectID domain.ProjectID, name string) (domain.Agent, error) {
	return scanAgent(r.q.QueryRowContext(ctx, `
SELECT id, project_id, name, hostname, version, status,
       last_heartbeat_at, last_seen_ip::text, created_at, updated_at
FROM agents WHERE project_id = $1 AND name = $2`, projectID, name))
}

func (r *AgentRepository) List(ctx context.Context, projectID domain.ProjectID, limit int) ([]domain.Agent, error) {
	if !projectID.Valid() || limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("%w: agent list", domain.ErrInvalid)
	}
	rows, err := r.q.QueryContext(ctx, `
SELECT id, project_id, name, hostname, version, status,
       last_heartbeat_at, last_seen_ip::text, created_at, updated_at
FROM agents WHERE project_id = $1
ORDER BY created_at, id LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()
	agents := make([]domain.Agent, 0)
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, classifyError(rows.Err())
}

func (r *AgentRepository) Heartbeat(ctx context.Context, projectID domain.ProjectID, agentID domain.AgentID, version string, seenAt time.Time, ip net.IP) (domain.Agent, error) {
	return scanAgent(r.q.QueryRowContext(ctx, `
UPDATE agents
SET version = $3, last_heartbeat_at = $4, last_seen_ip = $5,
    status = CASE WHEN status = 'stale' THEN 'active' ELSE status END,
    updated_at = now()
WHERE project_id = $1 AND id = $2 AND status <> 'disabled'
RETURNING id, project_id, name, hostname, version, status,
          last_heartbeat_at, last_seen_ip::text, created_at, updated_at`,
		projectID, agentID, version, seenAt, nullableIPString(ip)))
}

func (r *AgentRepository) MarkStaleBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("%w: stale agent limit", domain.ErrInvalid)
	}
	result, err := r.q.ExecContext(ctx, `
WITH candidates AS (
    SELECT id FROM agents
    WHERE status = 'active' AND (last_heartbeat_at IS NULL OR last_heartbeat_at < $1)
    ORDER BY last_heartbeat_at NULLS FIRST, id
    LIMIT $2
)
UPDATE agents a SET status = 'stale', updated_at = now()
FROM candidates c WHERE a.id = c.id`, before, limit)
	if err != nil {
		return 0, classifyError(err)
	}
	return result.RowsAffected()
}

func scanAgent(row interface{ Scan(...any) error }) (domain.Agent, error) {
	var agent domain.Agent
	var heartbeat sql.NullTime
	var ip sql.NullString
	if err := row.Scan(
		&agent.ID, &agent.ProjectID, &agent.Name, &agent.Hostname, &agent.Version,
		&agent.Status, &heartbeat, &ip, &agent.CreatedAt, &agent.UpdatedAt,
	); err != nil {
		return domain.Agent{}, classifyError(err)
	}
	agent.LastHeartbeat = nullableTime(heartbeat)
	if ip.Valid {
		agent.LastSeenIP = net.ParseIP(ip.String)
		if agent.LastSeenIP == nil {
			return domain.Agent{}, fmt.Errorf("%w: invalid agent ip", ErrCorruptRow)
		}
	}
	if err := agent.Validate(); err != nil {
		return domain.Agent{}, fmt.Errorf("%w: %v", ErrCorruptRow, err)
	}
	return agent, nil
}

func nullableIPString(ip net.IP) any {
	if ip == nil {
		return nil
	}
	return ip.String()
}
