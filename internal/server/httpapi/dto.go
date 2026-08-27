package httpapi

import (
	"encoding/json"
	"time"

	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/server/control"
)

type projectDTO struct {
	ID        domain.ProjectID     `json:"id"`
	Slug      string               `json:"slug"`
	Name      string               `json:"name"`
	Status    domain.ProjectStatus `json:"status"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
}

func projectResponse(value domain.Project) projectDTO {
	return projectDTO{value.ID, value.Slug, value.Name, value.Status, value.CreatedAt, value.UpdatedAt}
}

type apiKeyDTO struct {
	ID         domain.APIKeyID  `json:"id"`
	ProjectID  domain.ProjectID `json:"project_id"`
	AgentID    *domain.AgentID  `json:"agent_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	Prefix     string           `json:"prefix"`
	Scopes     []domain.Scope   `json:"scopes"`
	Status     domain.KeyStatus `json:"status"`
	ExpiresAt  *time.Time       `json:"expires_at"`
	LastUsedAt *time.Time       `json:"last_used_at"`
	CreatedAt  time.Time        `json:"created_at"`
	RevokedAt  *time.Time       `json:"revoked_at,omitempty"`
	Secret     string           `json:"secret,omitempty"`
	Warning    string           `json:"warning,omitempty"`
}

func keyResponse(value domain.APIKey) apiKeyDTO {
	return apiKeyDTO{
		ID: value.ID, ProjectID: value.ProjectID, AgentID: value.AgentID, Name: value.Name,
		Prefix: value.Prefix, Scopes: append([]domain.Scope(nil), value.Scopes...),
		Status: value.Status, ExpiresAt: value.ExpiresAt, LastUsedAt: value.LastUsedAt,
		CreatedAt: value.CreatedAt, RevokedAt: value.RevokedAt,
	}
}

func createdKeyResponse(value control.CreatedKey) apiKeyDTO {
	result := keyResponse(value.Key)
	result.Secret = value.Secret
	result.Warning = "This secret is shown once and cannot be recovered."
	return result
}

type agentDTO struct {
	ID            domain.AgentID     `json:"id"`
	ProjectID     domain.ProjectID   `json:"project_id"`
	Name          string             `json:"name"`
	Hostname      string             `json:"hostname"`
	Version       string             `json:"version"`
	Status        domain.AgentStatus `json:"status"`
	LastHeartbeat *time.Time         `json:"last_heartbeat_at"`
	LastSeenIP    *string            `json:"last_seen_ip,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

func agentResponse(value domain.Agent) agentDTO {
	var ip *string
	if value.LastSeenIP != nil {
		text := value.LastSeenIP.String()
		ip = &text
	}
	return agentDTO{
		ID: value.ID, ProjectID: value.ProjectID, Name: value.Name, Hostname: value.Hostname,
		Version: value.Version, Status: value.Status, LastHeartbeat: value.LastHeartbeat,
		LastSeenIP: ip, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

type pipelineDTO struct {
	ID             domain.PipelineID             `json:"id"`
	ProjectID      domain.ProjectID              `json:"project_id"`
	AgentID        domain.AgentID                `json:"agent_id"`
	Name           string                        `json:"name"`
	Service        string                        `json:"service"`
	Config         json.RawMessage               `json:"config,omitempty"`
	ConfigVersion  int64                         `json:"config_version"`
	Status         domain.PipelineStatus         `json:"status"`
	ReportedStatus domain.ReportedPipelineStatus `json:"reported_status"`
	ReportedAt     *time.Time                    `json:"reported_at"`
	LastError      *string                       `json:"last_error"`
	UpdatedAt      time.Time                     `json:"updated_at"`
}

func pipelineResponse(value domain.Pipeline) pipelineDTO {
	return pipelineDTO{
		ID: value.ID, ProjectID: value.ProjectID, AgentID: value.AgentID, Name: value.Name,
		Service: value.Service, Config: append(json.RawMessage(nil), value.Config...), ConfigVersion: value.ConfigVersion,
		Status: value.Status, ReportedStatus: value.ReportedStatus, ReportedAt: value.ReportedAt,
		LastError: value.LastError, UpdatedAt: value.UpdatedAt,
	}
}

type entryDTO struct {
	ID         domain.EntryID    `json:"id"`
	BatchID    domain.BatchID    `json:"batch_id"`
	AgentID    domain.AgentID    `json:"agent_id"`
	PipelineID domain.PipelineID `json:"pipeline_id"`
	Service    string            `json:"service"`
	Host       string            `json:"host"`
	Level      string            `json:"level"`
	Message    string            `json:"message"`
	ObservedAt time.Time         `json:"observed_at"`
	IngestedAt time.Time         `json:"ingested_at"`
	Attributes map[string]any    `json:"attributes"`
}

func entryResponse(value domain.Entry) entryDTO {
	return entryDTO{
		ID: value.ID, BatchID: value.BatchID, AgentID: value.AgentID, PipelineID: value.PipelineID,
		Service: value.Service, Host: value.Host, Level: value.Level, Message: value.Message,
		ObservedAt: value.ObservedAt, IngestedAt: value.IngestedAt, Attributes: value.Attributes,
	}
}

type retentionDTO struct {
	ProjectID     domain.ProjectID `json:"project_id"`
	MaxAgeSeconds int64            `json:"max_age_seconds"`
	MaxBytes      *int64           `json:"max_bytes"`
	Enabled       bool             `json:"enabled"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

func retentionResponse(value domain.RetentionPolicy) retentionDTO {
	return retentionDTO{value.ProjectID, int64(value.MaxAge / time.Second), value.MaxBytes, value.Enabled, value.UpdatedAt}
}

type usageDTO struct {
	ProjectID     domain.ProjectID `json:"project_id"`
	BucketStart   time.Time        `json:"bucket_start"`
	Entries       int64            `json:"entries"`
	Bytes         int64            `json:"bytes"`
	FailedBatches int64            `json:"failed_batches"`
}

func usageResponse(value domain.UsageBucket) usageDTO {
	return usageDTO{value.ProjectID, value.BucketStart, value.Entries, value.Bytes, value.FailedBatches}
}

type auditDTO struct {
	ID         domain.AuditID      `json:"id"`
	ProjectID  *domain.ProjectID   `json:"project_id"`
	ActorType  string              `json:"actor_type"`
	ActorID    string              `json:"actor_id"`
	Action     string              `json:"action"`
	Resource   string              `json:"resource"`
	ResourceID string              `json:"resource_id"`
	Outcome    domain.AuditOutcome `json:"outcome"`
	Metadata   json.RawMessage     `json:"metadata"`
	CreatedAt  time.Time           `json:"created_at"`
}

func auditResponse(value domain.AuditEvent) auditDTO {
	metadata := append(json.RawMessage(nil), value.Metadata...)
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	return auditDTO{value.ID, value.ProjectID, value.ActorType, value.ActorID, value.Action, value.Resource, value.ResourceID, value.Outcome, metadata, value.CreatedAt}
}

type quarantineDTO struct {
	ID          domain.QuarantineID     `json:"id"`
	ProjectID   domain.ProjectID        `json:"project_id"`
	BatchID     domain.BatchID          `json:"batch_id"`
	ErrorCode   string                  `json:"error_code"`
	ErrorDetail string                  `json:"error_detail"`
	Status      domain.QuarantineStatus `json:"status"`
	Attempts    int                     `json:"attempts"`
	CreatedAt   time.Time               `json:"created_at"`
	ClaimedAt   *time.Time              `json:"claimed_at"`
	ResolvedAt  *time.Time              `json:"resolved_at"`
}

func quarantineResponse(value domain.QuarantineBatch) quarantineDTO {
	return quarantineDTO{
		ID: value.ID, ProjectID: value.ProjectID, BatchID: value.BatchID,
		ErrorCode: value.ErrorCode, ErrorDetail: value.ErrorDetail, Status: value.Status,
		Attempts: value.Attempts, CreatedAt: value.CreatedAt, ClaimedAt: value.ClaimedAt,
		ResolvedAt: value.ResolvedAt,
	}
}
