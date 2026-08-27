package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/isyuah/gline/internal/domain"
	serverauth "github.com/isyuah/gline/internal/server/auth"
)

var (
	ErrVersionConflict = errors.New("pipeline config version conflict")
	ErrResourceBinding = errors.New("resource belongs to another parent")
	ErrDisabled        = errors.New("resource is disabled")
)

const DefaultRetentionMaxAge = 14 * 24 * time.Hour

type ProjectRepository interface {
	Create(context.Context, domain.Project) (domain.Project, error)
	Get(context.Context, domain.ProjectID) (domain.Project, error)
	SetStatus(context.Context, domain.ProjectID, domain.ProjectStatus) (domain.Project, error)
}

type APIKeyRepository interface {
	Create(context.Context, domain.APIKey) (domain.APIKey, error)
	Revoke(context.Context, domain.ProjectID, domain.APIKeyID, time.Time) (domain.APIKey, error)
}

type AgentRepository interface {
	Register(context.Context, domain.Agent) (domain.Agent, error)
	Get(context.Context, domain.ProjectID, domain.AgentID) (domain.Agent, error)
	Heartbeat(context.Context, domain.ProjectID, domain.AgentID, string, time.Time, net.IP) (domain.Agent, error)
}

type PipelineRepository interface {
	Create(context.Context, domain.Pipeline) (domain.Pipeline, error)
	Get(context.Context, domain.ProjectID, domain.PipelineID) (domain.Pipeline, error)
	UpdateConfig(context.Context, domain.ProjectID, domain.PipelineID, int64, json.RawMessage) (domain.Pipeline, error)
	SetDesiredStatus(context.Context, domain.ProjectID, domain.PipelineID, domain.PipelineStatus) (domain.Pipeline, error)
	ReportStatus(context.Context, domain.ProjectID, domain.PipelineID, domain.ReportedPipelineStatus, time.Time, *string) (domain.Pipeline, error)
	PauseByProject(context.Context, domain.ProjectID) (int64, error)
}

type AuditRepository interface {
	Append(context.Context, domain.AuditEvent) (domain.AuditEvent, error)
}

type RetentionRepository interface {
	UpsertPolicy(context.Context, domain.RetentionPolicy) (domain.RetentionPolicy, error)
}

type Repositories struct {
	Projects  ProjectRepository
	Keys      APIKeyRepository
	Agents    AgentRepository
	Pipelines PipelineRepository
	Retention RetentionRepository
	Audit     AuditRepository
}

type WithinTx func(context.Context, func(Repositories) error) error
type IDGenerator func() (string, error)
type SecretGenerator func(int) ([]byte, error)
type Clock func() time.Time

type Service struct {
	withinTx WithinTx
	newID    IDGenerator
	secret   SecretGenerator
	now      Clock
	pepper   []byte
}

func NewService(withinTx WithinTx, newID IDGenerator, secret SecretGenerator, now Clock, pepper []byte) (*Service, error) {
	if withinTx == nil || len(pepper) < 16 {
		return nil, errors.New("control service requires transaction runner and at least 16 pepper bytes")
	}
	if newID == nil {
		newID = NewUUIDv4
	}
	if secret == nil {
		secret = RandomBytes
	}
	if now == nil {
		now = time.Now
	}
	return &Service{withinTx: withinTx, newID: newID, secret: secret, now: now, pepper: append([]byte(nil), pepper...)}, nil
}

type CreateProjectInput struct {
	Slug string
	Name string
}

func (s *Service) CreateProject(ctx context.Context, principal serverauth.Principal, input CreateProjectInput) (domain.Project, error) {
	if err := principal.Require(domain.ScopeProjectWrite); err != nil {
		return domain.Project{}, err
	}
	id, err := s.projectID()
	if err != nil {
		return domain.Project{}, err
	}
	now := s.now().UTC()
	project := domain.Project{ID: id, Slug: strings.TrimSpace(input.Slug), Name: strings.TrimSpace(input.Name), Status: domain.ProjectActive, CreatedAt: now, UpdatedAt: now}
	if err := project.Validate(); err != nil {
		return domain.Project{}, err
	}
	err = s.withinTx(ctx, func(repos Repositories) error {
		if repos.Projects == nil || repos.Retention == nil || repos.Audit == nil {
			return errors.New("control transaction is missing project creation dependencies")
		}
		created, createErr := repos.Projects.Create(ctx, project)
		if createErr != nil {
			return createErr
		}
		project = created
		if _, retentionErr := repos.Retention.UpsertPolicy(ctx, domain.RetentionPolicy{
			ProjectID: project.ID,
			MaxAge:    DefaultRetentionMaxAge,
			Enabled:   true,
			UpdatedAt: now,
		}); retentionErr != nil {
			return retentionErr
		}
		return appendAudit(ctx, repos.Audit, principal, &project.ID, "project.create", "project", string(project.ID), domain.AuditSuccess, nil)
	})
	return project, err
}

func (s *Service) SetProjectStatus(ctx context.Context, principal serverauth.Principal, projectID domain.ProjectID, status domain.ProjectStatus) (domain.Project, error) {
	if err := principal.Require(domain.ScopeProjectWrite); err != nil {
		return domain.Project{}, err
	}
	if err := principal.RequireProject(projectID); err != nil {
		return domain.Project{}, err
	}
	if !status.Valid() {
		return domain.Project{}, fmt.Errorf("%w: project status", domain.ErrInvalid)
	}
	var project domain.Project
	err := s.withinTx(ctx, func(repos Repositories) error {
		current, err := repos.Projects.Get(ctx, projectID)
		if err != nil {
			return err
		}
		if current.Status == status {
			project = current
			return nil
		}
		project, err = repos.Projects.SetStatus(ctx, projectID, status)
		if err != nil {
			return err
		}
		if status == domain.ProjectDisabled {
			if _, err := repos.Pipelines.PauseByProject(ctx, projectID); err != nil {
				return err
			}
		}
		return appendAudit(ctx, repos.Audit, principal, &projectID, "project.status.set", "project", string(projectID), domain.AuditSuccess, map[string]any{"status": status})
	})
	return project, err
}

type CreateKeyInput struct {
	ProjectID domain.ProjectID
	AgentID   *domain.AgentID
	Name      string
	Scopes    []domain.Scope
	ExpiresAt *time.Time
}

type CreatedKey struct {
	Key    domain.APIKey
	Secret string
}

func (s *Service) CreateKey(ctx context.Context, principal serverauth.Principal, input CreateKeyInput) (CreatedKey, error) {
	if err := principal.Require(domain.ScopeKeyManage); err != nil {
		return CreatedKey{}, err
	}
	if err := principal.RequireProject(input.ProjectID); err != nil {
		return CreatedKey{}, err
	}
	return s.createKey(ctx, principal, input, nil)
}

func (s *Service) createKey(ctx context.Context, principal serverauth.Principal, input CreateKeyInput, revoke *domain.APIKeyID) (CreatedKey, error) {
	id, err := s.apiKeyID()
	if err != nil {
		return CreatedKey{}, err
	}
	random, err := s.secret(32)
	if err != nil {
		return CreatedKey{}, fmt.Errorf("generate api key secret: %w", err)
	}
	if len(random) < 24 {
		return CreatedKey{}, errors.New("secret generator returned fewer than 24 bytes")
	}
	secretPart := base64.RawURLEncoding.EncodeToString(random)
	prefix := "glk_" + strings.ReplaceAll(string(id), "-", "")[:12]
	raw := prefix + "." + secretPart
	hash := serverauth.HashSecret(secretPart, s.pepper)
	now := s.now().UTC()
	if input.ExpiresAt != nil && !input.ExpiresAt.After(now) {
		return CreatedKey{}, fmt.Errorf("%w: key expiry", domain.ErrInvalid)
	}
	key := domain.APIKey{
		ID: id, ProjectID: input.ProjectID, AgentID: input.AgentID, Name: strings.TrimSpace(input.Name), Prefix: prefix,
		SecretHash: hash[:], Scopes: append([]domain.Scope(nil), input.Scopes...),
		Status: domain.KeyActive, ExpiresAt: input.ExpiresAt, CreatedAt: now,
	}
	if err := key.Validate(); err != nil {
		return CreatedKey{}, err
	}
	err = s.withinTx(ctx, func(repos Repositories) error {
		project, err := repos.Projects.Get(ctx, input.ProjectID)
		if err != nil {
			return err
		}
		if err := project.CanIngest(); err != nil {
			return err
		}
		if input.AgentID != nil {
			if _, err := repos.Agents.Get(ctx, input.ProjectID, *input.AgentID); err != nil {
				return err
			}
		}
		created, err := repos.Keys.Create(ctx, key)
		if err != nil {
			return err
		}
		key = created
		if revoke != nil {
			if _, err := repos.Keys.Revoke(ctx, input.ProjectID, *revoke, now); err != nil {
				return err
			}
		}
		metadata := map[string]any{"scopes": input.Scopes, "rotated_from": revoke}
		return appendAudit(ctx, repos.Audit, principal, &input.ProjectID, "key.create", "api_key", string(key.ID), domain.AuditSuccess, metadata)
	})
	if err != nil {
		return CreatedKey{}, err
	}
	return CreatedKey{Key: key, Secret: raw}, nil
}

func (s *Service) RevokeKey(ctx context.Context, principal serverauth.Principal, projectID domain.ProjectID, keyID domain.APIKeyID) (domain.APIKey, error) {
	if err := principal.Require(domain.ScopeKeyManage); err != nil {
		return domain.APIKey{}, err
	}
	if err := principal.RequireProject(projectID); err != nil {
		return domain.APIKey{}, err
	}
	if !keyID.Valid() {
		return domain.APIKey{}, fmt.Errorf("%w: api key id", domain.ErrInvalid)
	}
	var key domain.APIKey
	err := s.withinTx(ctx, func(repos Repositories) error {
		var err error
		key, err = repos.Keys.Revoke(ctx, projectID, keyID, s.now().UTC())
		if err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, &projectID, "key.revoke", "api_key", string(keyID), domain.AuditSuccess, nil)
	})
	return key, err
}

func (s *Service) RotateKey(ctx context.Context, principal serverauth.Principal, oldKeyID domain.APIKeyID, input CreateKeyInput, revokeOld bool) (CreatedKey, error) {
	if err := principal.Require(domain.ScopeKeyManage); err != nil {
		return CreatedKey{}, err
	}
	if err := principal.RequireProject(input.ProjectID); err != nil {
		return CreatedKey{}, err
	}
	if !oldKeyID.Valid() {
		return CreatedKey{}, fmt.Errorf("%w: old api key id", domain.ErrInvalid)
	}
	if !revokeOld {
		return s.createKey(ctx, principal, input, nil)
	}
	return s.createKey(ctx, principal, input, &oldKeyID)
}

type RegisterAgentInput struct {
	ProjectID domain.ProjectID
	Name      string
	Hostname  string
	Version   string
}

func (s *Service) RegisterAgent(ctx context.Context, principal serverauth.Principal, input RegisterAgentInput) (domain.Agent, error) {
	if err := principal.Require(domain.ScopeAgentWrite); err != nil {
		return domain.Agent{}, err
	}
	if err := principal.RequireProject(input.ProjectID); err != nil {
		return domain.Agent{}, err
	}
	id, err := s.agentID()
	if err != nil {
		return domain.Agent{}, err
	}
	now := s.now().UTC()
	agent := domain.Agent{ID: id, ProjectID: input.ProjectID, Name: strings.TrimSpace(input.Name), Hostname: strings.TrimSpace(input.Hostname), Version: strings.TrimSpace(input.Version), Status: domain.AgentActive, CreatedAt: now, UpdatedAt: now}
	if err := agent.Validate(); err != nil {
		return domain.Agent{}, err
	}
	err = s.withinTx(ctx, func(repos Repositories) error {
		project, err := repos.Projects.Get(ctx, input.ProjectID)
		if err != nil {
			return err
		}
		if err := project.CanIngest(); err != nil {
			return err
		}
		agent, err = repos.Agents.Register(ctx, agent)
		if err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, &input.ProjectID, "agent.register", "agent", string(agent.ID), domain.AuditSuccess, nil)
	})
	return agent, err
}

type PipelineReport struct {
	ID        domain.PipelineID
	Status    domain.ReportedPipelineStatus
	LastError *string
}

type HeartbeatInput struct {
	ProjectID domain.ProjectID
	AgentID   domain.AgentID
	Version   string
	IP        net.IP
	Pipelines []PipelineReport
}

func (s *Service) Heartbeat(ctx context.Context, principal serverauth.Principal, input HeartbeatInput) (domain.Agent, error) {
	if err := principal.Require(domain.ScopeAgentWrite); err != nil {
		return domain.Agent{}, err
	}
	if err := principal.RequireProject(input.ProjectID); err != nil {
		return domain.Agent{}, err
	}
	if err := principal.RequireAgent(input.AgentID); err != nil {
		return domain.Agent{}, err
	}
	if len(input.Pipelines) > 256 || len(input.Version) > 128 {
		return domain.Agent{}, fmt.Errorf("%w: heartbeat limits", domain.ErrInvalid)
	}
	now := s.now().UTC()
	var agent domain.Agent
	err := s.withinTx(ctx, func(repos Repositories) error {
		project, err := repos.Projects.Get(ctx, input.ProjectID)
		if err != nil {
			return err
		}
		if err := project.CanIngest(); err != nil {
			return err
		}
		agent, err = repos.Agents.Heartbeat(ctx, input.ProjectID, input.AgentID, strings.TrimSpace(input.Version), now, input.IP)
		if err != nil {
			return err
		}
		seen := make(map[domain.PipelineID]struct{}, len(input.Pipelines))
		for _, report := range input.Pipelines {
			if !report.ID.Valid() || !report.Status.Valid() {
				return fmt.Errorf("%w: pipeline report", domain.ErrInvalid)
			}
			if _, duplicate := seen[report.ID]; duplicate {
				return fmt.Errorf("%w: duplicate pipeline report", domain.ErrInvalid)
			}
			seen[report.ID] = struct{}{}
			if report.LastError != nil && len(*report.LastError) > 2_048 {
				return fmt.Errorf("%w: pipeline error summary", domain.ErrInvalid)
			}
			pipeline, err := repos.Pipelines.Get(ctx, input.ProjectID, report.ID)
			if err != nil {
				return err
			}
			if pipeline.AgentID != input.AgentID {
				return ErrResourceBinding
			}
			if _, err := repos.Pipelines.ReportStatus(ctx, input.ProjectID, report.ID, report.Status, now, report.LastError); err != nil {
				return err
			}
		}
		return nil
	})
	return agent, err
}

type CreatePipelineInput struct {
	ProjectID domain.ProjectID
	AgentID   domain.AgentID
	Name      string
	Service   string
	Config    json.RawMessage
}

func (s *Service) CreatePipeline(ctx context.Context, principal serverauth.Principal, input CreatePipelineInput) (domain.Pipeline, error) {
	if err := principal.Require(domain.ScopePipelineWrite); err != nil {
		return domain.Pipeline{}, err
	}
	if err := principal.RequireProject(input.ProjectID); err != nil {
		return domain.Pipeline{}, err
	}
	id, err := s.pipelineID()
	if err != nil {
		return domain.Pipeline{}, err
	}
	pipeline := domain.Pipeline{ID: id, ProjectID: input.ProjectID, AgentID: input.AgentID, Name: strings.TrimSpace(input.Name), Service: strings.TrimSpace(input.Service), Config: append(json.RawMessage(nil), input.Config...), ConfigVersion: 1, Status: domain.PipelineEnabled, ReportedStatus: domain.PipelineStopped, UpdatedAt: s.now().UTC()}
	if err := pipeline.Validate(); err != nil {
		return domain.Pipeline{}, err
	}
	err = s.withinTx(ctx, func(repos Repositories) error {
		project, err := repos.Projects.Get(ctx, input.ProjectID)
		if err != nil {
			return err
		}
		if err := project.CanIngest(); err != nil {
			return err
		}
		agent, err := repos.Agents.Get(ctx, input.ProjectID, input.AgentID)
		if err != nil {
			return err
		}
		if agent.Status == domain.AgentDisabled {
			return ErrDisabled
		}
		pipeline, err = repos.Pipelines.Create(ctx, pipeline)
		if err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, &input.ProjectID, "pipeline.create", "pipeline", string(pipeline.ID), domain.AuditSuccess, nil)
	})
	return pipeline, err
}

func (s *Service) UpdatePipelineConfig(ctx context.Context, principal serverauth.Principal, projectID domain.ProjectID, pipelineID domain.PipelineID, expectedVersion int64, config json.RawMessage) (domain.Pipeline, error) {
	if err := principal.Require(domain.ScopePipelineWrite); err != nil {
		return domain.Pipeline{}, err
	}
	if err := principal.RequireProject(projectID); err != nil {
		return domain.Pipeline{}, err
	}
	var pipeline domain.Pipeline
	err := s.withinTx(ctx, func(repos Repositories) error {
		current, err := repos.Pipelines.Get(ctx, projectID, pipelineID)
		if err != nil {
			return err
		}
		if current.Status == domain.PipelineDisabled {
			return ErrDisabled
		}
		if current.ConfigVersion != expectedVersion {
			return ErrVersionConflict
		}
		pipeline, err = repos.Pipelines.UpdateConfig(ctx, projectID, pipelineID, expectedVersion, config)
		if err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, &projectID, "pipeline.config.update", "pipeline", string(pipelineID), domain.AuditSuccess, map[string]any{"config_version": pipeline.ConfigVersion})
	})
	return pipeline, err
}

func (s *Service) SetPipelineStatus(ctx context.Context, principal serverauth.Principal, projectID domain.ProjectID, pipelineID domain.PipelineID, status domain.PipelineStatus) (domain.Pipeline, error) {
	if err := principal.Require(domain.ScopePipelineWrite); err != nil {
		return domain.Pipeline{}, err
	}
	if err := principal.RequireProject(projectID); err != nil {
		return domain.Pipeline{}, err
	}
	if !status.Valid() {
		return domain.Pipeline{}, fmt.Errorf("%w: pipeline status", domain.ErrInvalid)
	}
	var pipeline domain.Pipeline
	err := s.withinTx(ctx, func(repos Repositories) error {
		var err error
		pipeline, err = repos.Pipelines.SetDesiredStatus(ctx, projectID, pipelineID, status)
		if err != nil {
			return err
		}
		return appendAudit(ctx, repos.Audit, principal, &projectID, "pipeline.status.set", "pipeline", string(pipelineID), domain.AuditSuccess, map[string]any{"status": status})
	})
	return pipeline, err
}

func appendAudit(ctx context.Context, repository AuditRepository, principal serverauth.Principal, projectID *domain.ProjectID, action, resource, resourceID string, outcome domain.AuditOutcome, metadata map[string]any) error {
	if repository == nil {
		return errors.New("control transaction has no audit repository")
	}
	encoded := json.RawMessage(`{}`)
	if metadata != nil {
		var err error
		encoded, err = json.Marshal(metadata)
		if err != nil {
			return err
		}
	}
	_, err := repository.Append(ctx, domain.AuditEvent{ProjectID: projectID, ActorType: "api_key", ActorID: string(principal.KeyID), Action: action, Resource: resource, ResourceID: resourceID, Outcome: outcome, Metadata: encoded})
	return err
}

func (s *Service) projectID() (domain.ProjectID, error) {
	value, err := s.newID()
	id := domain.ProjectID(value)
	if err != nil || !id.Valid() {
		return "", fmt.Errorf("generate project id: %w", firstError(err, domain.ErrInvalid))
	}
	return id, nil
}

func (s *Service) apiKeyID() (domain.APIKeyID, error) {
	value, err := s.newID()
	id := domain.APIKeyID(value)
	if err != nil || !id.Valid() {
		return "", fmt.Errorf("generate api key id: %w", firstError(err, domain.ErrInvalid))
	}
	return id, nil
}

func (s *Service) agentID() (domain.AgentID, error) {
	value, err := s.newID()
	id := domain.AgentID(value)
	if err != nil || !id.Valid() {
		return "", fmt.Errorf("generate agent id: %w", firstError(err, domain.ErrInvalid))
	}
	return id, nil
}

func (s *Service) pipelineID() (domain.PipelineID, error) {
	value, err := s.newID()
	id := domain.PipelineID(value)
	if err != nil || !id.Valid() {
		return "", fmt.Errorf("generate pipeline id: %w", firstError(err, domain.ErrInvalid))
	}
	return id, nil
}

func firstError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func NewUUIDv4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value[:])
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32], nil
}

func RandomBytes(size int) ([]byte, error) {
	if size <= 0 || size > 1<<20 {
		return nil, fmt.Errorf("%w: random byte count", domain.ErrInvalid)
	}
	result := make([]byte, size)
	_, err := rand.Read(result)
	return result, err
}
