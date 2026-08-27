import { ArchiveRestore, BarChart3, ClipboardList, Database, RotateCcw, Save, ShieldAlert, Trash2 } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { EmptyState, ErrorState, LoadingState } from '../components/AsyncState'
import { PageHeader } from '../components/PageHeader'
import { StatusBadge } from '../components/StatusBadge'
import { useProjects } from '../contexts/ProjectState'
import { useApi } from '../contexts/SessionState'
import { useAsync } from '../hooks/useAsync'
import { formatBytes, formatDateTime, formatRelative } from '../lib/format'
import type { GlineApi, RetentionPolicy } from '../lib/types'

type Tab = 'retention' | 'usage' | 'audit' | 'quarantine'

const tabs: { id: Tab; label: string; icon: typeof Database }[] = [
  { id: 'retention', label: '保留策略', icon: Database },
  { id: 'usage', label: '用量', icon: BarChart3 },
  { id: 'audit', label: '审计', icon: ClipboardList },
  { id: 'quarantine', label: '隔离区', icon: ShieldAlert },
]

export function OperationsPage() {
  const [tab, setTab] = useState<Tab>('retention')
  return <div className="page-stack"><PageHeader title="数据治理" description="保留策略、用量、审计和异常批次共享同一个 Project 边界。" /><div className="segmented tabs" role="tablist">{tabs.map(({ id, label, icon: Icon }) => <button type="button" role="tab" aria-selected={tab === id} className={tab === id ? 'active' : ''} key={id} onClick={() => setTab(id)}><Icon size={16} />{label}</button>)}</div>{tab === 'retention' && <RetentionView />}{tab === 'usage' && <UsageView />}{tab === 'audit' && <AuditView />}{tab === 'quarantine' && <QuarantineView />}</div>
}

function RetentionView() {
  const api = useApi()
  const { selected } = useProjects()
  const state = useAsync(() => selected ? api.getRetention(selected.id) : Promise.reject(new Error('请先选择 Project。')), [api, selected?.id])
  if (state.loading && !state.data) return <LoadingState label="正在读取保留策略" />
  if (state.error) return <ErrorState error={state.error} retry={state.reload} />
  if (!selected || !state.data) return <EmptyState title="没有保留策略" description="请先选择 Project。" />
  return <RetentionForm key={`${selected.id}:${state.data.updated_at}`} api={api} projectId={selected.id} projectName={selected.name} policy={state.data} />
}

function RetentionForm({ api, projectId, projectName, policy }: { api: GlineApi; projectId: string; projectName: string; policy: RetentionPolicy }) {
  const [days, setDays] = useState(() => Math.round(policy.max_age_seconds / 86_400))
  const [maxGb, setMaxGb] = useState(() => policy.max_bytes ? String(policy.max_bytes / 1_000_000_000) : '')
  const [enabled, setEnabled] = useState(policy.enabled)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<Error | null>(null)
  const [saved, setSaved] = useState(false)
  async function submit(event: FormEvent) { event.preventDefault(); setSaving(true); setSaveError(null); setSaved(false); try { await api.updateRetention(projectId, { max_age_seconds: days * 86_400, max_bytes: maxGb ? Number(maxGb) * 1_000_000_000 : null, enabled }); setSaved(true) } catch (cause) { setSaveError(cause instanceof Error ? cause : new Error('保存失败。')) } finally { setSaving(false) } }
  return <section className="content-section governance-panel"><header className="section-header"><div><h2>{projectName} 的保留策略</h2><p>后台任务按有界批次删除过期日志，不会同步阻塞接入。</p></div><small>更新于 {formatRelative(policy.updated_at)}</small></header>{saveError && <ErrorState error={saveError} />}{saved && <div className="inline-alert positive" role="status">策略已保存，下一轮 retention worker 将使用新配置。</div>}<form className="retention-form" onSubmit={submit}><label className="toggle-row"><div><strong>启用自动清理</strong><span>关闭后保留现有数据，不再运行 Project 级清理。</span></div><input type="checkbox" role="switch" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} /></label><div className="form-two"><label className="field"><span>最长保留天数</span><input type="number" min="1" max="3650" value={days} onChange={(event) => setDays(Number(event.target.value))} required /><small>基于日志接入时间进行有界删除。</small></label><label className="field"><span>最大存储量（GB）</span><input type="number" min="0.1" step="0.1" value={maxGb} onChange={(event) => setMaxGb(event.target.value)} placeholder="不限制" /><small>留空表示只按时间清理。</small></label></div><div className="policy-preview"><ArchiveRestore size={20} /><div><strong>当前策略预览</strong><p>{enabled ? `日志最长保留 ${days} 天${maxGb ? `，并限制在 ${maxGb} GB` : ''}。` : '自动清理已暂停。'}</p></div></div><footer className="form-actions"><button className="button primary" type="submit" disabled={saving}><Save size={16} />{saving ? '正在保存…' : '保存策略'}</button></footer></form></section>
}

function UsageView() {
  const api = useApi()
  const { selected } = useProjects()
  const [range] = useState(() => {
    const to = new Date()
    return { from: new Date(to.getTime() - 24 * 60 * 60_000).toISOString(), to: to.toISOString() }
  })
  const state = useAsync(() => selected ? api.getUsage(selected.id, range.from, range.to) : Promise.resolve([]), [api, selected?.id, range])
  if (state.loading) return <LoadingState label="正在汇总近 24 小时用量" />
  if (state.error) return <ErrorState error={state.error} retry={state.reload} />
  const data = state.data ?? []
  const totalEntries = data.reduce((sum, item) => sum + item.entries, 0)
  const totalBytes = data.reduce((sum, item) => sum + item.bytes, 0)
  const failures = data.reduce((sum, item) => sum + item.failed_batches, 0)
  const max = Math.max(1, ...data.map((item) => item.entries))
  return <section className="content-section governance-panel"><header className="section-header"><div><h2>近 24 小时用量</h2><p>基于持久化后的分钟桶，不代表计费承诺。</p></div></header><div className="usage-summary"><div><span>日志条数</span><strong>{totalEntries.toLocaleString()}</strong></div><div><span>接入字节</span><strong>{formatBytes(totalBytes)}</strong></div><div><span>失败批次</span><strong>{failures.toLocaleString()}</strong></div></div>{data.length === 0 ? <EmptyState title="暂无用量数据" description="成功接入日志后，usage worker 会生成分钟桶。" /> : <div className="usage-chart" aria-label="每小时接入条数">{[...data].reverse().map((bucket) => <div className="usage-bar-column" key={bucket.bucket_start} title={`${formatDateTime(bucket.bucket_start)}: ${bucket.entries} 条`}><span className="usage-bar" style={{ height: `${Math.max(4, bucket.entries / max * 100)}%` }} /><small>{new Date(bucket.bucket_start).getHours().toString().padStart(2, '0')}</small></div>)}</div>}</section>
}

function AuditView() {
  const api = useApi()
  const { selected } = useProjects()
  const state = useAsync(() => api.listAudit(selected?.id), [api, selected?.id])
  if (state.loading) return <LoadingState label="正在读取审计事件" />
  if (state.error) return <ErrorState error={state.error} retry={state.reload} />
  if (!state.data?.items.length) return <EmptyState title="没有审计事件" description="控制平面状态变化会以不可变事件记录在这里。" />
  return <section className="content-section governance-panel"><header className="section-header"><div><h2>审计事件</h2><p>展示操作者、动作、资源和结果，不包含 Secret。</p></div></header><div className="table-wrap"><table><thead><tr><th>时间</th><th>动作</th><th>资源</th><th>操作者</th><th>结果</th></tr></thead><tbody>{state.data.items.map((event) => <tr key={event.id}><td>{formatDateTime(event.created_at)}</td><td><code>{event.action}</code></td><td><strong>{event.resource}</strong><code>{event.resource_id}</code></td><td>{event.actor_type}<small>{event.actor_id}</small></td><td><StatusBadge status={event.outcome} /></td></tr>)}</tbody></table></div></section>
}

function QuarantineView() {
  const api = useApi()
  const { selected } = useProjects()
  const state = useAsync(() => api.listQuarantine(selected?.id), [api, selected?.id])
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<Error | null>(null)
  async function act(id: string, action: 'replay' | 'discard') { if (action === 'discard' && !window.confirm('丢弃后该隔离批次不会再自动重放。继续吗？')) return; setBusy(id); setError(null); try { if (action === 'replay') await api.replayQuarantine(id); else await api.discardQuarantine(id); state.reload() } catch (cause) { setError(cause instanceof Error ? cause : new Error('操作失败。')) } finally { setBusy(null) } }
  if (state.loading && !state.data) return <LoadingState label="正在读取隔离批次" />
  if (state.error) return <ErrorState error={state.error} retry={state.reload} />
  return <section className="content-section governance-panel"><header className="section-header"><div><h2>隔离批次</h2><p>检查失败原因后，显式重放或丢弃；操作会进入审计。</p></div></header>{error && <ErrorState error={error} />}{!state.data?.items.length ? <EmptyState title="隔离区为空" description="没有需要人工处置的失败批次。" /> : <div className="quarantine-list">{state.data.items.map((batch) => <article key={batch.id}><div className="quarantine-status"><StatusBadge status={batch.status} /><small>尝试 {batch.attempts} 次</small></div><div className="quarantine-main"><strong>{batch.error_code}</strong><p>{batch.error_detail}</p><div><code>{batch.batch_id}</code><span>{formatDateTime(batch.created_at)}</span></div></div><div className="row-actions">{batch.status === 'pending' && <><button type="button" className="button secondary" disabled={busy === batch.id} onClick={() => act(batch.id, 'replay')}><RotateCcw size={15} />重放</button><button type="button" className="icon-button danger" title="丢弃批次" disabled={busy === batch.id} onClick={() => act(batch.id, 'discard')}><Trash2 size={16} /></button></>}</div></article>)}</div>}</section>
}
