package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/server/admission"
	serverauth "github.com/isyuah/gline/internal/server/auth"
	"github.com/isyuah/gline/internal/server/control"
	"github.com/isyuah/gline/internal/server/ingest"
	"github.com/isyuah/gline/internal/server/query"
)

const (
	testProjectID    domain.ProjectID    = "11111111-1111-4111-8111-111111111111"
	otherProjectID   domain.ProjectID    = "99999999-9999-4999-8999-999999999999"
	testAgentID      domain.AgentID      = "22222222-2222-4222-8222-222222222222"
	testPipelineID   domain.PipelineID   = "33333333-3333-4333-8333-333333333333"
	testBatchID      domain.BatchID      = "44444444-4444-4444-8444-444444444444"
	testKeyID        domain.APIKeyID     = "55555555-5555-4555-8555-555555555555"
	testQuarantineID domain.QuarantineID = "66666666-6666-4666-8666-666666666666"
	testBootstrap                        = "bootstrap-token-at-least-24-bytes"
)

type fakeAuthenticator struct {
	principal serverauth.Principal
	err       error
}

func (f fakeAuthenticator) Authenticate(context.Context, string) (serverauth.Principal, error) {
	return f.principal, f.err
}

type fakeControl struct{}

func (fakeControl) CreateProject(context.Context, serverauth.Principal, control.CreateProjectInput) (domain.Project, error) {
	return domain.Project{}, nil
}
func (fakeControl) SetProjectStatus(context.Context, serverauth.Principal, domain.ProjectID, domain.ProjectStatus) (domain.Project, error) {
	return domain.Project{}, nil
}
func (fakeControl) CreateKey(context.Context, serverauth.Principal, control.CreateKeyInput) (control.CreatedKey, error) {
	return control.CreatedKey{}, nil
}
func (fakeControl) RevokeKey(context.Context, serverauth.Principal, domain.ProjectID, domain.APIKeyID) (domain.APIKey, error) {
	return domain.APIKey{}, nil
}
func (fakeControl) RegisterAgent(context.Context, serverauth.Principal, control.RegisterAgentInput) (domain.Agent, error) {
	return domain.Agent{}, nil
}
func (fakeControl) Heartbeat(context.Context, serverauth.Principal, control.HeartbeatInput) (domain.Agent, error) {
	return domain.Agent{}, nil
}
func (fakeControl) CreatePipeline(context.Context, serverauth.Principal, control.CreatePipelineInput) (domain.Pipeline, error) {
	return domain.Pipeline{}, nil
}
func (fakeControl) UpdatePipelineConfig(context.Context, serverauth.Principal, domain.ProjectID, domain.PipelineID, int64, json.RawMessage) (domain.Pipeline, error) {
	return domain.Pipeline{}, nil
}
func (fakeControl) SetPipelineStatus(context.Context, serverauth.Principal, domain.ProjectID, domain.PipelineID, domain.PipelineStatus) (domain.Pipeline, error) {
	return domain.Pipeline{}, nil
}

type fakeIngest struct {
	batch     domain.Batch
	principal serverauth.Principal
	result    ingest.Result
	err       error
}

func (f *fakeIngest) Accept(_ context.Context, principal serverauth.Principal, batch domain.Batch) (ingest.Result, error) {
	f.batch, f.principal = batch, principal
	return f.result, f.err
}

type fakeQuery struct {
	params    query.Params
	principal serverauth.Principal
	page      query.Page
	err       error
	calls     int
}

func (f *fakeQuery) Search(_ context.Context, principal serverauth.Principal, params query.Params) (query.Page, error) {
	f.calls++
	f.params, f.principal = params, principal
	return f.page, f.err
}

type fakeProjects struct{ projects []domain.Project }

func (f fakeProjects) Get(_ context.Context, id domain.ProjectID) (domain.Project, error) {
	for _, project := range f.projects {
		if project.ID == id {
			return project, nil
		}
	}
	return domain.Project{}, errors.New("not found")
}
func (f fakeProjects) List(context.Context, int) ([]domain.Project, error) { return f.projects, nil }

type fakeKeys struct{ keys []domain.APIKey }

func (f fakeKeys) List(context.Context, domain.ProjectID, int) ([]domain.APIKey, error) {
	return f.keys, nil
}

type fakeAgents struct{}

func (fakeAgents) List(context.Context, domain.ProjectID, int) ([]domain.Agent, error) {
	return nil, nil
}

type fakePipelines struct{}

func (fakePipelines) List(context.Context, domain.ProjectID, int) ([]domain.Pipeline, error) {
	return nil, nil
}

type fakeRetention struct{}

func (fakeRetention) GetPolicy(context.Context, domain.ProjectID) (domain.RetentionPolicy, error) {
	return domain.RetentionPolicy{}, nil
}

type fakeUsage struct{}

func (fakeUsage) List(context.Context, domain.ProjectID, time.Time, time.Time) ([]domain.UsageBucket, error) {
	return nil, nil
}

type fakeAudit struct{}

func (fakeAudit) List(context.Context, domain.ProjectID, *time.Time, int) ([]domain.AuditEvent, error) {
	return nil, nil
}

type fakeQuarantine struct{ batches []domain.QuarantineBatch }

func (f fakeQuarantine) List(context.Context, domain.ProjectID, int) ([]domain.QuarantineBatch, error) {
	return f.batches, nil
}
func (f fakeQuarantine) FindProject(context.Context, domain.QuarantineID) (domain.ProjectID, error) {
	if len(f.batches) > 0 {
		return f.batches[0].ProjectID, nil
	}
	return testProjectID, nil
}

type fakeOperations struct{}

func (fakeOperations) SetRetention(context.Context, serverauth.Principal, domain.RetentionPolicy) (domain.RetentionPolicy, error) {
	return domain.RetentionPolicy{}, nil
}
func (fakeOperations) ReplayQuarantine(context.Context, serverauth.Principal, domain.ProjectID, domain.QuarantineID) (domain.QuarantineBatch, error) {
	return domain.QuarantineBatch{}, nil
}
func (fakeOperations) DiscardQuarantine(context.Context, serverauth.Principal, domain.ProjectID, domain.QuarantineID) (domain.QuarantineBatch, error) {
	return domain.QuarantineBatch{}, nil
}

type fakeReady struct{ err error }

func (f fakeReady) Ping(context.Context) error { return f.err }

type countingReady struct{ calls int }

func (f *countingReady) Ping(context.Context) error {
	f.calls++
	return errors.New("database unavailable")
}

func testPrincipal(scopes ...domain.Scope) serverauth.Principal {
	granted := make(map[domain.Scope]struct{}, len(scopes))
	for _, scope := range scopes {
		granted[scope] = struct{}{}
	}
	return serverauth.Principal{KeyID: testKeyID, ProjectID: testProjectID, Scopes: granted}
}

func testDependencies(principal serverauth.Principal) Dependencies {
	return Dependencies{
		Authenticator: fakeAuthenticator{principal: principal}, Control: fakeControl{},
		Ingest: &fakeIngest{}, Query: &fakeQuery{}, Operations: fakeOperations{},
		Projects: fakeProjects{}, Keys: fakeKeys{}, Agents: fakeAgents{}, Pipelines: fakePipelines{},
		Retention: fakeRetention{}, Usage: fakeUsage{}, Audit: fakeAudit{}, Quarantine: fakeQuarantine{},
		Ready: fakeReady{},
	}
}

func testRouter(t *testing.T, dependencies Dependencies) http.Handler {
	t.Helper()
	config := DefaultConfig()
	config.BootstrapToken = testBootstrap
	handler, err := New(config, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	return handler.Router()
}

func perform(handler http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("X-Request-ID", "trace-123")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestLiveAndReadyKeepDependencyAndDrainingSemanticsSeparate(t *testing.T) {
	ready := &countingReady{}
	dependencies := testDependencies(testPrincipal())
	dependencies.Ready = ready
	draining := false
	config := DefaultConfig()
	config.BootstrapToken = testBootstrap
	config.Draining = func() bool { return draining }
	handler, err := New(config, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	router := handler.Router()

	live := perform(router, http.MethodGet, "/livez", "", "")
	if live.Code != http.StatusOK || ready.calls != 0 {
		t.Fatalf("live status=%d ready calls=%d", live.Code, ready.calls)
	}
	unavailable := perform(router, http.MethodGet, "/readyz", "", "")
	if unavailable.Code != http.StatusServiceUnavailable || ready.calls != 1 {
		t.Fatalf("ready status=%d ready calls=%d", unavailable.Code, ready.calls)
	}
	draining = true
	shuttingDown := perform(router, http.MethodGet, "/readyz", "", "")
	if shuttingDown.Code != http.StatusServiceUnavailable || ready.calls != 1 || !strings.Contains(shuttingDown.Body.String(), "draining") {
		t.Fatalf("draining status=%d ready calls=%d body=%s", shuttingDown.Code, ready.calls, shuttingDown.Body.String())
	}
}

func TestAuthenticationErrorUsesStableModelAndRequestID(t *testing.T) {
	handler := testRouter(t, testDependencies(testPrincipal(domain.ScopeProjectRead)))
	response := perform(handler, http.MethodGet, "/api/v1/projects", "", "")
	if response.Code != http.StatusUnauthorized || response.Header().Get("X-Request-ID") != "trace-123" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "invalid_credential" || payload.Error.RequestID != "trace-123" {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestAPIKeyCannotSelectAnotherProject(t *testing.T) {
	queries := &fakeQuery{}
	dependencies := testDependencies(testPrincipal(domain.ScopeQuery))
	dependencies.Query = queries
	handler := testRouter(t, dependencies)
	response := perform(handler, http.MethodGet,
		"/api/v1/entries?project_id="+string(otherProjectID)+"&from=2026-08-24T00:00:00Z&to=2026-08-24T01:00:00Z",
		"api-key", "")
	if response.Code != http.StatusForbidden || queries.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, queries.calls, response.Body)
	}
}

func TestBootstrapCanProjectOntoSelectedTenant(t *testing.T) {
	queries := &fakeQuery{}
	dependencies := testDependencies(testPrincipal(domain.ScopeQuery))
	dependencies.Query = queries
	handler := testRouter(t, dependencies)
	response := perform(handler, http.MethodGet,
		"/api/v1/entries?project_id="+string(otherProjectID)+"&from=2026-08-24T00:00:00Z&to=2026-08-24T01:00:00Z",
		testBootstrap, "")
	if response.Code != http.StatusOK || queries.calls != 1 || queries.principal.ProjectID != otherProjectID {
		t.Fatalf("status=%d calls=%d principal=%+v body=%s", response.Code, queries.calls, queries.principal, response.Body)
	}
}

func TestIngestRouteDecodesNormalizesAndForwardsAuthenticatedProject(t *testing.T) {
	agentID := testAgentID
	p := testPrincipal(domain.ScopeIngest)
	p.AgentID = &agentID
	ingestion := &fakeIngest{result: ingest.Result{BatchID: testBatchID, Status: ingest.StatusAccepted, AcceptedEntries: 1}}
	dependencies := testDependencies(p)
	dependencies.Ingest = ingestion
	handler := testRouter(t, dependencies)
	body := `{"protocol_version":1,"batch_id":"44444444-4444-4444-8444-444444444444","agent_id":"22222222-2222-4222-8222-222222222222","pipeline_id":"33333333-3333-4333-8333-333333333333","sequence":7,"sent_at":"2026-08-24T00:00:00Z","entries":[{"sequence":0,"observed_at":"2026-08-24T00:00:00Z","level":"info","service":"api","host":"host-a","message":"ready","attributes":{"attempt":1}}]}`
	response := perform(handler, http.MethodPost, "/api/v1/batches", "api-key", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if ingestion.principal.ProjectID != testProjectID || ingestion.batch.ProjectID != testProjectID ||
		ingestion.batch.Entries[0].ProjectID != testProjectID || ingestion.batch.Entries[0].Level != "INFO" ||
		ingestion.batch.Sequence != 7 {
		t.Fatalf("principal=%+v batch=%+v", ingestion.principal, ingestion.batch)
	}
	if !strings.Contains(response.Body.String(), `"status":"accepted"`) {
		t.Fatalf("body=%s", response.Body)
	}
}

func TestIngestRateLimitReturnsRetryAfter(t *testing.T) {
	ingestion := &fakeIngest{err: &admission.LimitError{Reason: admission.ReasonProjectInflight, RetryAfter: 2500 * time.Millisecond}}
	dependencies := testDependencies(testPrincipal(domain.ScopeIngest))
	dependencies.Ingest = ingestion
	handler := testRouter(t, dependencies)
	body := `{"protocol_version":1,"batch_id":"44444444-4444-4444-8444-444444444444","agent_id":"22222222-2222-4222-8222-222222222222","pipeline_id":"33333333-3333-4333-8333-333333333333","sequence":7,"sent_at":"2026-08-24T00:00:00Z","entries":[{"sequence":0,"observed_at":"2026-08-24T00:00:00Z","level":"info","service":"api","host":"host-a","message":"ready","attributes":{}}]}`
	response := perform(handler, http.MethodPost, "/api/v1/batches", "api-key", body)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "3" || !strings.Contains(response.Body.String(), `"code":"rate_limited"`) {
		t.Fatalf("status=%d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
}

func TestIngestCapacityErrorIsNonRetryable(t *testing.T) {
	ingestion := &fakeIngest{err: admission.ErrBatchExceedsCapacity}
	dependencies := testDependencies(testPrincipal(domain.ScopeIngest))
	dependencies.Ingest = ingestion
	handler := testRouter(t, dependencies)
	body := `{"protocol_version":1,"batch_id":"44444444-4444-4444-8444-444444444444","agent_id":"22222222-2222-4222-8222-222222222222","pipeline_id":"33333333-3333-4333-8333-333333333333","sequence":7,"sent_at":"2026-08-24T00:00:00Z","entries":[{"sequence":0,"observed_at":"2026-08-24T00:00:00Z","level":"info","service":"api","host":"host-a","message":"ready","attributes":{}}]}`
	response := perform(handler, http.MethodPost, "/api/v1/batches", "api-key", body)
	if response.Code != http.StatusRequestEntityTooLarge || response.Header().Get("Retry-After") != "" ||
		!strings.Contains(response.Body.String(), `"code":"admission_capacity_exceeded"`) {
		t.Fatalf("status=%d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
}

func TestEntryRouteMapsBoundedQueryParameters(t *testing.T) {
	queries := &fakeQuery{page: query.Page{Entries: []domain.Entry{{ID: 42}}, NextCursor: "next"}}
	dependencies := testDependencies(testPrincipal(domain.ScopeQuery))
	dependencies.Query = queries
	handler := testRouter(t, dependencies)
	target := "/api/v1/entries?project_id=" + string(testProjectID) +
		"&from=2026-08-24T00:00:00Z&to=2026-08-24T01:00:00Z&service=api&service=worker&host=node-a&level=error&q=timeout&cursor=opaque&limit=25"
	response := perform(handler, http.MethodGet, target, "api-key", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if queries.calls != 1 || queries.principal.ProjectID != testProjectID || queries.params.From != "2026-08-24T00:00:00Z" ||
		queries.params.To != "2026-08-24T01:00:00Z" || len(queries.params.Services) != 2 ||
		queries.params.Hosts[0] != "node-a" || queries.params.Levels[0] != "error" ||
		queries.params.Message != "timeout" || queries.params.Cursor != "opaque" || queries.params.Limit != 25 {
		t.Fatalf("principal=%+v params=%+v", queries.principal, queries.params)
	}
}

func TestSensitiveStorageFieldsAreNotSerialized(t *testing.T) {
	dependencies := testDependencies(testPrincipal(domain.ScopeKeyManage, domain.ScopeQuarantineRead))
	dependencies.Keys = fakeKeys{keys: []domain.APIKey{
		{ID: testKeyID, ProjectID: testProjectID, Prefix: "glk_test", SecretHash: []byte("secret-hash"), Scopes: []domain.Scope{domain.ScopeQuery}, Status: domain.KeyActive},
	}}
	dependencies.Quarantine = fakeQuarantine{batches: []domain.QuarantineBatch{
		{ID: testQuarantineID, ProjectID: testProjectID, BatchID: testBatchID, Payload: []byte("raw-private-payload"), ErrorCode: "invalid", Status: domain.QuarantinePending},
	}}
	handler := testRouter(t, dependencies)
	keys := perform(handler, http.MethodGet, "/api/v1/projects/"+string(testProjectID)+"/keys", "api-key", "")
	quarantine := perform(handler, http.MethodGet, "/api/v1/quarantine?project_id="+string(testProjectID), "api-key", "")
	combined := keys.Body.String() + quarantine.Body.String()
	if keys.Code != http.StatusOK || quarantine.Code != http.StatusOK || strings.Contains(combined, "secret_hash") ||
		strings.Contains(combined, "secret-hash") || strings.Contains(combined, "raw-private-payload") || strings.Contains(combined, `"payload"`) {
		t.Fatalf("keys=%d %s quarantine=%d %s", keys.Code, keys.Body, quarantine.Code, quarantine.Body)
	}
}
