package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/protocol/ingestv1"
	serverauth "github.com/isyuah/gline/internal/server/auth"
	"github.com/isyuah/gline/internal/server/control"
	"github.com/isyuah/gline/internal/server/query"
)

func (h *Handler) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "healthy", "version": h.config.Version,
		"observed_at": time.Now().UTC(),
	})
}

func (h *Handler) ready(c *gin.Context) {
	if h.config.Draining() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "draining", "version": h.config.Version,
			"checks":      gin.H{"server": gin.H{"status": "draining"}},
			"observed_at": time.Now().UTC(),
		})
		return
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.config.ReadyTimeout)
	defer cancel()
	err := h.deps.Ready.Ping(ctx)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "unavailable", "version": h.config.Version,
			"checks":      gin.H{"database": gin.H{"status": "unavailable", "latency_ms": latency}},
			"observed_at": time.Now().UTC(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "healthy", "version": h.config.Version,
		"checks":      gin.H{"database": gin.H{"status": "healthy", "latency_ms": latency}},
		"observed_at": time.Now().UTC(),
	})
}

func (h *Handler) listProjects(c *gin.Context) {
	p := principal(c)
	if err := p.Require(domain.ScopeProjectRead); err != nil {
		writeError(c, err)
		return
	}
	var projects []domain.Project
	var err error
	if isBootstrap(c) {
		projects, err = h.deps.Projects.List(c.Request.Context(), 1000)
	} else {
		var project domain.Project
		project, err = h.deps.Projects.Get(c.Request.Context(), p.ProjectID)
		if err == nil {
			projects = []domain.Project{project}
		}
	}
	if err != nil {
		writeError(c, err)
		return
	}
	result := make([]projectDTO, len(projects))
	for index := range projects {
		result[index] = projectResponse(projects[index])
	}
	c.JSON(http.StatusOK, gin.H{"projects": result})
}

func (h *Handler) createProject(c *gin.Context) {
	if !isBootstrap(c) {
		writeError(c, errForbidden("only the bootstrap administrator can create projects"))
		return
	}
	var request struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := decodeJSON(c, h.config.MaxJSONBytes, &request); err != nil {
		writeError(c, err)
		return
	}
	project, err := h.deps.Control.CreateProject(c.Request.Context(), principal(c), control.CreateProjectInput{Slug: request.Slug, Name: request.Name})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"project": projectResponse(project)})
}

func (h *Handler) setProjectStatus(c *gin.Context) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopeProjectWrite)
	if !ok {
		return
	}
	status := domain.ProjectDisabled
	if strings.HasSuffix(c.FullPath(), "/enable") {
		status = domain.ProjectActive
	}
	project, err := h.deps.Control.SetProjectStatus(c.Request.Context(), p, p.ProjectID, status)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"project": projectResponse(project)})
}

func (h *Handler) listKeys(c *gin.Context) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopeKeyManage)
	if !ok {
		return
	}
	keys, err := h.deps.Keys.List(c.Request.Context(), p.ProjectID, 1000)
	if err != nil {
		writeError(c, err)
		return
	}
	result := make([]apiKeyDTO, len(keys))
	for index := range keys {
		result[index] = keyResponse(keys[index])
	}
	c.JSON(http.StatusOK, gin.H{"keys": result})
}

func (h *Handler) createKey(c *gin.Context) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopeKeyManage)
	if !ok {
		return
	}
	var request struct {
		Name      string          `json:"name"`
		AgentID   *domain.AgentID `json:"agent_id"`
		Scopes    []domain.Scope  `json:"scopes"`
		ExpiresAt *time.Time      `json:"expires_at"`
	}
	if err := decodeJSON(c, h.config.MaxJSONBytes, &request); err != nil {
		writeError(c, err)
		return
	}
	if len(request.Name) > 128 {
		writeError(c, errBadRequest("invalid_name", "name exceeds 128 bytes", nil))
		return
	}
	created, err := h.deps.Control.CreateKey(c.Request.Context(), p, control.CreateKeyInput{
		ProjectID: p.ProjectID, AgentID: request.AgentID, Name: request.Name, Scopes: request.Scopes, ExpiresAt: request.ExpiresAt,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"key": createdKeyResponse(created)})
}

func (h *Handler) revokeKey(c *gin.Context) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopeKeyManage)
	if !ok {
		return
	}
	keyID := domain.APIKeyID(c.Param("keyID"))
	if !keyID.Valid() {
		writeError(c, errBadRequest("invalid_key_id", "keyID must be a UUID", nil))
		return
	}
	if _, err := h.deps.Control.RevokeKey(c.Request.Context(), p, p.ProjectID, keyID); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) listAgents(c *gin.Context) {
	p, ok := h.queryProjectPrincipal(c, domain.ScopeAgentRead)
	if !ok {
		return
	}
	agents, err := h.deps.Agents.List(c.Request.Context(), p.ProjectID, 1000)
	if err != nil {
		writeError(c, err)
		return
	}
	result := make([]agentDTO, len(agents))
	for index := range agents {
		result[index] = agentResponse(agents[index])
	}
	c.JSON(http.StatusOK, gin.H{"agents": result})
}

func (h *Handler) createAgent(c *gin.Context) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopeAgentWrite)
	if !ok {
		return
	}
	var request struct {
		Name     string `json:"name"`
		Hostname string `json:"hostname"`
		Version  string `json:"version"`
	}
	if err := decodeJSON(c, h.config.MaxJSONBytes, &request); err != nil {
		writeError(c, err)
		return
	}
	agent, err := h.deps.Control.RegisterAgent(c.Request.Context(), p, control.RegisterAgentInput{
		ProjectID: p.ProjectID, Name: request.Name, Hostname: request.Hostname, Version: request.Version,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"agent": agentResponse(agent)})
}

func (h *Handler) heartbeat(c *gin.Context) {
	p, ok := h.queryProjectPrincipal(c, domain.ScopeAgentWrite)
	if !ok {
		return
	}
	agentID := domain.AgentID(c.Param("agentID"))
	if !agentID.Valid() {
		writeError(c, errBadRequest("invalid_agent_id", "agentID must be a UUID", nil))
		return
	}
	var request struct {
		Version   string `json:"version"`
		Pipelines []struct {
			ID        domain.PipelineID             `json:"id"`
			Status    domain.ReportedPipelineStatus `json:"status"`
			LastError *string                       `json:"last_error"`
		} `json:"pipelines"`
	}
	if err := decodeJSON(c, h.config.MaxJSONBytes, &request); err != nil {
		writeError(c, err)
		return
	}
	reports := make([]control.PipelineReport, len(request.Pipelines))
	for index, report := range request.Pipelines {
		reports[index] = control.PipelineReport{ID: report.ID, Status: report.Status, LastError: report.LastError}
	}
	agent, err := h.deps.Control.Heartbeat(c.Request.Context(), p, control.HeartbeatInput{
		ProjectID: p.ProjectID, AgentID: agentID, Version: request.Version,
		IP: net.ParseIP(c.RemoteIP()), Pipelines: reports,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"agent": agentResponse(agent)})
}

func (h *Handler) listPipelines(c *gin.Context) {
	p, ok := h.queryProjectPrincipal(c, domain.ScopePipelineRead)
	if !ok {
		return
	}
	pipelines, err := h.deps.Pipelines.List(c.Request.Context(), p.ProjectID, 1000)
	if err != nil {
		writeError(c, err)
		return
	}
	result := make([]pipelineDTO, len(pipelines))
	for index := range pipelines {
		result[index] = pipelineResponse(pipelines[index])
	}
	c.JSON(http.StatusOK, gin.H{"pipelines": result})
}

func (h *Handler) createPipeline(c *gin.Context) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopePipelineWrite)
	if !ok {
		return
	}
	var request struct {
		AgentID domain.AgentID  `json:"agent_id"`
		Name    string          `json:"name"`
		Service string          `json:"service"`
		Config  json.RawMessage `json:"config"`
	}
	if err := decodeJSON(c, h.config.MaxJSONBytes, &request); err != nil {
		writeError(c, err)
		return
	}
	pipeline, err := h.deps.Control.CreatePipeline(c.Request.Context(), p, control.CreatePipelineInput{
		ProjectID: p.ProjectID, AgentID: request.AgentID, Name: request.Name, Service: request.Service, Config: request.Config,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"pipeline": pipelineResponse(pipeline)})
}

func (h *Handler) updatePipeline(c *gin.Context) {
	p, pipelineID, ok := h.pipelinePrincipal(c)
	if !ok {
		return
	}
	var request struct {
		ExpectedVersion int64           `json:"expected_version"`
		Config          json.RawMessage `json:"config"`
	}
	if err := decodeJSON(c, h.config.MaxJSONBytes, &request); err != nil {
		writeError(c, err)
		return
	}
	pipeline, err := h.deps.Control.UpdatePipelineConfig(c.Request.Context(), p, p.ProjectID, pipelineID, request.ExpectedVersion, request.Config)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"pipeline": pipelineResponse(pipeline)})
}

func (h *Handler) setPipelineStatus(c *gin.Context) {
	p, pipelineID, ok := h.pipelinePrincipal(c)
	if !ok {
		return
	}
	statuses := map[string]domain.PipelineStatus{
		"enable": domain.PipelineEnabled, "pause": domain.PipelinePaused, "disable": domain.PipelineDisabled,
	}
	status, exists := statuses[c.Param("action")]
	if !exists {
		writeError(c, errBadRequest("invalid_pipeline_action", "pipeline action must be enable, pause, or disable", nil))
		return
	}
	pipeline, err := h.deps.Control.SetPipelineStatus(c.Request.Context(), p, p.ProjectID, pipelineID, status)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"pipeline": pipelineResponse(pipeline)})
}

func (h *Handler) pipelinePrincipal(c *gin.Context) (serverauth.Principal, domain.PipelineID, bool) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopePipelineWrite)
	if !ok {
		return serverauth.Principal{}, "", false
	}
	pipelineID := domain.PipelineID(c.Param("pipelineID"))
	if !pipelineID.Valid() {
		writeError(c, errBadRequest("invalid_pipeline_id", "pipelineID must be a UUID", nil))
		return serverauth.Principal{}, "", false
	}
	return p, pipelineID, true
}

func (h *Handler) ingestBatch(c *gin.Context) {
	if isBootstrap(c) {
		writeError(c, errForbidden("bootstrap credentials cannot ingest agent batches"))
		return
	}
	p := principal(c)
	request, payloadBytes, err := ingestv1.Decode(c.Request.Body, h.config.IngestLimits.MaxBodyBytes)
	if err != nil {
		writeError(c, err)
		return
	}
	batch, err := ingestv1.Normalize(request, p.ProjectID, payloadBytes, h.config.IngestLimits)
	if err != nil {
		writeError(c, err)
		return
	}
	result, err := h.deps.Ingest.Accept(c.Request.Context(), p, batch)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"batch_id": result.BatchID, "status": result.Status, "accepted_entries": result.AcceptedEntries,
	})
}

func (h *Handler) searchEntries(c *gin.Context) {
	p, ok := h.queryProjectPrincipal(c, domain.ScopeQuery)
	if !ok {
		return
	}
	limit, err := queryInt(c, "limit", 0, 500)
	if err != nil {
		writeError(c, err)
		return
	}
	params := query.Params{
		From: c.Query("from"), To: c.Query("to"), Services: c.QueryArray("service"),
		Hosts: c.QueryArray("host"), Levels: c.QueryArray("level"), Message: c.Query("q"),
		Limit: limit, Cursor: c.Query("cursor"),
	}
	page, err := h.deps.Query.Search(c.Request.Context(), p, params)
	if err != nil {
		writeError(c, err)
		return
	}
	entries := make([]entryDTO, len(page.Entries))
	for index := range page.Entries {
		entries[index] = entryResponse(page.Entries[index])
	}
	var next any
	if page.NextCursor != "" {
		next = page.NextCursor
	}
	c.JSON(http.StatusOK, gin.H{"entries": entries, "next_cursor": next})
}

func (h *Handler) getRetention(c *gin.Context) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopeRetentionManage)
	if !ok {
		return
	}
	policy, err := h.deps.Retention.GetPolicy(c.Request.Context(), p.ProjectID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"policy": retentionResponse(policy)})
}

func (h *Handler) setRetention(c *gin.Context) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopeRetentionManage)
	if !ok {
		return
	}
	var request struct {
		MaxAgeSeconds int64  `json:"max_age_seconds"`
		MaxBytes      *int64 `json:"max_bytes"`
		Enabled       bool   `json:"enabled"`
	}
	if err := decodeJSON(c, h.config.MaxJSONBytes, &request); err != nil {
		writeError(c, err)
		return
	}
	if request.MaxAgeSeconds <= 0 || request.MaxAgeSeconds > int64((10*365*24*time.Hour)/time.Second) || (request.MaxBytes != nil && *request.MaxBytes <= 0) {
		writeError(c, errBadRequest("invalid_retention", "retention limits are outside the allowed range", nil))
		return
	}
	policy, err := h.deps.Operations.SetRetention(c.Request.Context(), p, domain.RetentionPolicy{
		ProjectID: p.ProjectID, MaxAge: time.Duration(request.MaxAgeSeconds) * time.Second,
		MaxBytes: request.MaxBytes, Enabled: request.Enabled,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"policy": retentionResponse(policy)})
}

func (h *Handler) getUsage(c *gin.Context) {
	p, ok := h.projectPrincipal(c, c.Param("projectID"), domain.ScopeProjectRead)
	if !ok {
		return
	}
	from, err := parseRFC3339(c.Query("from"), "from")
	if err != nil {
		writeError(c, err)
		return
	}
	to, err := parseRFC3339(c.Query("to"), "to")
	if err != nil || !from.Before(to) || to.Sub(from) > 366*24*time.Hour {
		writeError(c, errBadRequest("invalid_time_range", "usage range must be positive and at most 366 days", nil))
		return
	}
	buckets, err := h.deps.Usage.List(c.Request.Context(), p.ProjectID, from, to)
	if err != nil {
		writeError(c, err)
		return
	}
	result := make([]usageDTO, len(buckets))
	for index := range buckets {
		result[index] = usageResponse(buckets[index])
	}
	c.JSON(http.StatusOK, gin.H{"buckets": result})
}

func (h *Handler) listAudit(c *gin.Context) {
	p, ok := h.queryProjectPrincipal(c, domain.ScopeAuditRead)
	if !ok {
		return
	}
	limit, err := queryInt(c, "limit", 100, 500)
	if err != nil {
		writeError(c, err)
		return
	}
	var before *time.Time
	if raw := c.Query("before"); raw != "" {
		parsed, parseErr := parseRFC3339(raw, "before")
		if parseErr != nil {
			writeError(c, parseErr)
			return
		}
		before = &parsed
	}
	events, err := h.deps.Audit.List(c.Request.Context(), p.ProjectID, before, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	result := make([]auditDTO, len(events))
	for index := range events {
		result[index] = auditResponse(events[index])
	}
	c.JSON(http.StatusOK, gin.H{"events": result, "next_cursor": nil})
}

func (h *Handler) listQuarantine(c *gin.Context) {
	p, ok := h.queryProjectPrincipal(c, domain.ScopeQuarantineRead)
	if !ok {
		return
	}
	limit, err := queryInt(c, "limit", 100, 1000)
	if err != nil {
		writeError(c, err)
		return
	}
	batches, err := h.deps.Quarantine.List(c.Request.Context(), p.ProjectID, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	result := make([]quarantineDTO, len(batches))
	for index := range batches {
		result[index] = quarantineResponse(batches[index])
	}
	c.JSON(http.StatusOK, gin.H{"batches": result, "next_cursor": nil})
}

func (h *Handler) replayQuarantine(c *gin.Context) {
	h.quarantineTransition(c, true)
}

func (h *Handler) discardQuarantine(c *gin.Context) {
	h.quarantineTransition(c, false)
}

func (h *Handler) quarantineTransition(c *gin.Context, replay bool) {
	id := domain.QuarantineID(c.Param("id"))
	if !id.Valid() {
		writeError(c, errBadRequest("invalid_quarantine_id", "quarantine id must be a UUID", nil))
		return
	}
	projectID := strings.TrimSpace(c.Query("project_id"))
	if projectID == "" && isBootstrap(c) {
		located, err := h.deps.Quarantine.FindProject(c.Request.Context(), id)
		if err != nil {
			writeError(c, err)
			return
		}
		projectID = string(located)
	}
	if projectID == "" {
		projectID = string(principal(c).ProjectID)
	}
	p, ok := h.projectPrincipal(c, projectID, domain.ScopeQuarantineReplay)
	if !ok {
		return
	}
	var batch domain.QuarantineBatch
	var err error
	if replay {
		batch, err = h.deps.Operations.ReplayQuarantine(c.Request.Context(), p, p.ProjectID, id)
	} else {
		batch, err = h.deps.Operations.DiscardQuarantine(c.Request.Context(), p, p.ProjectID, id)
	}
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"batch": quarantineResponse(batch)})
}

func (h *Handler) queryProjectPrincipal(c *gin.Context, scope domain.Scope) (serverauth.Principal, bool) {
	raw := strings.TrimSpace(c.Query("project_id"))
	if raw == "" && !isBootstrap(c) {
		raw = string(principal(c).ProjectID)
	}
	if raw == "" {
		writeError(c, errBadRequest("project_required", "project_id is required for bootstrap requests", nil))
		return serverauth.Principal{}, false
	}
	return h.projectPrincipal(c, raw, scope)
}
