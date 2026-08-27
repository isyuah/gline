import { Ban, Check, Copy, KeyRound, Plus, Power, RefreshCw, ShieldOff } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { EmptyState, ErrorState, LoadingState } from '../components/AsyncState'
import { Modal } from '../components/Modal'
import { PageHeader } from '../components/PageHeader'
import { StatusBadge } from '../components/StatusBadge'
import { useProjects } from '../contexts/ProjectState'
import { useApi } from '../contexts/SessionState'
import { useAsync } from '../hooks/useAsync'
import { formatDateTime, formatRelative } from '../lib/format'
import type { CreatedAPIKey } from '../lib/types'

const availableScopes = ['ingest', 'query', 'project:read', 'project:write', 'key:manage', 'agent:read', 'agent:write', 'pipeline:read', 'pipeline:write', 'quarantine:read', 'quarantine:replay', 'retention:manage', 'audit:read']

export function ProjectsPage() {
  const api = useApi()
  const { projects, selected, selectedId, setSelectedId, loading, error, reload } = useProjects()
  const [createOpen, setCreateOpen] = useState(false)
  const [keyOpen, setKeyOpen] = useState(false)
  const [secret, setSecret] = useState<CreatedAPIKey | null>(null)
  const [mutationError, setMutationError] = useState<Error | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const keys = useAsync(() => selected ? api.listKeys(selected.id) : Promise.resolve([]), [api, selected?.id])

  async function setStatus(action: 'enable' | 'disable') {
    if (!selected) return
    if (action === 'disable' && !window.confirm(`停用 ${selected.name}？Agent 接入和普通查询将被拒绝。`)) return
    setBusy(`project-${action}`)
    setMutationError(null)
    try { await api.setProjectStatus(selected.id, action); reload() }
    catch (cause) { setMutationError(cause instanceof Error ? cause : new Error('操作失败。')) }
    finally { setBusy(null) }
  }

  async function revokeKey(keyId: string) {
    if (!selected || !window.confirm('吊销后使用此 Key 的客户端将立即失去权限。继续吗？')) return
    setBusy(keyId)
    setMutationError(null)
    try { await api.revokeKey(selected.id, keyId); keys.reload() }
    catch (cause) { setMutationError(cause instanceof Error ? cause : new Error('操作失败。')) }
    finally { setBusy(null) }
  }

  return <div className="page-stack">
    <PageHeader title="Project 与访问密钥" description="管理租户边界、生命周期和最小权限凭证。" actions={<button type="button" className="button primary" onClick={() => setCreateOpen(true)}><Plus size={16} />创建 Project</button>} />
    {mutationError && <ErrorState error={mutationError} />}
    {loading ? <LoadingState /> : error ? <ErrorState error={error} retry={reload} /> : projects.length === 0 ? <EmptyState title="还没有 Project" description="创建第一个 Project 后才能注册 Agent 和接入日志。" action={<button className="button primary" onClick={() => setCreateOpen(true)}><Plus size={16} />创建 Project</button>} /> : <div className="project-layout">
      <section className="project-list" aria-label="Project 列表">
        {projects.map((project) => <button type="button" key={project.id} className={`project-list-item ${selectedId === project.id ? 'selected' : ''}`} onClick={() => setSelectedId(project.id)}><span className="project-monogram">{project.name.slice(0, 2).toUpperCase()}</span><span className="project-copy"><strong>{project.name}</strong><small>{project.slug}</small></span><StatusBadge status={project.status} /></button>)}
      </section>
      {selected && <div className="project-detail">
        <section className="content-section project-summary"><header className="section-header"><div><h2>{selected.name}</h2><p><code>{selected.slug}</code> · 创建于 {formatDateTime(selected.created_at)}</p></div><button type="button" className={`button ${selected.status === 'active' ? 'danger-ghost' : 'secondary'}`} disabled={Boolean(busy)} onClick={() => setStatus(selected.status === 'active' ? 'disable' : 'enable')}>{selected.status === 'active' ? <><Power size={16} />停用</> : <><RefreshCw size={16} />恢复</>}</button></header><div className="project-facts"><div><span>Project ID</span><code>{selected.id}</code></div><div><span>状态</span><StatusBadge status={selected.status} /></div><div><span>更新时间</span><strong>{formatRelative(selected.updated_at)}</strong></div></div></section>
        <section className="content-section"><header className="section-header"><div><h2>API Keys</h2><p>Secret 仅在创建时展示一次</p></div><button type="button" className="button secondary" onClick={() => setKeyOpen(true)}><KeyRound size={16} />创建 Key</button></header>
          {keys.loading && !keys.data ? <LoadingState label="正在加载密钥" /> : keys.error ? <ErrorState error={keys.error} retry={keys.reload} /> : !keys.data?.length ? <EmptyState title="没有访问密钥" description="创建一个最小权限 Key 供 Agent 或查询客户端使用。" /> : <div className="table-wrap"><table><thead><tr><th>名称 / Prefix</th><th>Scopes</th><th>最后使用</th><th>状态</th><th><span className="sr-only">操作</span></th></tr></thead><tbody>{keys.data.map((key) => <tr key={key.id}><td><strong>{key.name || '未命名 Key'}</strong><code>{key.prefix}</code></td><td><div className="tag-list">{key.scopes.map((scope) => <span key={scope}>{scope}</span>)}</div></td><td>{formatRelative(key.last_used_at)}</td><td><StatusBadge status={key.status} /></td><td className="actions-cell">{key.status === 'active' && <button type="button" className="icon-button danger" title="吊销 Key" disabled={busy === key.id} onClick={() => revokeKey(key.id)}><ShieldOff size={16} /></button>}</td></tr>)}</tbody></table></div>}
        </section>
      </div>}
    </div>}
    {createOpen && <CreateProjectModal onClose={() => setCreateOpen(false)} onCreated={(projectId) => { setCreateOpen(false); reload(); setSelectedId(projectId) }} />}
    {keyOpen && selected && <CreateKeyModal projectId={selected.id} onClose={() => setKeyOpen(false)} onCreated={(created) => { setKeyOpen(false); setSecret(created); keys.reload() }} />}
    {secret && <SecretModal secret={secret} onClose={() => setSecret(null)} />}
  </div>
}

function CreateProjectModal({ onClose, onCreated }: { onClose(): void; onCreated(id: string): void }) {
  const api = useApi()
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [error, setError] = useState<Error | null>(null)
  const [busy, setBusy] = useState(false)
  async function submit(event: FormEvent) { event.preventDefault(); setBusy(true); setError(null); try { const project = await api.createProject({ name: name.trim(), slug: slug.trim() }); onCreated(project.id) } catch (cause) { setError(cause instanceof Error ? cause : new Error('创建失败。')) } finally { setBusy(false) } }
  return <Modal open title="创建 Project" description="Project 是日志、凭证和运维策略的租户边界。" onClose={onClose}><form className="modal-body form-stack" onSubmit={submit}>{error && <ErrorState error={error} />}<label className="field"><span>显示名称</span><input value={name} onChange={(event) => { setName(event.target.value); if (!slug) setSlug(event.target.value.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')) }} required maxLength={255} autoFocus /></label><label className="field"><span>Slug</span><input value={slug} onChange={(event) => setSlug(event.target.value.toLowerCase())} pattern="[a-z0-9][a-z0-9-]{0,62}" required /><small>稳定标识，只允许小写字母、数字和连字符。</small></label><footer className="modal-actions"><button type="button" className="button secondary" onClick={onClose}>取消</button><button type="submit" className="button primary" disabled={busy}>{busy ? '正在创建…' : '创建 Project'}</button></footer></form></Modal>
}

function CreateKeyModal({ projectId, onClose, onCreated }: { projectId: string; onClose(): void; onCreated(key: CreatedAPIKey): void }) {
  const api = useApi()
  const [name, setName] = useState('')
  const [scopes, setScopes] = useState<string[]>(['ingest'])
  const [expires, setExpires] = useState('')
  const [error, setError] = useState<Error | null>(null)
  const [busy, setBusy] = useState(false)
  async function submit(event: FormEvent) { event.preventDefault(); setBusy(true); setError(null); try { onCreated(await api.createKey(projectId, { name: name.trim(), scopes, expires_at: expires ? new Date(expires).toISOString() : null })) } catch (cause) { setError(cause instanceof Error ? cause : new Error('创建失败。')) } finally { setBusy(false) } }
  function toggle(scope: string) { setScopes((current) => current.includes(scope) ? current.filter((item) => item !== scope) : [...current, scope]) }
  return <Modal open title="创建 API Key" description="只选择客户端确实需要的 Scope。" onClose={onClose}><form className="modal-body form-stack" onSubmit={submit}>{error && <ErrorState error={error} />}<label className="field"><span>用途名称</span><input value={name} onChange={(event) => setName(event.target.value)} placeholder="production agent" required autoFocus /></label><fieldset className="scope-field"><legend>Scopes</legend><div className="scope-grid">{availableScopes.map((scope) => <label key={scope}><input type="checkbox" checked={scopes.includes(scope)} onChange={() => toggle(scope)} /><span>{scope}</span></label>)}</div></fieldset><label className="field"><span>过期时间 <small>可选</small></span><input type="datetime-local" value={expires} onChange={(event) => setExpires(event.target.value)} /></label><footer className="modal-actions"><button type="button" className="button secondary" onClick={onClose}>取消</button><button type="submit" className="button primary" disabled={busy || scopes.length === 0}>{busy ? '正在创建…' : '创建 Key'}</button></footer></form></Modal>
}

export function SecretModal({ secret, onClose }: { secret: CreatedAPIKey; onClose(): void }) {
  const [copied, setCopied] = useState(false)
  async function copy() { await navigator.clipboard.writeText(secret.secret); setCopied(true) }
  return <Modal open title="保存 API Key" description="关闭后无法再次查看 Secret。" onClose={onClose}><div className="modal-body secret-body"><div className="inline-alert warning"><Ban size={18} />请立即保存到密码管理器。不要写入仓库、日志或聊天记录。</div><label className="field"><span>Secret</span><div className="copy-field"><code data-testid="created-secret">{secret.secret}</code><button type="button" className="icon-button" title="复制 Secret" onClick={copy}>{copied ? <Check size={17} /> : <Copy size={17} />}</button></div></label><div className="tag-list">{secret.scopes.map((scope) => <span key={scope}>{scope}</span>)}</div><footer className="modal-actions"><button type="button" className="button primary" onClick={onClose}>我已安全保存</button></footer></div></Modal>
}
