package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
	serverauth "github.com/isyuah/gline/internal/server/auth"
)

const controlProjectID domain.ProjectID = "11111111-1111-1111-1111-111111111111"

type projectRepository struct{ project domain.Project }

func (r *projectRepository) Create(_ context.Context, project domain.Project) (domain.Project, error) {
	r.project = project
	return project, nil
}
func (r *projectRepository) Get(context.Context, domain.ProjectID) (domain.Project, error) {
	return r.project, nil
}
func (r *projectRepository) SetStatus(_ context.Context, _ domain.ProjectID, status domain.ProjectStatus) (domain.Project, error) {
	r.project.Status = status
	return r.project, nil
}

type keyRepository struct{ created domain.APIKey }

func (r *keyRepository) Create(_ context.Context, key domain.APIKey) (domain.APIKey, error) {
	r.created = key
	return key, nil
}
func (r *keyRepository) Revoke(_ context.Context, _ domain.ProjectID, _ domain.APIKeyID, at time.Time) (domain.APIKey, error) {
	r.created.Status = domain.KeyRevoked
	r.created.RevokedAt = &at
	return r.created, nil
}

type agentRepository struct{}

func (agentRepository) Register(_ context.Context, agent domain.Agent) (domain.Agent, error) {
	return agent, nil
}
func (agentRepository) Get(_ context.Context, projectID domain.ProjectID, agentID domain.AgentID) (domain.Agent, error) {
	return domain.Agent{ID: agentID, ProjectID: projectID, Name: "agent", Hostname: "host", Status: domain.AgentActive}, nil
}
func (agentRepository) Heartbeat(_ context.Context, projectID domain.ProjectID, agentID domain.AgentID, version string, seenAt time.Time, ip net.IP) (domain.Agent, error) {
	return domain.Agent{ID: agentID, ProjectID: projectID, Name: "agent", Hostname: "host", Version: version, Status: domain.AgentActive, LastHeartbeat: &seenAt, LastSeenIP: ip}, nil
}

type auditRepository struct{ events []domain.AuditEvent }

func (r *auditRepository) Append(_ context.Context, event domain.AuditEvent) (domain.AuditEvent, error) {
	r.events = append(r.events, event)
	return event, nil
}

type retentionRepository struct{ policy domain.RetentionPolicy }

func (r *retentionRepository) UpsertPolicy(_ context.Context, policy domain.RetentionPolicy) (domain.RetentionPolicy, error) {
	r.policy = policy
	return policy, nil
}

type pipelineRepository struct {
	pipeline    domain.Pipeline
	updateCalls int
}

func (r *pipelineRepository) Create(_ context.Context, pipeline domain.Pipeline) (domain.Pipeline, error) {
	r.pipeline = pipeline
	return pipeline, nil
}

func (r *pipelineRepository) Get(context.Context, domain.ProjectID, domain.PipelineID) (domain.Pipeline, error) {
	return r.pipeline, nil
}

func (r *pipelineRepository) ListByAgent(_ context.Context, projectID domain.ProjectID, agentID domain.AgentID, _ int) ([]domain.Pipeline, error) {
	if r.pipeline.ProjectID == projectID && r.pipeline.AgentID == agentID {
		return []domain.Pipeline{r.pipeline}, nil
	}
	return nil, nil
}

func (r *pipelineRepository) UpdateConfig(_ context.Context, _ domain.ProjectID, _ domain.PipelineID, _ int64, config json.RawMessage) (domain.Pipeline, error) {
	r.updateCalls++
	r.pipeline.Config = config
	r.pipeline.ConfigVersion++
	return r.pipeline, nil
}

func (r *pipelineRepository) SetDesiredStatus(_ context.Context, _ domain.ProjectID, _ domain.PipelineID, status domain.PipelineStatus) (domain.Pipeline, error) {
	r.pipeline.Status = status
	return r.pipeline, nil
}

func (r *pipelineRepository) PauseByProject(_ context.Context, _ domain.ProjectID) (int64, error) {
	return 1, nil
}

func (r *pipelineRepository) ReportStatus(_ context.Context, _ domain.ProjectID, _ domain.PipelineID, status domain.ReportedPipelineStatus, reportedAt time.Time, lastError *string) (domain.Pipeline, error) {
	r.pipeline.ReportedStatus = status
	r.pipeline.ReportedAt = &reportedAt
	r.pipeline.LastError = lastError
	return r.pipeline, nil
}

func TestCreateProjectCreatesDefaultRetentionInSameTransaction(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	projects := &projectRepository{}
	retention := &retentionRepository{}
	audit := &auditRepository{}
	within := func(ctx context.Context, fn func(Repositories) error) error {
		return fn(Repositories{Projects: projects, Retention: retention, Audit: audit})
	}
	service, err := NewService(within,
		func() (string, error) { return string(controlProjectID), nil }, nil,
		func() time.Time { return now }, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	principal := serverauth.Principal{KeyID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", ProjectID: controlProjectID, Scopes: map[domain.Scope]struct{}{domain.ScopeProjectWrite: {}}}
	project, err := service.CreateProject(context.Background(), principal, CreateProjectInput{Slug: "demo", Name: "Demo"})
	if err != nil {
		t.Fatal(err)
	}
	if project.ID != controlProjectID || retention.policy.ProjectID != project.ID || retention.policy.MaxAge != DefaultRetentionMaxAge || !retention.policy.Enabled {
		t.Fatalf("project=%+v retention=%+v", project, retention.policy)
	}
	if len(audit.events) != 1 || audit.events[0].Action != "project.create" || audit.events[0].ProjectID == nil || *audit.events[0].ProjectID != project.ID {
		t.Fatalf("audit events = %+v", audit.events)
	}
}

func TestCreateKeyReturnsSecretOnceAndStoresOnlyHMAC(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	pepper := []byte("0123456789abcdef0123456789abcdef")
	projects := &projectRepository{project: domain.Project{ID: controlProjectID, Slug: "demo", Name: "Demo", Status: domain.ProjectActive}}
	keys := &keyRepository{}
	audit := &auditRepository{}
	within := func(ctx context.Context, fn func(Repositories) error) error {
		return fn(Repositories{Projects: projects, Keys: keys, Agents: agentRepository{}, Audit: audit})
	}
	service, err := NewService(within,
		func() (string, error) { return "55555555-5555-5555-5555-555555555555", nil },
		func(size int) ([]byte, error) { return []byte(strings.Repeat("s", size)), nil },
		func() time.Time { return now }, pepper)
	if err != nil {
		t.Fatal(err)
	}
	principal := serverauth.Principal{KeyID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", ProjectID: controlProjectID, Scopes: map[domain.Scope]struct{}{domain.ScopeKeyManage: {}}}
	created, err := service.CreateKey(context.Background(), principal, CreateKeyInput{ProjectID: controlProjectID, Scopes: []domain.Scope{domain.ScopeIngest}})
	if err != nil {
		t.Fatal(err)
	}
	prefix, secret, err := serverauth.ParseKey(created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if prefix != keys.created.Prefix || strings.Contains(string(keys.created.SecretHash), secret) {
		t.Fatalf("stored key leaked raw credential: %+v", keys.created)
	}
	want := serverauth.HashSecret(secret, pepper)
	if subtle.ConstantTimeCompare(want[:], keys.created.SecretHash) != 1 {
		t.Fatal("stored HMAC does not authenticate returned secret")
	}
	if len(audit.events) != 1 || audit.events[0].Action != "key.create" || strings.Contains(string(audit.events[0].Metadata), secret) {
		t.Fatalf("audit events = %+v", audit.events)
	}
}

func TestCreateKeyDoesNotReturnCredentialWhenCommitFails(t *testing.T) {
	commitErr := errors.New("commit failed")
	within := func(ctx context.Context, fn func(Repositories) error) error {
		projects := &projectRepository{project: domain.Project{ID: controlProjectID, Slug: "demo", Name: "Demo", Status: domain.ProjectActive}}
		if err := fn(Repositories{Projects: projects, Keys: &keyRepository{}, Agents: agentRepository{}, Audit: &auditRepository{}}); err != nil {
			return err
		}
		return commitErr
	}
	service, _ := NewService(within,
		func() (string, error) { return "55555555-5555-5555-5555-555555555555", nil },
		func(size int) ([]byte, error) { return []byte(strings.Repeat("s", size)), nil },
		time.Now, []byte("0123456789abcdef0123456789abcdef"))
	principal := serverauth.Principal{KeyID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", ProjectID: controlProjectID, Scopes: map[domain.Scope]struct{}{domain.ScopeKeyManage: {}}}
	created, err := service.CreateKey(context.Background(), principal, CreateKeyInput{ProjectID: controlProjectID, Scopes: []domain.Scope{domain.ScopeIngest}})
	if !errors.Is(err, commitErr) || created.Secret != "" {
		t.Fatalf("created=%+v error=%v", created, err)
	}
}

func TestHeartbeatRejectsPipelineOwnedByAnotherAgent(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	agentID := domain.AgentID("22222222-2222-2222-2222-222222222222")
	otherAgentID := domain.AgentID("99999999-9999-9999-9999-999999999999")
	pipelineID := domain.PipelineID("33333333-3333-3333-3333-333333333333")
	projects := &projectRepository{project: domain.Project{ID: controlProjectID, Slug: "demo", Name: "Demo", Status: domain.ProjectActive}}
	pipelines := &pipelineRepository{pipeline: domain.Pipeline{ID: pipelineID, ProjectID: controlProjectID, AgentID: otherAgentID, Name: "other", Service: "api", Config: []byte(`{}`), ConfigVersion: 1, Status: domain.PipelineEnabled, ReportedStatus: domain.PipelineRunning}}
	within := func(ctx context.Context, fn func(Repositories) error) error {
		return fn(Repositories{Projects: projects, Agents: agentRepository{}, Pipelines: pipelines})
	}
	service, _ := NewService(within, nil, nil, func() time.Time { return now }, []byte("0123456789abcdef0123456789abcdef"))
	principal := serverauth.Principal{KeyID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", ProjectID: controlProjectID, AgentID: &agentID, Scopes: map[domain.Scope]struct{}{domain.ScopeAgentWrite: {}}}
	_, err := service.Heartbeat(context.Background(), principal, HeartbeatInput{ProjectID: controlProjectID, AgentID: agentID, Version: "1", Pipelines: []PipelineReport{{ID: pipelineID, ConfigVersion: 1, Status: domain.PipelineRunning}}})
	if !errors.Is(err, ErrResourceBinding) {
		t.Fatalf("heartbeat error = %v", err)
	}
}

func TestHeartbeatReturnsDesiredPipelineControl(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	agentID := domain.AgentID("22222222-2222-2222-2222-222222222222")
	pipelineID := domain.PipelineID("33333333-3333-3333-3333-333333333333")
	projects := &projectRepository{project: domain.Project{ID: controlProjectID, Slug: "demo", Name: "Demo", Status: domain.ProjectActive}}
	pipelines := &pipelineRepository{pipeline: domain.Pipeline{ID: pipelineID, ProjectID: controlProjectID, AgentID: agentID, Name: "api", Service: "api", Config: []byte(`{}`), ConfigVersion: 3, Status: domain.PipelinePaused, ReportedStatus: domain.PipelineRunning}}
	within := func(ctx context.Context, fn func(Repositories) error) error {
		return fn(Repositories{Projects: projects, Agents: agentRepository{}, Pipelines: pipelines})
	}
	service, _ := NewService(within, nil, nil, func() time.Time { return now }, []byte("0123456789abcdef0123456789abcdef"))
	principal := serverauth.Principal{KeyID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", ProjectID: controlProjectID, AgentID: &agentID, Scopes: map[domain.Scope]struct{}{domain.ScopeAgentWrite: {}}}
	result, err := service.Heartbeat(t.Context(), principal, HeartbeatInput{ProjectID: controlProjectID, AgentID: agentID, Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pipelines) != 1 || result.Pipelines[0].ID != pipelineID || result.Pipelines[0].DesiredStatus != domain.PipelinePaused || result.Pipelines[0].ConfigVersion != 3 {
		t.Fatalf("heartbeat result = %+v", result)
	}
}

func TestUpdatePipelineConfigRejectsStaleVersionBeforeWrite(t *testing.T) {
	pipelineID := domain.PipelineID("33333333-3333-3333-3333-333333333333")
	pipelines := &pipelineRepository{pipeline: domain.Pipeline{ID: pipelineID, ProjectID: controlProjectID, AgentID: "22222222-2222-2222-2222-222222222222", Name: "api", Service: "api", Config: []byte(`{}`), ConfigVersion: 2, Status: domain.PipelineEnabled, ReportedStatus: domain.PipelineRunning}}
	within := func(ctx context.Context, fn func(Repositories) error) error {
		return fn(Repositories{Pipelines: pipelines, Audit: &auditRepository{}})
	}
	service, _ := NewService(within, nil, nil, time.Now, []byte("0123456789abcdef0123456789abcdef"))
	principal := serverauth.Principal{KeyID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", ProjectID: controlProjectID, Scopes: map[domain.Scope]struct{}{domain.ScopePipelineWrite: {}}}
	_, err := service.UpdatePipelineConfig(context.Background(), principal, controlProjectID, pipelineID, 1, []byte(`{"service":"api"}`))
	if !errors.Is(err, ErrVersionConflict) || pipelines.updateCalls != 0 {
		t.Fatalf("error=%v updateCalls=%d", err, pipelines.updateCalls)
	}
}
