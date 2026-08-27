import type {
  Agent,
  APIKey,
  AuditEvent,
  CreatedAPIKey,
  EntryFilters,
  EntryPage,
  GlineApi,
  HealthStatus,
  PageResult,
  Pipeline,
  Project,
  QuarantineBatch,
  RetentionPolicy,
  UsageBucket,
} from './types'

export interface ApiErrorPayload {
  code?: string
  message?: string
  request_id?: string
  details?: unknown
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId?: string
  readonly details?: unknown

  constructor(status: number, payload: ApiErrorPayload = {}) {
    super(payload.message || fallbackMessage(status))
    this.name = 'ApiError'
    this.status = status
    this.code = payload.code || `http_${status}`
    this.requestId = payload.request_id
    this.details = payload.details
  }

  get isForbidden() {
    return this.status === 403
  }
}

function fallbackMessage(status: number) {
  if (status === 401) return 'API Token 无效或已经失效。'
  if (status === 403) return '当前 Token 没有执行此操作的权限。'
  if (status === 404) return '请求的资源不存在。'
  if (status === 409) return '资源状态已经变化，请刷新后重试。'
  if (status === 429) return '请求过于频繁，请稍后重试。'
  if (status >= 500) return 'Gline Server 暂时不可用。'
  return '请求未能完成。'
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null
}

function errorPayload(value: unknown): ApiErrorPayload {
  const record = asRecord(value)
  if (!record) return {}
  const nested = asRecord(record.error)
  const source = nested ?? record
  return {
    code: typeof source.code === 'string' ? source.code : undefined,
    message: typeof source.message === 'string' ? source.message : undefined,
    request_id: typeof source.request_id === 'string' ? source.request_id : undefined,
    details: source.details,
  }
}

function unwrapList<T>(value: unknown, keys: string[]): T[] {
  if (Array.isArray(value)) return value as T[]
  const record = asRecord(value)
  if (!record) return []
  for (const key of keys) {
    if (Array.isArray(record[key])) return record[key] as T[]
  }
  return []
}

function unwrapObject<T>(value: unknown, keys: string[] = []): T {
  const record = asRecord(value)
  if (record) {
    for (const key of keys) {
      if (asRecord(record[key])) return record[key] as T
    }
  }
  return value as T
}

export class HttpApi implements GlineApi {
  private readonly baseUrl: string
  private readonly token: string
  private readonly fetcher: typeof fetch

  constructor(input: { baseUrl?: string; token: string; fetcher?: typeof fetch }) {
    this.baseUrl = (input.baseUrl || '/api/v1').replace(/\/$/, '')
    this.token = input.token
    this.fetcher = input.fetcher ?? fetch
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    let response: Response
    try {
      response = await this.fetcher(`${this.baseUrl}${path}`, {
        ...init,
        headers: {
          Accept: 'application/json',
          ...(init.body ? { 'Content-Type': 'application/json' } : {}),
          Authorization: `Bearer ${this.token}`,
          ...init.headers,
        },
      })
    } catch (cause) {
      throw new ApiError(0, {
        code: 'network_error',
        message: '无法连接 Gline Server，请检查 API 地址和服务状态。',
        details: cause,
      })
    }

    const contentType = response.headers.get('content-type') ?? ''
    const data = response.status === 204
      ? undefined
      : contentType.includes('application/json')
        ? await response.json().catch(() => undefined)
        : await response.text().catch(() => undefined)

    if (!response.ok) throw new ApiError(response.status, errorPayload(data))
    return data as T
  }

  async listProjects() {
    return unwrapList<Project>(await this.request('/projects'), ['projects', 'items', 'data'])
  }

  async createProject(input: { slug: string; name: string }) {
    return unwrapObject<Project>(await this.request('/projects', { method: 'POST', body: JSON.stringify(input) }), ['project', 'data'])
  }

  async setProjectStatus(projectId: string, action: 'enable' | 'disable') {
    return unwrapObject<Project>(await this.request(`/projects/${projectId}/${action}`, { method: 'POST' }), ['project', 'data'])
  }

  async listKeys(projectId: string) {
    return unwrapList<APIKey>(await this.request(`/projects/${projectId}/keys`), ['keys', 'items', 'data'])
  }

  async createKey(projectId: string, input: { name: string; scopes: string[]; expires_at?: string | null }) {
    return unwrapObject<CreatedAPIKey>(await this.request(`/projects/${projectId}/keys`, { method: 'POST', body: JSON.stringify(input) }), ['key', 'data'])
  }

  async revokeKey(projectId: string, keyId: string) {
    await this.request(`/projects/${projectId}/keys/${keyId}/revoke`, { method: 'POST' })
  }

  async listAgents(projectId?: string) {
    const query = projectId ? `?project_id=${encodeURIComponent(projectId)}` : ''
    return unwrapList<Agent>(await this.request(`/agents${query}`), ['agents', 'items', 'data'])
  }

  async createAgent(projectId: string, input: { name: string; hostname: string; version: string }) {
	return unwrapObject<Agent>(await this.request(`/projects/${projectId}/agents`, { method: 'POST', body: JSON.stringify(input) }), ['agent', 'data'])
  }

  async listPipelines(projectId?: string) {
    const query = projectId ? `?project_id=${encodeURIComponent(projectId)}` : ''
    return unwrapList<Pipeline>(await this.request(`/pipelines${query}`), ['pipelines', 'items', 'data'])
  }

  async createPipeline(projectId: string, input: { agent_id: string; name: string; service: string; config: Record<string, unknown> }) {
	return unwrapObject<Pipeline>(await this.request(`/projects/${projectId}/pipelines`, { method: 'POST', body: JSON.stringify(input) }), ['pipeline', 'data'])
  }

  async setPipelineStatus(projectId: string, pipelineId: string, action: 'enable' | 'pause' | 'disable') {
    return unwrapObject<Pipeline>(await this.request(`/projects/${projectId}/pipelines/${pipelineId}/${action}`, { method: 'POST' }), ['pipeline', 'data'])
  }

  async searchEntries(filters: EntryFilters) {
    const params = new URLSearchParams({
      project_id: filters.projectId,
      from: filters.from,
      to: filters.to,
      limit: String(filters.limit ?? 100),
    })
    if (filters.service) params.set('service', filters.service)
    if (filters.host) params.set('host', filters.host)
    if (filters.level) params.set('level', filters.level)
    if (filters.q) params.set('q', filters.q)
    if (filters.cursor) params.set('cursor', filters.cursor)
    const data = await this.request<unknown>(`/entries?${params}`)
    const record = asRecord(data)
    return {
      entries: unwrapList(record?.data ?? data, ['entries', 'items', 'data']),
      next_cursor: (typeof record?.next_cursor === 'string' ? record.next_cursor : null),
    } satisfies EntryPage
  }

  async getRetention(projectId: string) {
    return unwrapObject<RetentionPolicy>(await this.request(`/projects/${projectId}/retention`), ['policy', 'data'])
  }

  async updateRetention(projectId: string, input: Pick<RetentionPolicy, 'max_age_seconds' | 'max_bytes' | 'enabled'>) {
    return unwrapObject<RetentionPolicy>(await this.request(`/projects/${projectId}/retention`, { method: 'PUT', body: JSON.stringify(input) }), ['policy', 'data'])
  }

  async getUsage(projectId: string, from: string, to: string) {
    const params = new URLSearchParams({ from, to })
    return unwrapList<UsageBucket>(await this.request(`/projects/${projectId}/usage?${params}`), ['buckets', 'items', 'data'])
  }

  async listAudit(projectId?: string) {
    const query = projectId ? `?project_id=${encodeURIComponent(projectId)}` : ''
    const data = await this.request<unknown>(`/audit${query}`)
    const record = asRecord(data)
    return { items: unwrapList(data, ['events', 'items', 'data']), next_cursor: typeof record?.next_cursor === 'string' ? record.next_cursor : null } satisfies PageResult<AuditEvent>
  }

  async listQuarantine(projectId?: string) {
    const query = projectId ? `?project_id=${encodeURIComponent(projectId)}` : ''
    const data = await this.request<unknown>(`/quarantine${query}`)
    const record = asRecord(data)
    return { items: unwrapList(data, ['batches', 'items', 'data']), next_cursor: typeof record?.next_cursor === 'string' ? record.next_cursor : null } satisfies PageResult<QuarantineBatch>
  }

  async replayQuarantine(id: string) {
    return unwrapObject<QuarantineBatch>(await this.request(`/quarantine/${id}/replay`, { method: 'POST' }), ['batch', 'data'])
  }

  async discardQuarantine(id: string) {
    return unwrapObject<QuarantineBatch>(await this.request(`/quarantine/${id}/discard`, { method: 'POST' }), ['batch', 'data'])
  }

  async health(): Promise<HealthStatus> {
    const healthUrl = this.baseUrl.endsWith('/api/v1') ? `${this.baseUrl.slice(0, -7)}/readyz` : `${this.baseUrl}/readyz`
    try {
      const response = await this.fetcher(healthUrl, { headers: { Accept: 'application/json' } })
      const raw = response.headers.get('content-type')?.includes('json') ? await response.json() : {}
      const record = asRecord(raw)
      const reported = record?.status
      const status: HealthStatus['status'] = reported === 'healthy' || reported === 'degraded' || reported === 'unavailable'
        ? reported
        : response.ok ? 'healthy' : 'unavailable'
      return {
        ...(record ?? {}),
        status,
        observed_at: typeof record?.observed_at === 'string' ? record.observed_at : new Date().toISOString(),
      } as HealthStatus
    } catch {
      return { status: 'unavailable' as const, observed_at: new Date().toISOString() }
    }
  }
}
