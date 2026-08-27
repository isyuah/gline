import { CirclePause, CirclePlay, OctagonX, Plus, RefreshCw, ServerCog } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { EmptyState, ErrorState, LoadingState } from '../components/AsyncState'
import { Modal } from '../components/Modal'
import { PageHeader } from '../components/PageHeader'
import { StatusBadge } from '../components/StatusBadge'
import { useProjects } from '../contexts/ProjectState'
import { useApi } from '../contexts/SessionState'
import { useAsync } from '../hooks/useAsync'
import { formatDateTime, formatRelative } from '../lib/format'

export function AgentsPage() {
  const api = useApi()
  const { selected } = useProjects()
  const state = useAsync(async () => {
    const [agents, pipelines] = await Promise.all([api.listAgents(selected?.id), api.listPipelines(selected?.id)])
    return { agents, pipelines }
  }, [api, selected?.id])
  const [busy, setBusy] = useState<string | null>(null)
  const [mutationError, setMutationError] = useState<Error | null>(null)
	const [agentOpen, setAgentOpen] = useState(false)
	const [pipelineOpen, setPipelineOpen] = useState(false)

  async function setPipeline(projectId: string, pipelineId: string, action: 'enable' | 'pause' | 'disable') {
    if (action === 'disable' && !window.confirm('禁用后需要管理员显式恢复。继续吗？')) return
    setBusy(pipelineId)
    setMutationError(null)
    try { await api.setPipelineStatus(projectId, pipelineId, action); state.reload() }
    catch (cause) { setMutationError(cause instanceof Error ? cause : new Error('操作失败。')) }
    finally { setBusy(null) }
  }

  return <div className="page-stack">
    <PageHeader title="Agent 与 Pipeline" description="对比 Server 期望状态和 Agent 最近一次报告状态。" actions={<><button type="button" className="button secondary" disabled={!selected} onClick={() => setAgentOpen(true)}><Plus size={16} />注册 Agent</button><button type="button" className="button secondary" disabled={!selected || !state.data?.agents.length} onClick={() => setPipelineOpen(true)}><Plus size={16} />创建 Pipeline</button><button type="button" className="button secondary" onClick={state.reload}><RefreshCw size={16} />刷新状态</button></>} />
    {mutationError && <ErrorState error={mutationError} />}
    {state.loading && !state.data ? <LoadingState label="正在读取心跳状态" /> : state.error ? <ErrorState error={state.error} retry={state.reload} /> : !state.data?.agents.length ? <EmptyState title="没有注册 Agent" description="为当前 Project 创建注册凭证并启动 Agent 后，状态会显示在这里。" /> : <div className="agent-stack">
      {state.data.agents.map((agent) => {
        const pipelines = state.data!.pipelines.filter((pipeline) => pipeline.agent_id === agent.id)
        return <section className="content-section agent-section" key={agent.id}>
          <header className="agent-header"><span className={`agent-icon ${agent.status}`}><ServerCog /></span><div className="agent-identity"><div><h2>{agent.name}</h2><StatusBadge status={agent.status} /></div><p>{agent.hostname} · Agent v{agent.version} · {agent.last_seen_ip || 'IP 未报告'}</p></div><dl className="agent-heartbeat"><div><dt>最后心跳</dt><dd>{formatRelative(agent.last_heartbeat_at)}</dd></div><div><dt>具体时间</dt><dd>{formatDateTime(agent.last_heartbeat_at)}</dd></div></dl></header>
          {pipelines.length === 0 ? <p className="empty-inline">该 Agent 尚未报告 Pipeline。</p> : <div className="pipeline-table"><div className="pipeline-table-head"><span>Pipeline</span><span>期望状态</span><span>报告状态</span><span>配置</span><span>最近报告</span><span><span className="sr-only">操作</span></span></div>{pipelines.map((pipeline) => <div className="pipeline-row" key={pipeline.id}><div><strong>{pipeline.name}</strong><small>{pipeline.service}</small>{pipeline.last_error && <p className="row-error">{pipeline.last_error}</p>}</div><StatusBadge status={pipeline.status} /><StatusBadge status={pipeline.reported_status} /><span>v{pipeline.config_version}</span><span>{formatRelative(pipeline.reported_at)}</span><div className="row-actions">{pipeline.status !== 'enabled' && <button type="button" className="icon-button" title="启用 Pipeline" disabled={busy === pipeline.id} onClick={() => setPipeline(pipeline.project_id, pipeline.id, 'enable')}><CirclePlay size={17} /></button>}{pipeline.status === 'enabled' && <button type="button" className="icon-button" title="暂停 Pipeline" disabled={busy === pipeline.id} onClick={() => setPipeline(pipeline.project_id, pipeline.id, 'pause')}><CirclePause size={17} /></button>}{pipeline.status !== 'disabled' && <button type="button" className="icon-button danger" title="禁用 Pipeline" disabled={busy === pipeline.id} onClick={() => setPipeline(pipeline.project_id, pipeline.id, 'disable')}><OctagonX size={17} /></button>}</div></div>)}</div>}
        </section>
      })}
    </div>}
	{selected && <RegisterAgentModal open={agentOpen} projectId={selected.id} onClose={() => setAgentOpen(false)} onCreated={() => { setAgentOpen(false); state.reload() }} />}
	{selected && <CreatePipelineModal open={pipelineOpen} projectId={selected.id} agents={state.data?.agents ?? []} onClose={() => setPipelineOpen(false)} onCreated={() => { setPipelineOpen(false); state.reload() }} />}
  </div>
}

function RegisterAgentModal({ open, projectId, onClose, onCreated }: { open: boolean; projectId: string; onClose(): void; onCreated(): void }) {
	const api = useApi()
	const [name, setName] = useState('')
	const [hostname, setHostname] = useState('')
	const [version, setVersion] = useState('dev')
	const [error, setError] = useState<Error | null>(null)
	const [busy, setBusy] = useState(false)
	async function submit(event: FormEvent) {
		event.preventDefault()
		setBusy(true)
		setError(null)
		try {
			await api.createAgent(projectId, { name: name.trim(), hostname: hostname.trim(), version: version.trim() })
			onCreated()
		} catch (cause) {
			setError(cause instanceof Error ? cause : new Error('注册失败。'))
		} finally {
			setBusy(false)
		}
	}
	return <Modal open={open} title="注册 Agent" description="名称在 Project 内唯一；同名同主机的重试是幂等的。" onClose={onClose}><form className="modal-body form-stack" onSubmit={submit}>{error && <ErrorState error={error} />}<label className="field"><span>Agent 名称</span><input value={name} onChange={(event) => setName(event.target.value)} placeholder="checkout-prod-01" required autoFocus /></label><label className="field"><span>Hostname</span><input value={hostname} onChange={(event) => setHostname(event.target.value)} placeholder="node-a" required /></label><label className="field"><span>版本</span><input value={version} onChange={(event) => setVersion(event.target.value)} placeholder="0.1.0" /></label><footer className="modal-actions"><button type="button" className="button secondary" onClick={onClose}>取消</button><button type="submit" className="button primary" disabled={busy}>{busy ? '正在注册…' : '注册 Agent'}</button></footer></form></Modal>
}

function CreatePipelineModal({ open, projectId, agents, onClose, onCreated }: { open: boolean; projectId: string; agents: { id: string; name: string }[]; onClose(): void; onCreated(): void }) {
	const api = useApi()
	const [agentId, setAgentId] = useState('')
	const [name, setName] = useState('')
	const [service, setService] = useState('')
	const [config, setConfig] = useState('{\n  "source": { "type": "file" }\n}')
	const [error, setError] = useState<Error | null>(null)
	const [busy, setBusy] = useState(false)
	async function submit(event: FormEvent) {
		event.preventDefault()
		setBusy(true)
		setError(null)
		try {
			const parsed = JSON.parse(config) as unknown
			if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('Pipeline 配置必须是 JSON object。')
			await api.createPipeline(projectId, { agent_id: agentId || agents[0]?.id, name: name.trim(), service: service.trim(), config: parsed as Record<string, unknown> })
			onCreated()
		} catch (cause) {
			setError(cause instanceof Error ? cause : new Error('创建失败。'))
		} finally {
			setBusy(false)
		}
	}
	return <Modal open={open} title="创建 Pipeline" description="Pipeline 把一个 Agent 上的采集配置绑定到稳定 Service。" onClose={onClose}><form className="modal-body form-stack" onSubmit={submit}>{error && <ErrorState error={error} />}<label className="field"><span>Agent</span><select value={agentId || agents[0]?.id || ''} onChange={(event) => setAgentId(event.target.value)} required>{agents.map((agent) => <option key={agent.id} value={agent.id}>{agent.name}</option>)}</select></label><label className="field"><span>Pipeline 名称</span><input value={name} onChange={(event) => setName(event.target.value)} placeholder="application-json" required autoFocus /></label><label className="field"><span>Service</span><input value={service} onChange={(event) => setService(event.target.value)} placeholder="checkout-api" required /></label><label className="field"><span>配置 JSON</span><textarea rows={7} value={config} onChange={(event) => setConfig(event.target.value)} spellCheck={false} required /></label><footer className="modal-actions"><button type="button" className="button secondary" onClick={onClose}>取消</button><button type="submit" className="button primary" disabled={busy || agents.length === 0}>{busy ? '正在创建…' : '创建 Pipeline'}</button></footer></form></Modal>
}
