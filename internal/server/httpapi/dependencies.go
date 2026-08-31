package httpapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/isyuah/gline/internal/domain"
	serverauth "github.com/isyuah/gline/internal/server/auth"
	"github.com/isyuah/gline/internal/server/control"
	"github.com/isyuah/gline/internal/server/ingest"
	"github.com/isyuah/gline/internal/server/query"
)

type Authenticator interface {
	Authenticate(context.Context, string) (serverauth.Principal, error)
}

type ControlService interface {
	CreateProject(context.Context, serverauth.Principal, control.CreateProjectInput) (domain.Project, error)
	SetProjectStatus(context.Context, serverauth.Principal, domain.ProjectID, domain.ProjectStatus) (domain.Project, error)
	CreateKey(context.Context, serverauth.Principal, control.CreateKeyInput) (control.CreatedKey, error)
	RevokeKey(context.Context, serverauth.Principal, domain.ProjectID, domain.APIKeyID) (domain.APIKey, error)
	RegisterAgent(context.Context, serverauth.Principal, control.RegisterAgentInput) (domain.Agent, error)
	Heartbeat(context.Context, serverauth.Principal, control.HeartbeatInput) (control.HeartbeatResult, error)
	CreatePipeline(context.Context, serverauth.Principal, control.CreatePipelineInput) (domain.Pipeline, error)
	UpdatePipelineConfig(context.Context, serverauth.Principal, domain.ProjectID, domain.PipelineID, int64, json.RawMessage) (domain.Pipeline, error)
	SetPipelineStatus(context.Context, serverauth.Principal, domain.ProjectID, domain.PipelineID, domain.PipelineStatus) (domain.Pipeline, error)
}

type IngestService interface {
	Accept(context.Context, serverauth.Principal, domain.Batch) (ingest.Result, error)
}

type QueryService interface {
	Search(context.Context, serverauth.Principal, query.Params) (query.Page, error)
}

type ProjectRepository interface {
	Get(context.Context, domain.ProjectID) (domain.Project, error)
	List(context.Context, int) ([]domain.Project, error)
}

type APIKeyRepository interface {
	List(context.Context, domain.ProjectID, int) ([]domain.APIKey, error)
}

type AgentRepository interface {
	List(context.Context, domain.ProjectID, int) ([]domain.Agent, error)
}

type PipelineRepository interface {
	List(context.Context, domain.ProjectID, int) ([]domain.Pipeline, error)
}

type RetentionRepository interface {
	GetPolicy(context.Context, domain.ProjectID) (domain.RetentionPolicy, error)
}

type UsageRepository interface {
	List(context.Context, domain.ProjectID, time.Time, time.Time) ([]domain.UsageBucket, error)
}

type AuditRepository interface {
	List(context.Context, domain.ProjectID, *time.Time, int) ([]domain.AuditEvent, error)
}

type QuarantineRepository interface {
	List(context.Context, domain.ProjectID, int) ([]domain.QuarantineBatch, error)
	FindProject(context.Context, domain.QuarantineID) (domain.ProjectID, error)
}

// OperationsService owns state transitions that do not belong in the HTTP
// adapter. Implementations are expected to authorize again and write audit
// events in the same transaction as the state change.
type OperationsService interface {
	SetRetention(context.Context, serverauth.Principal, domain.RetentionPolicy) (domain.RetentionPolicy, error)
	ReplayQuarantine(context.Context, serverauth.Principal, domain.ProjectID, domain.QuarantineID) (domain.QuarantineBatch, error)
	DiscardQuarantine(context.Context, serverauth.Principal, domain.ProjectID, domain.QuarantineID) (domain.QuarantineBatch, error)
}

type ReadyChecker interface {
	Ping(context.Context) error
}

type Dependencies struct {
	Authenticator Authenticator
	Control       ControlService
	Ingest        IngestService
	Query         QueryService
	Operations    OperationsService
	Projects      ProjectRepository
	Keys          APIKeyRepository
	Agents        AgentRepository
	Pipelines     PipelineRepository
	Retention     RetentionRepository
	Usage         UsageRepository
	Audit         AuditRepository
	Quarantine    QuarantineRepository
	Ready         ReadyChecker
}
