export type ProjectStatus = 'active' | 'disabled'
export type AgentStatus = 'active' | 'stale' | 'disabled'
export type PipelineDesiredStatus = 'enabled' | 'paused' | 'error' | 'disabled'
export type PipelineReportedStatus = 'running' | 'stopped' | 'error'
export type KeyStatus = 'active' | 'revoked' | 'expired'
export type QuarantineStatus = 'pending' | 'replaying' | 'resolved' | 'discarded'
export type AuditOutcome = 'success' | 'rejected' | 'failed'

export interface Project {
  id: string
  slug: string
  name: string
  status: ProjectStatus
  created_at: string
  updated_at?: string
}

export interface APIKey {
  id: string
  project_id: string
  name?: string
  prefix: string
  scopes: string[]
  status: KeyStatus
  expires_at: string | null
  last_used_at: string | null
  created_at: string
  revoked_at?: string | null
}

export interface CreatedAPIKey extends APIKey {
  secret: string
  warning?: string
}

export interface Agent {
  id: string
  project_id: string
  name: string
  hostname: string
  version: string
  status: AgentStatus
  last_heartbeat_at: string | null
  last_seen_ip?: string | null
  created_at: string
  updated_at: string
}

export interface Pipeline {
  id: string
  project_id: string
  agent_id: string
  name: string
  service: string
  config_version: number
  status: PipelineDesiredStatus
  reported_status: PipelineReportedStatus
  reported_at: string | null
  last_error: string | null
  updated_at: string
	config?: Record<string, unknown>
}

export interface LogEntry {
  id: number
  batch_id: string
  agent_id: string
  pipeline_id: string
  service: string
  host: string
  level: string
  message: string
  observed_at: string
  ingested_at: string
  attributes: Record<string, unknown>
}

export interface EntryPage {
  entries: LogEntry[]
  next_cursor: string | null
}

export interface EntryFilters {
  projectId: string
  from: string
  to: string
  service?: string
  host?: string
  level?: string
  q?: string
  cursor?: string
  limit?: number
}

export interface RetentionPolicy {
  project_id: string
  max_age_seconds: number
  max_bytes: number | null
  enabled: boolean
  updated_at: string
}

export interface UsageBucket {
  project_id: string
  bucket_start: string
  entries: number
  bytes: number
  failed_batches: number
}

export interface AuditEvent {
  id: number
  project_id: string | null
  actor_type: string
  actor_id: string
  action: string
  resource: string
  resource_id: string
  outcome: AuditOutcome
  metadata: Record<string, unknown>
  created_at: string
}

export interface QuarantineBatch {
  id: string
  project_id: string
  batch_id: string
  error_code: string
  error_detail: string
  status: QuarantineStatus
  attempts: number
  created_at: string
  claimed_at: string | null
  resolved_at: string | null
}

export interface HealthStatus {
  status: 'healthy' | 'degraded' | 'unavailable'
  version?: string
  checks?: Record<string, { status: string; latency_ms?: number; message?: string }>
  observed_at: string
}

export interface DashboardSnapshot {
  projects: Project[]
  agents: Agent[]
  pipelines: Pipeline[]
  usage: UsageBucket[]
}

export interface PageResult<T> {
  items: T[]
  next_cursor: string | null
}

export interface GlineApi {
  listProjects(): Promise<Project[]>
  createProject(input: { slug: string; name: string }): Promise<Project>
  setProjectStatus(projectId: string, action: 'enable' | 'disable'): Promise<Project>
  listKeys(projectId: string): Promise<APIKey[]>
  createKey(projectId: string, input: { name: string; scopes: string[]; expires_at?: string | null }): Promise<CreatedAPIKey>
  revokeKey(projectId: string, keyId: string): Promise<void>
  listAgents(projectId?: string): Promise<Agent[]>
	createAgent(projectId: string, input: { name: string; hostname: string; version: string }): Promise<Agent>
  listPipelines(projectId?: string): Promise<Pipeline[]>
	createPipeline(projectId: string, input: { agent_id: string; name: string; service: string; config: Record<string, unknown> }): Promise<Pipeline>
  setPipelineStatus(projectId: string, pipelineId: string, action: 'enable' | 'pause' | 'disable'): Promise<Pipeline>
  searchEntries(filters: EntryFilters): Promise<EntryPage>
  getRetention(projectId: string): Promise<RetentionPolicy>
  updateRetention(projectId: string, input: Pick<RetentionPolicy, 'max_age_seconds' | 'max_bytes' | 'enabled'>): Promise<RetentionPolicy>
  getUsage(projectId: string, from: string, to: string): Promise<UsageBucket[]>
  listAudit(projectId?: string): Promise<PageResult<AuditEvent>>
  listQuarantine(projectId?: string): Promise<PageResult<QuarantineBatch>>
  replayQuarantine(id: string): Promise<QuarantineBatch>
  discardQuarantine(id: string): Promise<QuarantineBatch>
  health(): Promise<HealthStatus>
}
