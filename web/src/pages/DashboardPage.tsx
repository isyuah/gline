import { Activity, AlertTriangle, ArrowRight, Database, FileSearch, Radio, ServerCog } from 'lucide-react'
import { Link } from 'react-router-dom'
import { ErrorState, LoadingState } from '../components/AsyncState'
import { PageHeader } from '../components/PageHeader'
import { StatusBadge } from '../components/StatusBadge'
import { useProjects } from '../contexts/ProjectState'
import { useApi } from '../contexts/SessionState'
import { useAsync } from '../hooks/useAsync'
import { formatBytes, formatRelative } from '../lib/format'

export function DashboardPage() {
  const api = useApi()
  const { projects, selected, error: projectError, reload: reloadProjects } = useProjects()
  const state = useAsync(async () => {
    const [agents, pipelines, usage, health] = await Promise.all([
      api.listAgents(selected?.id),
      api.listPipelines(selected?.id),
      selected ? api.getUsage(selected.id, new Date(Date.now() - 86_400_000).toISOString(), new Date().toISOString()) : Promise.resolve([]),
      api.health(),
    ])
    return { agents, pipelines, usage, health }
  }, [api, selected?.id])

  if (projectError) return <ErrorState error={projectError} retry={reloadProjects} />
  if (state.loading && !state.data) return <LoadingState label="正在汇总运行状态" />
  if (state.error) return <ErrorState error={state.error} retry={state.reload} />

  const data = state.data!
  const activeAgents = data.agents.filter((agent) => agent.status === 'active').length
  const unhealthyPipelines = data.pipelines.filter((pipeline) => pipeline.reported_status === 'error').length
  const entries = data.usage.reduce((sum, bucket) => sum + bucket.entries, 0)
  const bytes = data.usage.reduce((sum, bucket) => sum + bucket.bytes, 0)

  return <div className="page-stack">
    <PageHeader title="运行概览" description={selected ? `${selected.name} 的接入、查询与存储状态。` : '所有 Project 的整体运行状态。'} actions={<Link className="button secondary" to="/logs"><FileSearch size={16} />搜索日志</Link>} />
    <section className="metric-grid" aria-label="关键指标">
      <article className="metric"><div className="metric-icon teal"><Activity /></div><div><span>在线 Agent</span><strong>{activeAgents}<small> / {data.agents.length}</small></strong><p>{data.agents.length - activeAgents === 0 ? '全部 Agent 正常心跳' : `${data.agents.length - activeAgents} 个需要检查`}</p></div></article>
      <article className="metric"><div className="metric-icon red"><AlertTriangle /></div><div><span>异常 Pipeline</span><strong>{unhealthyPipelines}</strong><p>{unhealthyPipelines ? '存在采集错误或状态不一致' : '没有活跃异常'}</p></div></article>
      <article className="metric"><div className="metric-icon blue"><Radio /></div><div><span>近 24 小时日志</span><strong>{entries.toLocaleString()}</strong><p>{formatBytes(bytes)} 已接入</p></div></article>
      <article className="metric"><div className="metric-icon gray"><Database /></div><div><span>Project</span><strong>{projects.filter((project) => project.status === 'active').length}<small> / {projects.length}</small></strong><p>{projects.filter((project) => project.status === 'disabled').length} 个已停用</p></div></article>
    </section>

    <div className="dashboard-columns">
      <section className="content-section">
        <header className="section-header"><div><h2>采集状态</h2><p>最近心跳与管道报告状态</p></div><Link to="/agents" className="text-link">查看全部<ArrowRight size={15} /></Link></header>
        <div className="activity-list">
          {data.agents.slice(0, 5).map((agent) => {
            const agentPipelines = data.pipelines.filter((pipeline) => pipeline.agent_id === agent.id)
            return <div className="activity-row" key={agent.id}><span className={`activity-indicator ${agent.status}`}><ServerCog size={17} /></span><div className="activity-main"><strong>{agent.name}</strong><span>{agent.hostname} · v{agent.version}</span></div><div className="activity-meta"><StatusBadge status={agent.status} /><small>{formatRelative(agent.last_heartbeat_at)}</small></div><div className="pipeline-mini">{agentPipelines.length ? agentPipelines.map((pipeline) => <span key={pipeline.id} title={`${pipeline.service}: ${pipeline.reported_status}`} className={pipeline.reported_status} />) : <small>无 Pipeline</small>}</div></div>
          })}
          {data.agents.length === 0 && <p className="empty-inline">这个 Project 还没有注册 Agent。</p>}
        </div>
      </section>

      <section className="content-section system-summary">
        <header className="section-header"><div><h2>系统状态</h2><p>Server readiness 汇总</p></div><StatusBadge status={data.health.status} /></header>
        <dl className="summary-list"><div><dt>API 版本</dt><dd>{data.health.version ?? '未报告'}</dd></div><div><dt>数据库</dt><dd>{data.health.checks?.database?.status ?? '未报告'}</dd></div><div><dt>迁移状态</dt><dd>{data.health.checks?.migrations?.status ?? '未报告'}</dd></div><div><dt>观测时间</dt><dd>{formatRelative(data.health.observed_at)}</dd></div></dl>
        <Link to="/health" className="button subtle wide">查看健康详情<ArrowRight size={16} /></Link>
      </section>
    </div>
  </div>
}
