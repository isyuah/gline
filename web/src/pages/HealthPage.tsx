import { Activity, Clock3, Database, RefreshCw, Server, Workflow } from 'lucide-react'
import { ErrorState, LoadingState } from '../components/AsyncState'
import { PageHeader } from '../components/PageHeader'
import { StatusBadge } from '../components/StatusBadge'
import { useApi } from '../contexts/SessionState'
import { useAsync } from '../hooks/useAsync'
import { formatDateTime } from '../lib/format'

const checkIcons = { database: Database, migrations: Workflow, background_workers: Activity }

export function HealthPage() {
  const api = useApi()
  const state = useAsync(() => api.health(), [api])
  return <div className="page-stack"><PageHeader title="系统健康" description="Readiness 表示实例当前是否适合承接流量，不等同于进程仍然存活。" actions={<button type="button" className="button secondary" disabled={state.loading} onClick={state.reload}><RefreshCw size={16} />立即检查</button>} />{state.loading && !state.data ? <LoadingState label="正在检查 Server readiness" /> : state.error ? <ErrorState error={state.error} retry={state.reload} /> : state.data && <><section className={`health-banner ${state.data.status}`}><span><Server /></span><div><div><h2>Gline Server</h2><StatusBadge status={state.data.status} /></div><p>{state.data.status === 'healthy' ? '实例已准备好承接控制、接入和查询请求。' : '实例当前不应承接新流量，请检查依赖项。'}</p></div><dl><div><dt>版本</dt><dd>{state.data.version ?? '未报告'}</dd></div><div><dt>检查时间</dt><dd>{formatDateTime(state.data.observed_at)}</dd></div></dl></section><section className="health-check-grid">{Object.entries(state.data.checks ?? {}).map(([name, check]) => { const Icon = checkIcons[name as keyof typeof checkIcons] ?? Activity; return <article className="health-check" key={name}><header><span><Icon /></span><StatusBadge status={check.status} /></header><strong>{name.replaceAll('_', ' ')}</strong><p>{check.message ?? '检查通过，没有附加诊断。'}</p>{check.latency_ms !== undefined && <small><Clock3 size={14} />{check.latency_ms} ms</small>}</article> })}</section></>}</div>
}
