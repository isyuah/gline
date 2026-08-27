import type {
  Agent,
  APIKey,
  AuditEvent,
  CreatedAPIKey,
  EntryFilters,
  GlineApi,
  HealthStatus,
  LogEntry,
  Pipeline,
  Project,
  QuarantineBatch,
  RetentionPolicy,
  UsageBucket,
} from './types'

const now = Date.now()
const iso = (offsetMinutes = 0) => new Date(now + offsetMinutes * 60_000).toISOString()
const wait = <T>(value: T, ms = 180) => new Promise<T>((resolve) => setTimeout(() => resolve(value), ms))

const projects: Project[] = [
  { id: 'prj-shop', slug: 'shop', name: 'Shop Platform', status: 'active', created_at: iso(-24_000), updated_at: iso(-80) },
  { id: 'prj-labs', slug: 'labs', name: 'Labs', status: 'active', created_at: iso(-12_000), updated_at: iso(-145) },
  { id: 'prj-archive', slug: 'archive', name: 'Archive', status: 'disabled', created_at: iso(-32_000), updated_at: iso(-2_400) },
]

let keys: APIKey[] = [
  { id: 'key-1', project_id: 'prj-shop', name: 'production agents', prefix: 'glk_shop_7F2A', scopes: ['ingest'], status: 'active', expires_at: null, last_used_at: iso(-4), created_at: iso(-7_200) },
  { id: 'key-2', project_id: 'prj-shop', name: 'dashboard query', prefix: 'glk_shop_A91C', scopes: ['query', 'agent:read'], status: 'revoked', expires_at: null, last_used_at: iso(-1_200), created_at: iso(-6_000), revoked_at: iso(-900) },
]

let agents: Agent[] = [
  { id: 'agt-1', project_id: 'prj-shop', name: 'shop-prod-01', hostname: 'node-a', version: '0.4.1', status: 'active', last_heartbeat_at: iso(-1), last_seen_ip: '10.0.2.11', created_at: iso(-10_000), updated_at: iso(-1) },
  { id: 'agt-2', project_id: 'prj-shop', name: 'shop-prod-02', hostname: 'node-b', version: '0.4.1', status: 'stale', last_heartbeat_at: iso(-44), last_seen_ip: '10.0.2.12', created_at: iso(-9_000), updated_at: iso(-44) },
  { id: 'agt-3', project_id: 'prj-labs', name: 'labs-dev', hostname: 'devbox', version: '0.4.0', status: 'active', last_heartbeat_at: iso(-2), last_seen_ip: '10.0.8.3', created_at: iso(-3_000), updated_at: iso(-2) },
]

let pipelines: Pipeline[] = [
  { id: 'pipe-1', project_id: 'prj-shop', agent_id: 'agt-1', name: 'api-json', service: 'checkout-api', config_version: 4, status: 'enabled', reported_status: 'running', reported_at: iso(-1), last_error: null, updated_at: iso(-40) },
  { id: 'pipe-2', project_id: 'prj-shop', agent_id: 'agt-2', name: 'worker-text', service: 'order-worker', config_version: 2, status: 'enabled', reported_status: 'error', reported_at: iso(-44), last_error: 'source file is not readable', updated_at: iso(-44) },
  { id: 'pipe-3', project_id: 'prj-labs', agent_id: 'agt-3', name: 'sandbox', service: 'sandbox-api', config_version: 1, status: 'paused', reported_status: 'stopped', reported_at: iso(-3), last_error: null, updated_at: iso(-70) },
]

const messages = [
  ['error', 'checkout-api', 'node-a', 'payment authorization failed for order 79231'],
  ['warn', 'order-worker', 'node-b', 'retry queue depth exceeded warning threshold'],
  ['info', 'checkout-api', 'node-a', 'request completed status=200 duration_ms=47'],
  ['debug', 'sandbox-api', 'devbox', 'cache lookup key=inventory:sku-84 hit=true'],
  ['error', 'order-worker', 'node-b', 'database connection reset by peer'],
] as const

const entries: LogEntry[] = Array.from({ length: 36 }, (_, index) => {
  const item = messages[index % messages.length]
  return {
    id: 8000 - index,
    batch_id: `batch-${Math.floor(index / 5)}`,
    agent_id: item[2] === 'node-b' ? 'agt-2' : item[2] === 'devbox' ? 'agt-3' : 'agt-1',
    pipeline_id: item[1] === 'order-worker' ? 'pipe-2' : item[1] === 'sandbox-api' ? 'pipe-3' : 'pipe-1',
    level: item[0],
    service: item[1],
    host: item[2],
    message: item[3],
    observed_at: iso(-(index * 4 + 2)),
    ingested_at: iso(-(index * 4 + 1)),
    attributes: { environment: item[2] === 'devbox' ? 'development' : 'production', trace_id: `trace-${8000 - index}` },
  }
})

const retention = new Map<string, RetentionPolicy>(projects.map((project) => [project.id, {
  project_id: project.id,
  max_age_seconds: project.id === 'prj-shop' ? 14 * 86_400 : 7 * 86_400,
  max_bytes: project.id === 'prj-shop' ? 20_000_000_000 : null,
  enabled: true,
  updated_at: iso(-250),
}]))

const usage: UsageBucket[] = Array.from({ length: 12 }, (_, index) => ({
  project_id: index % 3 === 0 ? 'prj-labs' : 'prj-shop',
  bucket_start: iso(-(index * 60)),
  entries: 1250 - index * 43,
  bytes: 380_000 - index * 11_000,
  failed_batches: index === 2 || index === 8 ? 2 : 0,
}))

const audit: AuditEvent[] = [
  { id: 91, project_id: 'prj-shop', actor_type: 'api_key', actor_id: 'glk_admin', action: 'api_key.create', resource: 'api_key', resource_id: 'key-1', outcome: 'success', metadata: {}, created_at: iso(-300) },
  { id: 90, project_id: 'prj-shop', actor_type: 'api_key', actor_id: 'glk_admin', action: 'quarantine.replay', resource: 'batch', resource_id: 'batch-19', outcome: 'success', metadata: { attempts: 2 }, created_at: iso(-420) },
  { id: 89, project_id: 'prj-archive', actor_type: 'api_key', actor_id: 'glk_admin', action: 'project.disable', resource: 'project', resource_id: 'prj-archive', outcome: 'success', metadata: {}, created_at: iso(-1_200) },
]

let quarantine: QuarantineBatch[] = [
  { id: 'q-1', project_id: 'prj-shop', batch_id: 'batch-118', error_code: 'invalid_entry', error_detail: 'entry[14].observed_at is required', status: 'pending', attempts: 1, created_at: iso(-95), claimed_at: null, resolved_at: null },
  { id: 'q-2', project_id: 'prj-labs', batch_id: 'batch-91', error_code: 'pipeline_not_found', error_detail: 'pipeline does not exist in project', status: 'resolved', attempts: 2, created_at: iso(-600), claimed_at: null, resolved_at: iso(-540) },
]

export class MockApi implements GlineApi {
  async listProjects() { return wait([...projects]) }
  async createProject(input: { slug: string; name: string }) {
    const project: Project = { id: `prj-${crypto.randomUUID()}`, ...input, status: 'active', created_at: iso(), updated_at: iso() }
    projects.unshift(project)
    return wait(project)
  }
  async setProjectStatus(projectId: string, action: 'enable' | 'disable') {
    const project = projects.find((item) => item.id === projectId)!
    project.status = action === 'enable' ? 'active' : 'disabled'
    project.updated_at = iso()
    return wait({ ...project })
  }
  async listKeys(projectId: string) { return wait(keys.filter((key) => key.project_id === projectId)) }
  async createKey(projectId: string, input: { name: string; scopes: string[]; expires_at?: string | null }) {
    const key: CreatedAPIKey = { id: `key-${crypto.randomUUID()}`, project_id: projectId, ...input, prefix: `glk_demo_${Math.random().toString(16).slice(2, 6).toUpperCase()}`, secret: `glk_demo.${crypto.randomUUID().replaceAll('-', '')}`, status: 'active', expires_at: input.expires_at ?? null, last_used_at: null, created_at: iso(), warning: 'store this value now; it will not be shown again' }
    keys = [key, ...keys]
    return wait(key)
  }
  async revokeKey(projectId: string, keyId: string) {
    keys = keys.map((key) => key.id === keyId && key.project_id === projectId ? { ...key, status: 'revoked', revoked_at: iso() } : key)
    await wait(undefined)
  }
  async listAgents(projectId?: string) { return wait(agents.filter((item) => !projectId || item.project_id === projectId)) }
  async createAgent(projectId: string, input: { name: string; hostname: string; version: string }) {
	const agent: Agent = { id: `agt-${crypto.randomUUID()}`, project_id: projectId, ...input, status: 'active', last_heartbeat_at: null, created_at: iso(), updated_at: iso() }
	agents = [agent, ...agents]
	return wait(agent)
  }
  async listPipelines(projectId?: string) { return wait(pipelines.filter((item) => !projectId || item.project_id === projectId)) }
  async createPipeline(projectId: string, input: { agent_id: string; name: string; service: string; config: Record<string, unknown> }) {
	const pipeline: Pipeline = { id: `pipe-${crypto.randomUUID()}`, project_id: projectId, ...input, config_version: 1, status: 'enabled', reported_status: 'stopped', reported_at: null, last_error: null, updated_at: iso() }
	pipelines = [pipeline, ...pipelines]
	return wait(pipeline)
  }
  async setPipelineStatus(projectId: string, pipelineId: string, action: 'enable' | 'pause' | 'disable') {
    pipelines = pipelines.map((item) => item.id === pipelineId && item.project_id === projectId ? { ...item, status: action === 'pause' ? 'paused' : action === 'enable' ? 'enabled' : 'disabled', updated_at: iso() } : item)
    return wait({ ...pipelines.find((item) => item.id === pipelineId)! })
  }
  async searchEntries(filters: EntryFilters) {
    const from = new Date(filters.from).getTime()
    const to = new Date(filters.to).getTime()
    const offset = filters.cursor ? Number(filters.cursor) : 0
    const filtered = entries.filter((entry) => {
      const time = new Date(entry.observed_at).getTime()
      return time >= from && time <= to
        && (!filters.service || entry.service.includes(filters.service))
        && (!filters.host || entry.host.includes(filters.host))
        && (!filters.level || entry.level === filters.level)
        && (!filters.q || `${entry.message} ${JSON.stringify(entry.attributes)}`.toLowerCase().includes(filters.q.toLowerCase()))
    })
    const limit = Math.min(filters.limit ?? 20, 20)
    return wait({ entries: filtered.slice(offset, offset + limit), next_cursor: offset + limit < filtered.length ? String(offset + limit) : null })
  }
  async getRetention(projectId: string) { return wait({ ...retention.get(projectId)! }) }
  async updateRetention(projectId: string, input: Pick<RetentionPolicy, 'max_age_seconds' | 'max_bytes' | 'enabled'>) {
    const policy = { project_id: projectId, ...input, updated_at: iso() }
    retention.set(projectId, policy)
    return wait(policy)
  }
  async getUsage(projectId: string) { return wait(usage.filter((item) => item.project_id === projectId)) }
  async listAudit(projectId?: string) { return wait({ items: audit.filter((item) => !projectId || item.project_id === projectId), next_cursor: null }) }
  async listQuarantine(projectId?: string) { return wait({ items: quarantine.filter((item) => !projectId || item.project_id === projectId), next_cursor: null }) }
  async replayQuarantine(id: string) {
    quarantine = quarantine.map((item) => item.id === id ? { ...item, status: 'resolved', attempts: item.attempts + 1, resolved_at: iso(), claimed_at: null } : item)
    return wait({ ...quarantine.find((item) => item.id === id)! })
  }
  async discardQuarantine(id: string) {
    quarantine = quarantine.map((item) => item.id === id ? { ...item, status: 'discarded', resolved_at: iso(), claimed_at: null } : item)
    return wait({ ...quarantine.find((item) => item.id === id)! })
  }
  async health(): Promise<HealthStatus> {
    return wait({ status: 'healthy', version: '0.4.1-mock', observed_at: iso(), checks: { database: { status: 'healthy', latency_ms: 4 }, migrations: { status: 'healthy' }, background_workers: { status: 'healthy' } } })
  }
}
