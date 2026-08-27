package domain

type ProjectStatus string

const (
	ProjectActive   ProjectStatus = "active"
	ProjectDisabled ProjectStatus = "disabled"
)

func (s ProjectStatus) Valid() bool { return s == ProjectActive || s == ProjectDisabled }

type KeyStatus string

const (
	KeyActive  KeyStatus = "active"
	KeyRevoked KeyStatus = "revoked"
	KeyExpired KeyStatus = "expired"
)

func (s KeyStatus) Valid() bool { return s == KeyActive || s == KeyRevoked || s == KeyExpired }

type AgentStatus string

const (
	AgentActive   AgentStatus = "active"
	AgentStale    AgentStatus = "stale"
	AgentDisabled AgentStatus = "disabled"
)

func (s AgentStatus) Valid() bool { return s == AgentActive || s == AgentStale || s == AgentDisabled }

type PipelineStatus string

const (
	PipelineEnabled  PipelineStatus = "enabled"
	PipelinePaused   PipelineStatus = "paused"
	PipelineError    PipelineStatus = "error"
	PipelineDisabled PipelineStatus = "disabled"
)

func (s PipelineStatus) Valid() bool {
	return s == PipelineEnabled || s == PipelinePaused || s == PipelineError || s == PipelineDisabled
}

type ReportedPipelineStatus string

const (
	PipelineRunning ReportedPipelineStatus = "running"
	PipelineStopped ReportedPipelineStatus = "stopped"
	PipelineFailed  ReportedPipelineStatus = "error"
)

func (s ReportedPipelineStatus) Valid() bool {
	return s == PipelineRunning || s == PipelineStopped || s == PipelineFailed
}

type BatchStatus string

const (
	BatchCommitted   BatchStatus = "committed"
	BatchRejected    BatchStatus = "rejected"
	BatchQuarantined BatchStatus = "quarantined"
)

func (s BatchStatus) Valid() bool {
	return s == BatchCommitted || s == BatchRejected || s == BatchQuarantined
}

type QuarantineStatus string

const (
	QuarantinePending   QuarantineStatus = "pending"
	QuarantineReplaying QuarantineStatus = "replaying"
	QuarantineResolved  QuarantineStatus = "resolved"
	QuarantineDiscarded QuarantineStatus = "discarded"
)

func (s QuarantineStatus) Valid() bool {
	return s == QuarantinePending || s == QuarantineReplaying || s == QuarantineResolved || s == QuarantineDiscarded
}

type AuditOutcome string

const (
	AuditSuccess  AuditOutcome = "success"
	AuditRejected AuditOutcome = "rejected"
	AuditFailed   AuditOutcome = "failed"
)

func (s AuditOutcome) Valid() bool {
	return s == AuditSuccess || s == AuditRejected || s == AuditFailed
}

type Scope string

const (
	ScopeIngest           Scope = "ingest"
	ScopeQuery            Scope = "query"
	ScopeProjectRead      Scope = "project:read"
	ScopeProjectWrite     Scope = "project:write"
	ScopeKeyManage        Scope = "key:manage"
	ScopeAgentRead        Scope = "agent:read"
	ScopeAgentWrite       Scope = "agent:write"
	ScopePipelineRead     Scope = "pipeline:read"
	ScopePipelineWrite    Scope = "pipeline:write"
	ScopeQuarantineRead   Scope = "quarantine:read"
	ScopeQuarantineReplay Scope = "quarantine:replay"
	ScopeRetentionManage  Scope = "retention:manage"
	ScopeAuditRead        Scope = "audit:read"
)

func (s Scope) Valid() bool {
	switch s {
	case ScopeIngest, ScopeQuery, ScopeProjectRead, ScopeProjectWrite,
		ScopeKeyManage, ScopeAgentRead, ScopeAgentWrite,
		ScopePipelineRead, ScopePipelineWrite, ScopeQuarantineRead,
		ScopeQuarantineReplay, ScopeRetentionManage, ScopeAuditRead:
		return true
	default:
		return false
	}
}
